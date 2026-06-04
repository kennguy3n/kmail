// Package retention — Phase 5 retention / archive lifecycle.
//
// Tenant admins declare retention policies that auto-archive or
// auto-delete email older than N days. The Phase 5 implementation
// is intentionally narrow:
//
//   * Policy CRUD against `retention_policies` (admin UI surface).
//   * `EvaluateRetention` is a no-op stub that walks the policies
//     list and emits an audit event recording how many policies
//     would have run; the actual JMAP-side `Email/set destroy` plus
//     the zk-object-fabric placement-update for the archive tier
//     lands as a Phase 5 follow-up once the retention worker has
//     been validated against staging traffic.
//
// The retention worker (worker.go) ticks daily and calls
// `EvaluateRetention` for every active tenant.
package retention

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// EnforcementRun is one row of `retention_enforcement_log`
// projected for the admin status card.
type EnforcementRun struct {
	ID              string     `json:"id"`
	PolicyID        string     `json:"policy_id"`
	EmailsProcessed int        `json:"emails_processed"`
	EmailsDeleted   int        `json:"emails_deleted"`
	EmailsArchived  int        `json:"emails_archived"`
	StartedAt       time.Time  `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	Error           string     `json:"error,omitempty"`
	Notes           string     `json:"notes,omitempty"`
}

// Policy is the public shape of a retention policy.
type Policy struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	PolicyType    string    `json:"policy_type"` // "archive" | "delete"
	RetentionDays int       `json:"retention_days"`
	AppliesTo     string    `json:"applies_to"` // "all" | "mailbox" | "label"
	TargetRef     string    `json:"target_ref,omitempty"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Service manages retention policies and drives enforcement
// through an optional Enforcer.
type Service struct {
	pool *pgxpool.Pool
	// enforcer is registered by the worker goroutine (engineFor)
	// while EvaluateRetention may read it from another goroutine, so
	// it is stored atomically to keep the two paths race-free.
	enforcer atomic.Pointer[Enforcer]
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// WithEnforcer wires the enforcement engine used by
// EvaluateRetention. The retention worker builds the Enforcer once
// its options are known and registers it here so both the worker
// loop and any direct EvaluateRetention caller share one engine.
func (s *Service) WithEnforcer(e *Enforcer) *Service {
	s.enforcer.Store(e)
	return s
}

// CreatePolicy inserts a new policy.
func (s *Service) CreatePolicy(ctx context.Context, p Policy) (*Policy, error) {
	if err := validatePolicy(p); err != nil {
		return nil, err
	}
	if s.pool == nil {
		return nil, errors.New("retention: pool not configured")
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO retention_policies (
			tenant_id, policy_type, retention_days, applies_to, target_ref, enabled
		) VALUES ($1::uuid, $2, $3, $4, $5, $6)
		RETURNING id::text, created_at, updated_at
	`, p.TenantID, p.PolicyType, p.RetentionDays, p.AppliesTo, p.TargetRef, p.Enabled)
	if err := row.Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, fmt.Errorf("create retention: %w", err)
	}
	return &p, nil
}

// UpdatePolicy persists changes.
func (s *Service) UpdatePolicy(ctx context.Context, p Policy) (*Policy, error) {
	if err := validatePolicy(p); err != nil {
		return nil, err
	}
	if s.pool == nil {
		return nil, errors.New("retention: pool not configured")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE retention_policies
		SET policy_type = $2, retention_days = $3, applies_to = $4, target_ref = $5, enabled = $6
		WHERE id = $1::uuid AND tenant_id = $7::uuid
	`, p.ID, p.PolicyType, p.RetentionDays, p.AppliesTo, p.TargetRef, p.Enabled, p.TenantID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePolicy removes the policy.
func (s *Service) DeletePolicy(ctx context.Context, tenantID, id string) error {
	if s.pool == nil {
		return errors.New("retention: pool not configured")
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM retention_policies WHERE id = $1::uuid AND tenant_id = $2::uuid
	`, id, tenantID)
	return err
}

// ListPolicies returns the policies for a tenant.
func (s *Service) ListPolicies(ctx context.Context, tenantID string) ([]Policy, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, tenant_id::text, policy_type, retention_days, applies_to,
		       target_ref, enabled, created_at, updated_at
		FROM retention_policies WHERE tenant_id = $1::uuid
		ORDER BY created_at ASC
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		if err := rows.Scan(&p.ID, &p.TenantID, &p.PolicyType, &p.RetentionDays, &p.AppliesTo, &p.TargetRef, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EvaluateRetention enforces every enabled policy for a tenant via
// the configured Enforcer and returns the number of policies that
// completed without error. Per-policy failures are collected and
// returned joined so one bad policy does not abort the rest.
//
// When no Enforcer is configured it degrades to counting enabled
// policies (a no-op evaluation) so callers wired before the worker
// builds its engine still get a sane answer instead of a panic.
func (s *Service) EvaluateRetention(ctx context.Context, tenantID string) (int, error) {
	policies, err := s.ListPolicies(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	enforcer := s.enforcer.Load()
	if enforcer == nil {
		enabled := 0
		for _, p := range policies {
			if p.Enabled {
				enabled++
			}
		}
		return enabled, nil
	}
	enforced := 0
	var errs []error
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		if _, err := enforcer.EnforcePolicy(ctx, tenantID, p); err != nil {
			errs = append(errs, fmt.Errorf("policy %s: %w", p.ID, err))
			continue
		}
		enforced++
	}
	return enforced, errors.Join(errs...)
}

// ListActiveTenants returns the active tenants the worker should
// evaluate. The worker uses this rather than `tenant.Service.List`
// to avoid pulling the full tenant package as a dependency.
func (s *Service) ListActiveTenants(ctx context.Context) ([]string, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM tenants WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func validatePolicy(p Policy) error {
	if p.TenantID == "" {
		return errors.New("retention: tenant_id required")
	}
	if p.PolicyType != "archive" && p.PolicyType != "delete" {
		return errors.New("retention: policy_type must be archive|delete")
	}
	if p.RetentionDays <= 0 {
		return errors.New("retention: retention_days must be > 0")
	}
	switch p.AppliesTo {
	case "all", "mailbox", "label":
	default:
		return errors.New("retention: applies_to must be all|mailbox|label")
	}
	return nil
}

// RecentEnforcementRuns returns the most recent enforcement log
// rows for a tenant, ordered newest-first.
func (s *Service) RecentEnforcementRuns(ctx context.Context, tenantID string, limit int) ([]EnforcementRun, error) {
	if s.pool == nil || tenantID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var out []EnforcementRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, policy_id::text, emails_processed, emails_deleted,
			       emails_archived, started_at, completed_at, error, notes
			FROM retention_enforcement_log
			WHERE tenant_id = $1::uuid
			ORDER BY started_at DESC
			LIMIT $2
		`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r EnforcementRun
			if err := rows.Scan(&r.ID, &r.PolicyID, &r.EmailsProcessed, &r.EmailsDeleted,
				&r.EmailsArchived, &r.StartedAt, &r.CompletedAt, &r.Error, &r.Notes); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ErrNotFound is exported for handler 404 mapping.
var ErrNotFound = pgx.ErrNoRows
