// Package adminproxy implements the reverse access proxy that
// gates KMail support / SRE access to tenant mailbox data behind
// the Phase 5 approval workflow.
//
// Why: even with RLS, KMail operators need *some* access path to
// help tenants debug "where is my mail?" tickets. Phase 5
// formalises that path so every byte of tenant data an admin
// touches is preceded by an explicit approval and recorded in the
// audit chain.
//
// Lifecycle:
//
//  1. Admin calls `POST /api/v1/admin/proxy/{tenantId}/access`
//     with a reason and scope. The service creates an
//     `approval_requests` row via the existing approval workflow.
//  2. The tenant's authorised approver clicks Approve in the
//     existing Approvals admin UI; the approval row flips to
//     `approved`.
//  3. `ProxyRequest` opens (or re-uses) an
//     `admin_access_sessions` row keyed on the approval, then
//     forwards the request to the tenant's Stalwart shard. Every
//     forward writes an audit entry.
package adminproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/approval"
	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/tenant"
)

// ApprovalAction is the action name used by the underlying
// approval workflow for admin proxy requests.
const ApprovalAction = "admin_proxy_access"

// SessionDuration is how long an approved admin proxy session stays
// open before the proxy refuses to forward more traffic.
const SessionDuration = 4 * time.Hour

// ErrNotApproved is returned when a proxy attempt references an
// approval that is not in `approved` status.
var ErrNotApproved = errors.New("adminproxy: access not approved")

// ErrSessionExpired is returned when the approved session window
// has elapsed.
var ErrSessionExpired = errors.New("adminproxy: session expired")

// ErrSessionRevoked is returned when an admin's session has been
// revoked (e.g. by the tenant pulling the plug mid-session).
var ErrSessionRevoked = errors.New("adminproxy: session revoked")

// AccessSession is the public view of an `admin_access_sessions`
// row.
type AccessSession struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id"`
	ApprovalRequestID string     `json:"approval_request_id"`
	AdminUserID       string     `json:"admin_user_id"`
	Scope             string     `json:"scope"`
	StartedAt         time.Time  `json:"started_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

// AdminProxyService is the admin proxy.
type AdminProxyService struct {
	pool        *pgxpool.Pool
	approvalSvc *approval.Service
	auditSvc    *audit.Service
	shards      *tenant.ShardService
}

// NewService returns an AdminProxyService.
func NewService(pool *pgxpool.Pool, approvalSvc *approval.Service, auditSvc *audit.Service, shards *tenant.ShardService) *AdminProxyService {
	return &AdminProxyService{pool: pool, approvalSvc: approvalSvc, auditSvc: auditSvc, shards: shards}
}

// RequestAccess creates an `approval_requests` row tagged with
// the proxy action and returns the approval ID. The admin caller
// then waits for the tenant to approve through the existing
// Approvals admin UI.
func (s *AdminProxyService) RequestAccess(ctx context.Context, tenantID, adminUserID, reason, scope string) (*approval.Request, error) {
	if tenantID == "" || adminUserID == "" {
		return nil, errors.New("adminproxy: tenant + admin required")
	}
	if scope == "" {
		scope = "mailbox"
	}
	target := scope + ":" + reason
	req, err := s.approvalSvc.CreateRequest(ctx, tenantID, adminUserID, ApprovalAction, target)
	if err != nil {
		return nil, err
	}
	if s.auditSvc != nil {
		_, _ = s.auditSvc.Log(ctx, audit.Entry{
			TenantID:     tenantID,
			ActorID:      adminUserID,
			ActorType:    audit.ActorAdmin,
			Action:       "admin_proxy_request",
			ResourceType: "approval_request",
			ResourceID:   req.ID,
			Metadata: map[string]any{
				"reason": reason,
				"scope":  scope,
			},
		})
	}
	return req, nil
}

// EnsureSession verifies the approval is still valid and (if the
// session row does not yet exist) opens one. Returns the active
// session.
func (s *AdminProxyService) EnsureSession(ctx context.Context, tenantID, approvalID, adminUserID string) (*AccessSession, error) {
	if s.pool == nil {
		return nil, errors.New("adminproxy: pool not configured")
	}
	if tenantID == "" || approvalID == "" {
		return nil, errors.New("adminproxy: tenant + approval required")
	}
	var sess AccessSession
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
			status         string
			scope          string
			requesterID    string
			approvalTenant string
		)
		err := tx.QueryRow(ctx, `
			SELECT status, target_resource, requester_id, tenant_id::text
			FROM approval_requests
			WHERE id = $1::uuid AND tenant_id = $2::uuid AND action = $3
		`, approvalID, tenantID, ApprovalAction).Scan(&status, &scope, &requesterID, &approvalTenant)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotApproved
		}
		if err != nil {
			return err
		}
		if status != string(approval.StatusApproved) {
			return ErrNotApproved
		}
		// Try to load an existing session; INSERT on miss.
		err = tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, approval_request_id::text, admin_user_id, scope,
			       started_at, expires_at, revoked_at
			FROM admin_access_sessions
			WHERE approval_request_id = $1::uuid AND tenant_id = $2::uuid
		`, approvalID, tenantID).Scan(
			&sess.ID, &sess.TenantID, &sess.ApprovalRequestID, &sess.AdminUserID,
			&sess.Scope, &sess.StartedAt, &sess.ExpiresAt, &sess.RevokedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			scopeShort := scope
			if scopeShort == "" {
				scopeShort = "mailbox"
			}
			expiresAt := time.Now().Add(SessionDuration)
			return tx.QueryRow(ctx, `
				INSERT INTO admin_access_sessions
					(tenant_id, approval_request_id, admin_user_id, scope, expires_at)
				VALUES ($1::uuid, $2::uuid, $3, $4, $5)
				RETURNING id::text, tenant_id::text, approval_request_id::text, admin_user_id, scope,
				          started_at, expires_at, revoked_at
			`, tenantID, approvalID, adminUserID, scopeShort, expiresAt).Scan(
				&sess.ID, &sess.TenantID, &sess.ApprovalRequestID, &sess.AdminUserID,
				&sess.Scope, &sess.StartedAt, &sess.ExpiresAt, &sess.RevokedAt,
			)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if sess.RevokedAt != nil {
		return nil, ErrSessionRevoked
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, ErrSessionExpired
	}
	return &sess, nil
}

