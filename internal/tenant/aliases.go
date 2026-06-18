// Package tenant — alias CRUD.
//
// Aliases are secondary email addresses that route inbound mail to
// a primary user (`users.email`). The schema for the `aliases`
// table ships with `migrations/001_baseline.sql`; this file
// implements the service / handler surface that operates on it.
//
// Persistence model: every row is tenant-scoped, RLS-isolated via
// the `app.tenant_id` GUC (the same pattern every other write in
// this package follows).
//
// Stalwart sync is best-effort and persisted via the
// `alias_stalwart_sync_queue` table (see `migrations/001_baseline.sql`).
// Each alias CRUD writes the alias row AND the queue intent atomically, then
// attempts Stalwart sync inline. On inline success the queue row
// is marked `synced` immediately; on inline failure the row stays
// `pending` and the `AliasStalwartSyncWorker` retries it with
// exponential backoff. The handler always sees a successful
// service call regardless of Stalwart reachability — a Stalwart
// outage no longer fails alias creation in the admin console.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
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
// global UNIQUE constraint — see migrations/001_baseline.sql).
// Mapped to HTTP 409 by `statusForServiceError`.
var ErrAliasInUse = errors.New("alias email already in use")

// StalwartAliasSync is the narrow slice of the Stalwart admin API
// the tenant service consumes to mirror alias CRUD into Stalwart's
// principal database. Defined as an interface here so:
//
//   - the in-process write commits without depending on Stalwart's
//     reachability,
//   - tests can substitute a recorder without spinning up Stalwart,
//   - production wires `StalwartAliasHTTPSync` which edits the
//     principal's `aliases` list via the `x:Account/set` JMAP
//     management method.
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
// `aliases.user_id` referencing `users.id`). Stalwart sync is
// enqueued atomically with the alias write and then attempted
// inline; failures leave the queue row pending for the background
// `AliasStalwartSyncWorker` to retry. The caller always sees a
// successful response — a Stalwart outage does not fail alias
// creation in the admin console.
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
	var syncQueueID string
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
		if err := tx.QueryRow(ctx, `
			INSERT INTO aliases (tenant_id, user_id, alias_email)
			VALUES ($1::uuid, $2::uuid, $3)
			RETURNING id::text, tenant_id::text, user_id::text,
			          alias_email, created_at, updated_at
		`, tenantID, in.UserID, alias).Scan(
			&a.ID, &a.TenantID, &a.UserID, &a.AliasEmail,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return err
		}
		// Atomic enqueue: if the caller has wired a Stalwart
		// sync target, the intent lives in the same transaction
		// as the alias INSERT so a crash between the two is
		// impossible. The worker drains this queue.
		if s.aliasSync != nil {
			id, err := enqueueAliasSyncTx(ctx, tx, tenantID, aliasSyncOpAdd, stalwartAccountID, a.AliasEmail)
			if err != nil {
				return fmt.Errorf("enqueue alias sync: %w", err)
			}
			syncQueueID = id
		}
		return nil
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
	s.attemptAliasSyncInline(ctx, tenantID, syncQueueID, aliasSyncOpAdd, stalwartAccountID, a.AliasEmail)
	return &a, nil
}

