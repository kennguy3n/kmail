// Package tenant hosts the Tenant Service business logic: the
// tenant lifecycle (create / suspend / delete / rename / rotate),
// user lifecycle, aliases, shared inboxes, and quotas.
//
// Authoritative for the control-plane Postgres schema defined in
// `docs/SCHEMA.md`. See `docs/ARCHITECTURE.md` §7 for the service
// topology.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Service holds the Tenant Service dependencies and exposes the
// tenant / user / alias / shared-inbox lifecycle methods consumed by
// the HTTP handlers in this package.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service wired to the provided pgx pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Tenant is the API representation of a row in `tenants`.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Plan      string    `json:"plan"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User is the API representation of a row in `users`.
type User struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	KChatUserID       string    `json:"kchat_user_id"`
	StalwartAccountID string    `json:"stalwart_account_id"`
	Email             string    `json:"email"`
	DisplayName       string    `json:"display_name"`
	Role              string    `json:"role"`
	Status            string    `json:"status"`
	QuotaBytes        int64     `json:"quota_bytes"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Domain is the API representation of a row in `domains`.
type Domain struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	Domain         string    `json:"domain"`
	Verified       bool      `json:"verified"`
	MXVerified     bool      `json:"mx_verified"`
	SPFVerified    bool      `json:"spf_verified"`
	DKIMVerified   bool      `json:"dkim_verified"`
	DMARCVerified  bool      `json:"dmarc_verified"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SharedInbox is the API representation of a row in `shared_inboxes`.
type SharedInbox struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Address     string    `json:"address"`
	DisplayName string    `json:"display_name"`
	MLSGroupID  string    `json:"mls_group_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateTenantInput carries the fields accepted by POST /api/v1/tenants.
type CreateTenantInput struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
	Plan string `json:"plan"`
}

// CreateUserInput carries the fields accepted by
// POST /api/v1/tenants/:id/users.
type CreateUserInput struct {
	KChatUserID       string `json:"kchat_user_id"`
	StalwartAccountID string `json:"stalwart_account_id"`
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	Role              string `json:"role"`
	QuotaBytes        int64  `json:"quota_bytes"`
}

// CreateDomainInput carries the fields accepted by
// POST /api/v1/tenants/:id/domains.
type CreateDomainInput struct {
	Domain string `json:"domain"`
}

// CreateSharedInboxInput carries the fields accepted by
// POST /api/v1/tenants/:id/shared-inboxes.
type CreateSharedInboxInput struct {
	Address     string `json:"address"`
	DisplayName string `json:"display_name"`
	MLSGroupID  string `json:"mls_group_id"`
}

// ErrNotFound is returned when a lookup resolves no rows.
var ErrNotFound = errors.New("not found")

// ErrInvalidInput wraps caller-visible validation failures (missing
// or malformed fields on a Create* input). Handlers surface this as
// HTTP 400; every other Service error is treated as 500.
var ErrInvalidInput = errors.New("invalid input")

// CreateTenant inserts a new tenant row and returns the persisted
// representation. Tenants are the only entity not subject to RLS —
// creating one is a control-plane-admin operation, not a
// tenant-scoped one.
func (s *Service) CreateTenant(ctx context.Context, in CreateTenantInput) (*Tenant, error) {
	if in.Name == "" || in.Slug == "" || in.Plan == "" {
		return nil, fmt.Errorf("%w: name, slug, and plan are required", ErrInvalidInput)
	}
	var t Tenant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan)
		VALUES ($1, $2, $3)
		RETURNING id::text, name, slug, plan, status, created_at, updated_at
	`, in.Name, in.Slug, in.Plan).Scan(
		&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert tenant: %w", err)
	}
	return &t, nil
}