// RevokeSession terminates an active proxy session. Future calls
// to EnsureSession on the same approval will fail with
// ErrSessionRevoked.
func (s *AdminProxyService) RevokeSession(ctx context.Context, tenantID, sessionID string) error {
	if s.pool == nil {
		return errors.New("adminproxy: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE admin_access_sessions SET revoked_at = now()
			WHERE id = $1::uuid AND tenant_id = $2::uuid AND revoked_at IS NULL
		`, sessionID, tenantID)
		return err
	})
}

// ListSessions returns all sessions for a tenant (most recent first).
func (s *AdminProxyService) ListSessions(ctx context.Context, tenantID string) ([]AccessSession, error) {
	if s.pool == nil || tenantID == "" {
		return nil, nil
	}
	var out []AccessSession
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, approval_request_id::text, admin_user_id, scope,
			       started_at, expires_at, revoked_at
			FROM admin_access_sessions
			WHERE tenant_id = $1::uuid
			ORDER BY started_at DESC
			LIMIT 100
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s AccessSession
			if err := rows.Scan(
				&s.ID, &s.TenantID, &s.ApprovalRequestID, &s.AdminUserID, &s.Scope,
				&s.StartedAt, &s.ExpiresAt, &s.RevokedAt,
			); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// ResolveShard returns the Stalwart URL for the tenant. Falls
// back to a default URL when the shard service is unavailable
// (dev / unit tests).
func (s *AdminProxyService) ResolveShard(ctx context.Context, tenantID, fallback string) (string, error) {
	if s.shards == nil {
		return fallback, nil
	}
	url, err := s.shards.GetTenantShard(ctx, tenantID)
	if err != nil || url == "" {
		return fallback, nil
	}
	return url, nil
}

// LogProxyAccess emits an audit entry for every forwarded request.
func (s *AdminProxyService) LogProxyAccess(ctx context.Context, tenantID, adminUserID string, r *http.Request, sessionID, scope string, status int) {
	if s.auditSvc == nil {
		return
	}
	_, _ = s.auditSvc.Log(ctx, audit.Entry{
		TenantID:     tenantID,
		ActorID:      adminUserID,
		ActorType:    audit.ActorAdmin,
		Action:       "admin_proxy_forward",
		ResourceType: "stalwart",
		ResourceID:   sessionID,
		Metadata: map[string]any{
			"path":   r.URL.Path,
			"method": r.Method,
			"scope":  scope,
			"status": status,
		},
		IPAddress: r.RemoteAddr,
		UserAgent: r.UserAgent(),
	})
}

// String prints a debug-friendly representation of a session.
func (s *AccessSession) String() string {
	return fmt.Sprintf("session %s tenant=%s admin=%s scope=%s exp=%s", s.ID, s.TenantID, s.AdminUserID, s.Scope, s.ExpiresAt.Format(time.RFC3339))
}
