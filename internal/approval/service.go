// Package approval — Phase 5 admin access approval workflow.
//
// Sensitive admin actions (`user_delete`, `domain_remove`,
// `data_export`, `plan_downgrade`, `retention_policy_change`) can
// be gated by a per-tenant approval policy. Callers ask
// `RequiresApproval(action)` before executing; if true, they call
// `CreateRequest` to enqueue a pending approval. The approver
// later calls `ApproveRequest` (and optionally `ExecuteApproved` if
// the executor is wired) to commit the action.
//
// `ExecuteApproved` is intentionally pluggable: callers register
// an `Executor` per action so the approval package does not pull
// every executing service (`tenant`, `billing`, `retention`) as a
// dependency.
package approval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Status values mirror the SQL CHECK constraint.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusExpired  Status = "expired"
)

// Request is the public approval request shape.
type Request struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	RequesterID    string    `json:"requester_id"`
	Action         string    `json:"action"`
	TargetResource string    `json:"target_resource"`
	Status         Status    `json:"status"`
	ApproverID     string    `json:"approver_id,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Executor is the per-action callback that commits the approved
// action. Wired in main.go so the approval package stays
// independent of the executing services.
type Executor func(ctx context.Context, req Request) error

// Service implements the approval API.
type Service struct {
	pool      *pgxpool.Pool
	executors map[string]Executor
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, executors: map[string]Executor{}}
}

// RegisterExecutor binds an executor to an action.
func (s *Service) RegisterExecutor(action string, exec Executor) {
	s.executors[action] = exec
}

// RequiresApproval returns true iff the per-tenant `approval_config`
// has the action gated.
func (s *Service) RequiresApproval(ctx context.Context, tenantID, action string) (bool, error) {
	if s.pool == nil {
		return false, nil
	}
	var requires bool
	err := s.pool.QueryRow(ctx, `
		SELECT requires_approval FROM approval_config
		WHERE tenant_id = $1::uuid AND action = $2
	`, tenantID, action).Scan(&requires)
	if err != nil {
		return false, nil
	}
	return requires, nil
}

// CreateRequest enqueues a new pending approval.
func (s *Service) CreateRequest(ctx context.Context, tenantID, requesterID, action, target string) (*Request, error) {
	if tenantID == "" || requesterID == "" || action == "" {
		return nil, errors.New("approval: tenant, requester, action required")
	}
	if s.pool == nil {
		return nil, errors.New("approval: pool not configured")
	}
	var r Request
	err := s.pool.QueryRow(ctx, `
		INSERT INTO approval_requests (tenant_id, requester_id, action, target_resource, status)
		VALUES ($1::uuid, $2, $3, $4, 'pending')
		RETURNING id::text, tenant_id::text, requester_id, action, target_resource, status,
		          COALESCE(approver_id, ''), reason, created_at, resolved_at, expires_at
	`, tenantID, requesterID, action, target).Scan(
		&r.ID, &r.TenantID, &r.RequesterID, &r.Action, &r.TargetResource, &r.Status,
		&r.ApproverID, &r.Reason, &r.CreatedAt, &r.ResolvedAt, &r.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ApproveRequest marks the request approved. The tenantID is
// taken from the authenticated request path and is enforced by
// both an explicit `AND tenant_id = ...` clause and a Postgres
// session GUC that engages RLS, so a caller cannot resolve another
// tenant's pending approval by guessing the UUID.
func (s *Service) ApproveRequest(ctx context.Context, tenantID, approvalID, approverID string) (*Request, error) {
	return s.resolve(ctx, tenantID, approvalID, approverID, StatusApproved)
}

// RejectRequest marks the request rejected. Same tenant scoping as
// ApproveRequest applies.
func (s *Service) RejectRequest(ctx context.Context, tenantID, approvalID, approverID, reason string) (*Request, error) {
	r, err := s.resolve(ctx, tenantID, approvalID, approverID, StatusRejected)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE approval_requests SET reason = $2
				WHERE id = $1::uuid AND tenant_id = $3::uuid
			`, approvalID, reason, tenantID)
			return err
		})
		if err == nil {
			r.Reason = reason
		}
	}
	return r, nil
}