// GetTenant fetches a tenant by ID.
func (s *Service) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	var t Tenant
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, slug, plan, status, created_at, updated_at
		FROM tenants
		WHERE id = $1::uuid
	`, id).Scan(&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select tenant: %w", err)
	}
	return &t, nil
}

// CreateUser inserts a new user inside the given tenant. It runs
// inside a transaction with `app.tenant_id` set to `tenantID` so the
// RLS policy on `users` validates the insert.
func (s *Service) CreateUser(ctx context.Context, tenantID string, in CreateUserInput) (*User, error) {
	if in.KChatUserID == "" || in.StalwartAccountID == "" || in.Email == "" || in.DisplayName == "" {
		return nil, fmt.Errorf("%w: kchat_user_id, stalwart_account_id, email, and display_name are required", ErrInvalidInput)
	}
	role := in.Role
	if role == "" {
		role = "member"
	}
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO users (
				tenant_id, kchat_user_id, stalwart_account_id, email,
				display_name, role, quota_bytes
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
			RETURNING id::text, tenant_id::text, kchat_user_id,
			          stalwart_account_id, email, display_name, role,
			          status, quota_bytes, created_at, updated_at
		`,
			tenantID, in.KChatUserID, in.StalwartAccountID, in.Email,
			in.DisplayName, role, in.QuotaBytes,
		).Scan(
			&u.ID, &u.TenantID, &u.KChatUserID, &u.StalwartAccountID,
			&u.Email, &u.DisplayName, &u.Role, &u.Status, &u.QuotaBytes,
			&u.CreatedAt, &u.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	return &u, nil
}

// CreateDomain adds a domain to a tenant. Verification flags start
// false and are flipped by the DNS Onboarding Service as each DNS
// record is observed — see `docs/ARCHITECTURE.md` §7.
func (s *Service) CreateDomain(ctx context.Context, tenantID string, in CreateDomainInput) (*Domain, error) {
	if in.Domain == "" {
		return nil, fmt.Errorf("%w: domain is required", ErrInvalidInput)
	}
	var d Domain
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO domains (tenant_id, domain)
			VALUES ($1::uuid, $2)
			RETURNING id::text, tenant_id::text, domain, verified,
			          mx_verified, spf_verified, dkim_verified,
			          dmarc_verified, created_at, updated_at
		`, tenantID, in.Domain).Scan(
			&d.ID, &d.TenantID, &d.Domain, &d.Verified,
			&d.MXVerified, &d.SPFVerified, &d.DKIMVerified,
			&d.DMARCVerified, &d.CreatedAt, &d.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert domain: %w", err)
	}
	return &d, nil
}

// ListDomains returns every domain owned by a tenant.
func (s *Service) ListDomains(ctx context.Context, tenantID string) ([]Domain, error) {
	var out []Domain
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, domain, verified,
			       mx_verified, spf_verified, dkim_verified,
			       dmarc_verified, created_at, updated_at
			FROM domains
			WHERE tenant_id = $1::uuid
			ORDER BY created_at ASC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Domain
			if err := rows.Scan(
				&d.ID, &d.TenantID, &d.Domain, &d.Verified,
				&d.MXVerified, &d.SPFVerified, &d.DKIMVerified,
				&d.DMARCVerified, &d.CreatedAt, &d.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	return out, nil
}

// CreateSharedInbox creates a shared inbox for a tenant. Membership
// is managed separately via `shared_inbox_members` — this method
// only mints the inbox row and the MLS group reference.
func (s *Service) CreateSharedInbox(ctx context.Context, tenantID string, in CreateSharedInboxInput) (*SharedInbox, error) {
	if in.Address == "" || in.DisplayName == "" || in.MLSGroupID == "" {
		return nil, fmt.Errorf("%w: address, display_name, and mls_group_id are required", ErrInvalidInput)
	}
	var s2 SharedInbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO shared_inboxes (tenant_id, address, display_name, mls_group_id)
			VALUES ($1::uuid, $2, $3, $4)
			RETURNING id::text, tenant_id::text, address, display_name,
			          mls_group_id, created_at, updated_at
		`, tenantID, in.Address, in.DisplayName, in.MLSGroupID).Scan(
			&s2.ID, &s2.TenantID, &s2.Address, &s2.DisplayName,
			&s2.MLSGroupID, &s2.CreatedAt, &s2.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert shared inbox: %w", err)
	}
	return &s2, nil
}
