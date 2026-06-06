package featureflags

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultReadTimeout bounds how long a control-plane read (the admin
// GET and the resolver's background refresh) may block on Postgres.
// Without it, a frozen or unreachable database hangs the read
// indefinitely — the request only unblocks when the client gives up
// (e.g. curl --max-time), and the chaos-postgres harness measured this
// as a hard hang with no DB-side ceiling. 5s is comfortably above a
// healthy two-query read (single-digit ms) yet fails fast enough that a
// degraded DB surfaces as a retryable 503 instead of a stuck request.
// The flag evaluation path stays available regardless: the Service
// serves its last in-memory snapshot when a refresh times out.
const defaultReadTimeout = 5 * time.Second

// Store is the pgx-backed persistence layer over the `feature_flags`
// and `feature_flag_overrides` tables (migration 006). It implements
// the unexported [source] interface the [Service] resolves against and
// also exposes the admin write operations the handlers call.
//
// These are control-plane tables (no RLS — see migration 006), so the
// Store queries the pool directly without setting the app.tenant_id
// GUC, mirroring tenant.ShardService.
type Store struct {
	pool        *pgxpool.Pool
	readTimeout time.Duration
}

// NewStore builds a Store. A nil pool yields a Store whose reads return
// empty sets and whose writes error — matching the stub behaviour the
// rest of the codebase uses so a binary without Postgres still boots.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, readTimeout: defaultReadTimeout}
}

// WithReadTimeout overrides the per-read deadline applied to the
// control-plane read paths. A non-positive duration disables the
// deadline (reads block on the caller's context only) — use this to
// opt out, e.g. when the pool already enforces a statement_timeout.
// Returns the receiver for chaining.
func (s *Store) WithReadTimeout(d time.Duration) *Store {
	s.readTimeout = d
	return s
}

// readContext derives the context used for a read query. When a
// readTimeout is configured it bounds the read so a stalled Postgres
// fails fast instead of hanging; otherwise it returns the caller's
// context unchanged. The returned cancel func must always be called.
func (s *Store) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.readTimeout > 0 {
		return context.WithTimeout(ctx, s.readTimeout)
	}
	return ctx, func() {}
}

// ErrNoPool is returned by write operations when the Store has no pool.
var ErrNoPool = errors.New("featureflags: no database pool configured")

// loadAll returns the full registry and every override in one pair of
// queries. Implements source.
func (s *Store) loadAll(ctx context.Context) ([]Flag, []Override, error) {
	if s.pool == nil {
		return nil, nil, nil
	}
	// One deadline covers both reads: the admin GET and the resolver
	// refresh must complete within the read budget or fail fast.
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	flags, err := s.listFlags(ctx)
	if err != nil {
		return nil, nil, err
	}
	overrides, err := s.listOverrides(ctx)
	if err != nil {
		return nil, nil, err
	}
	return flags, overrides, nil
}

// loadViews returns every flag with its overrides nested, sorted for
// stable admin output. Used by the admin handlers (which hold a Store,
// not the resolver Service).
func (s *Store) loadViews(ctx context.Context) ([]FlagView, error) {
	flags, overrides, err := s.loadAll(ctx)
	if err != nil {
		return nil, err
	}
	return assembleViews(flags, overrides), nil
}

func (s *Store) listFlags(ctx context.Context) ([]Flag, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, description, default_enabled, created_at, updated_at
		FROM feature_flags
		ORDER BY key
	`)
	if err != nil {
		return nil, fmt.Errorf("featureflags: list flags: %w", err)
	}
	defer rows.Close()
	var out []Flag
	for rows.Next() {
		var f Flag
		if err := rows.Scan(&f.Key, &f.Description, &f.DefaultEnabled, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) listOverrides(ctx context.Context) ([]Override, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, flag_key, scope, scope_id, enabled, created_at, updated_at
		FROM feature_flag_overrides
		ORDER BY flag_key, scope, scope_id
	`)
	if err != nil {
		return nil, fmt.Errorf("featureflags: list overrides: %w", err)
	}
	defer rows.Close()
	var out []Override
	for rows.Next() {
		var o Override
		var scope string
		if err := rows.Scan(&o.ID, &o.FlagKey, &scope, &o.ScopeID, &o.Enabled, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		o.Scope = Scope(scope)
		out = append(out, o)
	}
	return out, rows.Err()
}

