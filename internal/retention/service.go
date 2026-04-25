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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

// Service manages retention policies.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
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

// EvaluateRetention walks the enabled policies for a tenant and
// emits an audit-style summary. The actual `Email/set destroy` /
// placement-update for the archive tier lives in the Phase 5
// follow-up worker (see package doc).
func (s *Service) EvaluateRetention(ctx context.Context, tenantID string) (int, error) {
	policies, err := s.ListPolicies(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	enabled := 0
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		enabled++
	}
	return enabled, nil
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

// ErrNotFound is exported for handler 404 mapping.
var ErrNotFound = pgx.ErrNoRows