func (s *Service) resolve(ctx context.Context, tenantID, approvalID, approverID string, status Status) (*Request, error) {
	if tenantID == "" {
		return nil, errors.New("approval: tenantID required")
	}
	if s.pool == nil {
		return nil, errors.New("approval: pool not configured")
	}
	var r Request
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			UPDATE approval_requests
			SET status = $2, approver_id = $3, resolved_at = now()
			WHERE id = $1::uuid AND tenant_id = $4::uuid AND status = 'pending'
			RETURNING id::text, tenant_id::text, requester_id, action, target_resource, status,
			          COALESCE(approver_id, ''), reason, created_at, resolved_at, expires_at
		`, approvalID, string(status), approverID, tenantID).Scan(
			&r.ID, &r.TenantID, &r.RequesterID, &r.Action, &r.TargetResource, &r.Status,
			&r.ApproverID, &r.Reason, &r.CreatedAt, &r.ResolvedAt, &r.ExpiresAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	return &r, nil
}

// ListPendingRequests lists pending approvals for a tenant.
func (s *Service) ListPendingRequests(ctx context.Context, tenantID string) ([]Request, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, tenant_id::text, requester_id, action, target_resource, status,
		       COALESCE(approver_id, ''), reason, created_at, resolved_at, expires_at
		FROM approval_requests
		WHERE tenant_id = $1::uuid AND status = 'pending' AND expires_at > now()
		ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequests(rows)
}

// ListAll returns recent approvals for the tenant (any status).
func (s *Service) ListAll(ctx context.Context, tenantID string, limit int) ([]Request, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, tenant_id::text, requester_id, action, target_resource, status,
		       COALESCE(approver_id, ''), reason, created_at, resolved_at, expires_at
		FROM approval_requests
		WHERE tenant_id = $1::uuid
		ORDER BY created_at DESC
		LIMIT $2
	`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequests(rows)
}

// ExecuteApproved runs the registered executor for the approved
// request. Returns ErrNoExecutor when no executor is wired so the
// caller can fall back to manual operator action. Like the other
// resolve methods, the lookup is tenant-scoped (explicit predicate
// + RLS) so a caller cannot execute another tenant's approval.
func (s *Service) ExecuteApproved(ctx context.Context, tenantID, approvalID string) error {
	if tenantID == "" {
		return errors.New("approval: tenantID required")
	}
	if s.pool == nil {
		return errors.New("approval: pool not configured")
	}
	var r Request
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, requester_id, action, target_resource, status,
			       COALESCE(approver_id, ''), reason, created_at, resolved_at, expires_at
			FROM approval_requests
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, approvalID, tenantID).Scan(
			&r.ID, &r.TenantID, &r.RequesterID, &r.Action, &r.TargetResource, &r.Status,
			&r.ApproverID, &r.Reason, &r.CreatedAt, &r.ResolvedAt, &r.ExpiresAt,
		)
	})
	if err != nil {
		return err
	}
	if r.Status != StatusApproved {
		return fmt.Errorf("approval: request not approved (status=%s)", r.Status)
	}
	exec, ok := s.executors[r.Action]
	if !ok {
		return ErrNoExecutor
	}
	return exec(ctx, r)
}

// ErrNoExecutor is returned when ExecuteApproved cannot find a
// registered handler for the action.
var ErrNoExecutor = errors.New("approval: no executor registered for action")

// SetActionConfig enables/disables approval gating for a tenant +
// action.
func (s *Service) SetActionConfig(ctx context.Context, tenantID, action string, requires bool) error {
	if s.pool == nil {
		return errors.New("approval: pool not configured")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO approval_config (tenant_id, action, requires_approval)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (tenant_id, action) DO UPDATE
		SET requires_approval = EXCLUDED.requires_approval
	`, tenantID, action, requires)
	return err
}

// ListActionConfig returns the per-tenant gating map.
func (s *Service) ListActionConfig(ctx context.Context, tenantID string) (map[string]bool, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT action, requires_approval FROM approval_config
		WHERE tenant_id = $1::uuid
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		var b bool
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out[a] = b
	}
	return out, rows.Err()
}

func scanRequests(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Request, error) {
	var out []Request
	for rows.Next() {
		var r Request
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.RequesterID, &r.Action, &r.TargetResource, &r.Status,
			&r.ApproverID, &r.Reason, &r.CreatedAt, &r.ResolvedAt, &r.ExpiresAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
