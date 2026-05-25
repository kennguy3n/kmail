// Package tenant — alias CRUD.
//
// Aliases are secondary email addresses that route inbound mail to
// a primary user (`users.email`). The schema for the `aliases`
// table ships with `migrations/001_initial_schema.sql`; this file
// implements the service / handler surface that operates on it.
//
// Persistence model: every row is tenant-scoped, RLS-isolated via
// the `app.tenant_id` GUC (the same pattern every other write in
// this package follows). Stalwart-side sync is delegated to the
// `StalwartAliasSync` interface so the in-process write commits
// before the BFF tries to push to Stalwart — a Stalwart blip leaves
// the BFF row in place and is retried by the operator out of band
// rather than rolling back the user-visible alias creation.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Alias is the API representation of a row in `aliases`.
type Alias struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	UserID     string    `json:"user_id"`
	AliasEmail string    `json:"alias_email"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CreateAliasInput is the JSON body for `POST /api/v1/tenants/{id}/aliases`.
type CreateAliasInput struct {
	UserID     string `json:"user_id"`
	AliasEmail string `json:"alias_email"`
}

// ErrAliasInUse is returned by CreateAlias when the alias email
// collides with another row (the `aliases.alias_email` column has a
// global UNIQUE constraint — see migrations/001_initial_schema.sql).
// Mapped to HTTP 409 by `statusForServiceError`.
var ErrAliasInUse = errors.New("alias email already in use")

// StalwartAliasSync is the narrow slice of the Stalwart admin API
// the tenant service consumes to mirror alias CRUD into Stalwart's
// principal database. Defined as an interface here so:
//
//   - the in-process write commits without depending on Stalwart's
//     reachability,
//   - tests can substitute a recorder without spinning up Stalwart,
//   - production wires `StalwartAliasHTTPSync` which talks to
//     Stalwart's `/api/principal/{name}` admin endpoint.
//
// Implementations MUST be idempotent: AddAlias on an existing row
// and RemoveAlias on a missing row both return nil. The service
// retries on transient errors but does not roll back the BFF write
// when Stalwart sync fails — failures are logged and surfaced via
// the operator audit log instead so a Stalwart outage doesn't
// break the admin console.
type StalwartAliasSync interface {
	AddAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string) error
	RemoveAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string) error
}

// WithStalwartAliasSync returns a copy of the Service wired to the
// provided Stalwart sync. The Tenant Service otherwise treats
// Stalwart sync as a no-op (e.g. unit tests, dev compose without
// Stalwart admin credentials).
func (s *Service) WithStalwartAliasSync(sync StalwartAliasSync) *Service {
	cp := *s
	cp.aliasSync = sync
	return &cp
}

// normalizeAliasEmail validates and lower-cases an alias address.
// The `aliases.alias_email` column is the lookup key for inbound
// SMTP, which compares case-insensitively per RFC 5321 §2.4 — so
// the BFF lower-cases at the boundary to avoid two rows that differ
// only in case both claiming the same address.
func normalizeAliasEmail(in string) (string, error) {
	trimmed := strings.TrimSpace(in)
	if trimmed == "" {
		return "", fmt.Errorf("%w: alias_email is required", ErrInvalidInput)
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: alias_email must be a valid RFC 5322 address", ErrInvalidInput)
	}
	// `mail.ParseAddress` accepts angle-bracketed "Name <a@b>"
	// inputs; we only want the bare address here so the row
	// stores `a@b` instead of `Name <a@b>`.
	lowered := strings.ToLower(addr.Address)
	if !strings.Contains(lowered, "@") {
		return "", fmt.Errorf("%w: alias_email must contain '@'", ErrInvalidInput)
	}
	return lowered, nil
}

// CreateAlias inserts a new alias row for the given user. The user
// MUST already exist inside the tenant (FK constraint enforced by
// `aliases.user_id` referencing `users.id`). Stalwart sync runs
// after the BFF row commits so a Stalwart blip leaves the row
// visible in the admin console and gets retried by the operator.
func (s *Service) CreateAlias(ctx context.Context, tenantID string, in CreateAliasInput) (*Alias, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	if in.UserID == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrInvalidInput)
	}
	alias, err := normalizeAliasEmail(in.AliasEmail)
	if err != nil {
		return nil, err
	}
	if s.pool == nil {
		return nil, fmt.Errorf("%w: tenant service requires a database pool", ErrInvalidInput)
	}
	var a Alias
	var stalwartAccountID string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Confirm the user is part of the tenant before
		// inserting — without this the FK only catches
		// cross-tenant inserts when the destination tenant has
		// no row with that id, which leaks alias rows under a
		// shared-id collision. RLS already filters the SELECT
		// to the current tenant.
		if err := tx.QueryRow(ctx, `
			SELECT stalwart_account_id
			FROM users
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, in.UserID, tenantID).Scan(&stalwartAccountID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO aliases (tenant_id, user_id, alias_email)
			VALUES ($1::uuid, $2::uuid, $3)
			RETURNING id::text, tenant_id::text, user_id::text,
			          alias_email, created_at, updated_at
		`, tenantID, in.UserID, alias).Scan(
			&a.ID, &a.TenantID, &a.UserID, &a.AliasEmail,
			&a.CreatedAt, &a.UpdatedAt,
		)
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAliasInUse
		}
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("insert alias: %w", err)
	}
	if s.aliasSync != nil {
		if err := s.aliasSync.AddAlias(ctx, tenantID, stalwartAccountID, a.AliasEmail); err != nil {
			// Don't roll back: the BFF row is the source of
			// truth for the admin console; Stalwart sync is
			// retried out of band.
			return &a, fmt.Errorf("alias created in control plane but stalwart sync failed: %w", err)
		}
	}
	return &a, nil
}

// ListAliases returns every alias for a tenant, RLS-scoped via the
// `app.tenant_id` GUC.
func (s *Service) ListAliases(ctx context.Context, tenantID string) ([]Alias, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	return s.listAliases(ctx, tenantID, "")
}

// ListUserAliases returns the aliases assigned to a specific user
// inside the tenant. RLS filters out other tenants automatically;
// the explicit `user_id = $2` predicate narrows to one user.
func (s *Service) ListUserAliases(ctx context.Context, tenantID, userID string) ([]Alias, error) {
	if tenantID == "" || userID == "" {
		return nil, fmt.Errorf("%w: tenant id and user id are required", ErrInvalidInput)
	}
	return s.listAliases(ctx, tenantID, userID)
}

func (s *Service) listAliases(ctx context.Context, tenantID, userID string) ([]Alias, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("%w: tenant service requires a database pool", ErrInvalidInput)
	}
	var out []Alias
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := queryAliasesTx(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Alias
			if err := rows.Scan(
				&a.ID, &a.TenantID, &a.UserID, &a.AliasEmail,
				&a.CreatedAt, &a.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	return out, nil
}

// queryAliasesTx runs the per-tenant alias SELECT inside an open
// transaction. Split out from `listAliases` so the same body covers
// both the tenant-wide and per-user paths without ad-hoc string
// concatenation that confuses static analyzers.
func queryAliasesTx(ctx context.Context, tx pgx.Tx, tenantID, userID string) (pgx.Rows, error) {
	if userID == "" {
		return tx.Query(ctx, `
			SELECT id::text, tenant_id::text, user_id::text,
			       alias_email, created_at, updated_at
			FROM aliases
			WHERE tenant_id = $1::uuid
			ORDER BY alias_email ASC
		`, tenantID)
	}
	return tx.Query(ctx, `
		SELECT id::text, tenant_id::text, user_id::text,
		       alias_email, created_at, updated_at
		FROM aliases
		WHERE tenant_id = $1::uuid AND user_id = $2::uuid
		ORDER BY alias_email ASC
	`, tenantID, userID)
}

// DeleteAlias removes an alias row. Returns ErrNotFound when the
// alias does not exist in the tenant. Stalwart sync runs after the
// BFF delete commits so a Stalwart outage leaves the row gone in
// the admin console and the operator can re-run a reconciliation
// job later.
func (s *Service) DeleteAlias(ctx context.Context, tenantID, aliasID string) error {
	if tenantID == "" || aliasID == "" {
		return fmt.Errorf("%w: tenant id and alias id are required", ErrInvalidInput)
	}
	if s.pool == nil {
		return fmt.Errorf("%w: tenant service requires a database pool", ErrInvalidInput)
	}
	var aliasEmail, stalwartAccountID string
	var affected int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Join `aliases` to `users` so we get both the freed
		// alias address AND the Stalwart account identifier in
		// one round-trip. Stalwart sync uses the same identifier
		// the JMAP proxy stamps on the `X-KMail-Stalwart-Account-Id`
		// header. RLS scopes both tables to the current tenant.
		var userID string
		err := tx.QueryRow(ctx, `
			DELETE FROM aliases
			WHERE id = $1::uuid AND tenant_id = $2::uuid
			RETURNING alias_email, user_id::text
		`, aliasID, tenantID).Scan(&aliasEmail, &userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		affected = 1
		if s.aliasSync != nil {
			return tx.QueryRow(ctx, `
				SELECT stalwart_account_id FROM users
				WHERE id = $1::uuid AND tenant_id = $2::uuid
			`, userID, tenantID).Scan(&stalwartAccountID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if s.aliasSync != nil {
		if err := s.aliasSync.RemoveAlias(ctx, tenantID, stalwartAccountID, aliasEmail); err != nil {
			return fmt.Errorf("alias deleted in control plane but stalwart sync failed: %w", err)
		}
	}
	return nil
}

// isUniqueViolation matches the pgx error emitted when a unique
// constraint is violated. The `aliases.alias_email` column has a
// global UNIQUE constraint, so this is the only "alias already
// exists" code path.
//
// We unwrap via `errors.As` against `*pgconn.PgError` rather than
// substring-matching on `err.Error()` so the check survives error
// wrappers and locale-tagged messages.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