// attemptAliasSyncInline runs the best-effort inline Stalwart sync
// for a queue row that was just enqueued by CreateAlias /
// DeleteAlias. Most calls succeed inline and the queue row is
// marked `synced` immediately; failures leave the row pending for
// the worker. The function never returns an error — that's the
// whole point of the queue.
//
// `syncQueueID` may be empty when the caller has no Stalwart sync
// wired (queue insert was skipped); in that case this is a no-op.
func (s *Service) attemptAliasSyncInline(
	ctx context.Context,
	tenantID, syncQueueID string,
	op aliasSyncOp,
	stalwartAccountID, aliasEmail string,
) {
	if s.aliasSync == nil || syncQueueID == "" {
		return
	}
	logger := s.aliasLogger()
	var syncErr error
	switch op {
	case aliasSyncOpAdd:
		syncErr = s.aliasSync.AddAlias(ctx, tenantID, stalwartAccountID, aliasEmail)
	case aliasSyncOpRemove:
		syncErr = s.aliasSync.RemoveAlias(ctx, tenantID, stalwartAccountID, aliasEmail)
	default:
		logger.Printf("alias sync: unknown operation %q for queue row %s", op, syncQueueID)
		return
	}
	if syncErr == nil {
		if markErr := markAliasSyncSynced(ctx, s.pool, syncQueueID); markErr != nil {
			logger.Printf("alias sync: mark synced for queue %s failed: %v", syncQueueID, markErr)
		}
		return
	}
	// Inline attempt failed. Record the failure on the queue row
	// so the worker sees an accurate `attempts` / `last_error`
	// and can apply exponential backoff. The row stays `pending`
	// for the worker to drain.
	if markErr := recordAliasSyncFailure(ctx, s.pool, syncQueueID, syncErr.Error(), nextAliasSyncBackoff(1)); markErr != nil {
		logger.Printf("alias sync: record inline failure for queue %s failed: %v (sync error: %v)", syncQueueID, markErr, syncErr)
		return
	}
	logger.Printf("alias sync inline attempt failed for tenant=%s alias=%s op=%s queue=%s: %v (worker will retry)", tenantID, aliasEmail, op, syncQueueID, syncErr)
}

// aliasLogger returns the Service's logger, falling back to the
// standard logger when none is wired. Keeps every call site
// trivially nil-safe.
func (s *Service) aliasLogger() *log.Logger {
	if s.logger != nil {
		return s.logger
	}
	return log.Default()
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
// alias does not exist in the tenant. Stalwart sync is enqueued
// atomically with the alias delete and attempted inline; failures
// leave the queue row pending for the worker. The caller always
// sees a successful response (or ErrNotFound) — a Stalwart outage
// does not fail alias deletion in the admin console.
func (s *Service) DeleteAlias(ctx context.Context, tenantID, aliasID string) error {
	if tenantID == "" || aliasID == "" {
		return fmt.Errorf("%w: tenant id and alias id are required", ErrInvalidInput)
	}
	if s.pool == nil {
		return fmt.Errorf("%w: tenant service requires a database pool", ErrInvalidInput)
	}
	var aliasEmail, stalwartAccountID, syncQueueID string
	var affected int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Two separate queries on different tables, sharing
		// the same transaction so the alias delete and the
		// Stalwart sync intent are committed atomically:
		//
		//   1. DELETE FROM aliases ... RETURNING alias_email, user_id
		//   2. SELECT users.stalwart_account_id WHERE id = user_id
		//
		// Stalwart sync uses the same `stalwart_account_id` the
		// JMAP proxy stamps on the `X-KMail-Stalwart-Account-Id`
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
			// Tolerate a missing user row here. A future hard-
			// delete migration on `users` could leave the alias
			// queue without a principal to update in Stalwart;
			// in that case the principal is gone too, so the
			// remove is a no-op upstream. We MUST NOT let the
			// missing row roll back the alias DELETE the admin
			// just performed.
			err := tx.QueryRow(ctx, `
				SELECT stalwart_account_id FROM users
				WHERE id = $1::uuid AND tenant_id = $2::uuid
			`, userID, tenantID).Scan(&stalwartAccountID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if stalwartAccountID == "" {
				// Principal already gone; skip enqueue.
				return nil
			}
			id, err := enqueueAliasSyncTx(ctx, tx, tenantID, aliasSyncOpRemove, stalwartAccountID, aliasEmail)
			if err != nil {
				return fmt.Errorf("enqueue alias sync: %w", err)
			}
			syncQueueID = id
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete alias: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	s.attemptAliasSyncInline(ctx, tenantID, syncQueueID, aliasSyncOpRemove, stalwartAccountID, aliasEmail)
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