// tenantPlan resolves a tenant's billing plan. Implements source. An
// unknown tenant yields ("", nil) so a stale/invalid tenant id simply
// disables plan-scoped overrides instead of erroring the evaluation.
func (s *Store) tenantPlan(ctx context.Context, tenantID string) (string, error) {
	if s.pool == nil || tenantID == "" {
		return "", nil
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	var plan string
	err := s.pool.QueryRow(ctx, `
		SELECT plan FROM tenants WHERE id = $1::uuid
	`, tenantID).Scan(&plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("featureflags: tenant plan: %w", err)
	}
	return plan, nil
}

// UpsertFlag creates or updates a flag registry entry. The key is the
// natural primary key, so an existing key has its description and
// default refreshed (and updated_at bumped). Returns the stored row.
func (s *Store) UpsertFlag(ctx context.Context, f Flag) (*Flag, error) {
	if s.pool == nil {
		return nil, ErrNoPool
	}
	if f.Key == "" {
		return nil, errors.New("featureflags: flag key required")
	}
	var out Flag
	err := s.pool.QueryRow(ctx, `
		INSERT INTO feature_flags (key, description, default_enabled)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE
		   SET description = EXCLUDED.description,
		       default_enabled = EXCLUDED.default_enabled,
		       updated_at = now()
		RETURNING key, description, default_enabled, created_at, updated_at
	`, f.Key, f.Description, f.DefaultEnabled).
		Scan(&out.Key, &out.Description, &out.DefaultEnabled, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("featureflags: upsert flag: %w", err)
	}
	return &out, nil
}

// DeleteFlag removes a flag and (via ON DELETE CASCADE) its overrides.
// Deleting a non-existent flag is a no-op.
func (s *Store) DeleteFlag(ctx context.Context, key string) error {
	if s.pool == nil {
		return ErrNoPool
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM feature_flags WHERE key = $1`, key)
	if err != nil {
		return fmt.Errorf("featureflags: delete flag: %w", err)
	}
	return nil
}

// SetOverride upserts a scoped override, keyed by (flag, scope,
// scope_id). The flag must already exist (FK). Returns the stored row.
func (s *Store) SetOverride(ctx context.Context, o Override) (*Override, error) {
	if s.pool == nil {
		return nil, ErrNoPool
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	var out Override
	var scope string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO feature_flag_overrides (flag_key, scope, scope_id, enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flag_key, scope, scope_id) DO UPDATE
		   SET enabled = EXCLUDED.enabled,
		       updated_at = now()
		RETURNING id::text, flag_key, scope, scope_id, enabled, created_at, updated_at
	`, o.FlagKey, string(o.Scope), o.ScopeID, o.Enabled).
		Scan(&out.ID, &out.FlagKey, &scope, &out.ScopeID, &out.Enabled, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("featureflags: set override: %w", err)
	}
	out.Scope = Scope(scope)
	return &out, nil
}

// DeleteOverride removes a scoped override. Removing a non-existent
// override is a no-op.
func (s *Store) DeleteOverride(ctx context.Context, flagKey string, scope Scope, scopeID string) error {
	if s.pool == nil {
		return ErrNoPool
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM feature_flag_overrides
		WHERE flag_key = $1 AND scope = $2 AND scope_id = $3
	`, flagKey, string(scope), scopeID)
	if err != nil {
		return fmt.Errorf("featureflags: delete override: %w", err)
	}
	return nil
}

// Validate checks an override is well-formed before it hits Postgres,
// mirroring the CHECK constraints in migration 006 so the API returns
// a 400 with a clear message rather than a raw constraint violation.
func (o Override) Validate() error {
	if o.FlagKey == "" {
		return errors.New("featureflags: override flag_key required")
	}
	if _, ok := validScopes[o.Scope]; !ok {
		return fmt.Errorf("featureflags: invalid scope %q", o.Scope)
	}
	if o.Scope == ScopeGlobal {
		if o.ScopeID != "" {
			return errors.New("featureflags: global override must have empty scope_id")
		}
	} else if o.ScopeID == "" {
		return fmt.Errorf("featureflags: scope %q requires a non-empty scope_id", o.Scope)
	}
	return nil
}
