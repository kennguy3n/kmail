package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// MailboxSizer reports the cumulative mailbox size for a tenant in
// bytes. The auto-cutover worker uses this to decide whether the
// tenant has outgrown the Meilisearch single-node ceiling. Production
// wires this against Stalwart's admin API (`/admin/tenant/{id}/storage`);
// tests inject a fake. The contract is intentionally minimal — the
// worker doesn't care HOW the size is computed, only what it is.
type MailboxSizer interface {
	TenantMailboxSize(ctx context.Context, tenantID string) (int64, error)
}

// MailboxSizerFunc adapts a closure into the MailboxSizer
// interface, useful for inline wiring in main.go and for tests.
type MailboxSizerFunc func(ctx context.Context, tenantID string) (int64, error)

// TenantMailboxSize implements MailboxSizer.
func (f MailboxSizerFunc) TenantMailboxSize(ctx context.Context, tenantID string) (int64, error) {
	return f(ctx, tenantID)
}

// MessageSource pulls every indexable message for a tenant so the
// cutover worker can hand them to `Service.Reindex`. Production
// wires this against Stalwart's JMAP `Email/query` + `Email/get`
// loop; tests return a slice directly. Returning the full slice
// (rather than a stream) is fine for Phase 5 since the cutover
// threshold is bounded (default ~100k messages); a streaming
// refinement is a future optimisation.
type MessageSource interface {
	MessagesForTenant(ctx context.Context, tenantID string) ([]Message, error)
}

// MessageSourceFunc adapts a closure into the MessageSource
// interface for inline wiring.
type MessageSourceFunc func(ctx context.Context, tenantID string) ([]Message, error)

// MessagesForTenant implements MessageSource.
func (f MessageSourceFunc) MessagesForTenant(ctx context.Context, tenantID string) ([]Message, error) {
	return f(ctx, tenantID)
}

// BackendFlipper is the subset of *Service the cutover worker
// needs. Order matters: the worker MUST reindex into the target
// backend first (via ReindexTo, which targets a specific backend
// regardless of the tenant's current `search_backend` column) and
// only flip the column via SetBackend after the reindex succeeds.
// If we flipped the column first and the reindex failed, the
// tenant would be permanently invisible to retries (the candidate
// query filters on `search_backend = 'meilisearch'`) and stuck
// reading from an empty OpenSearch index.
type BackendFlipper interface {
	// ReindexTo bulk-imports `msgs` into a SPECIFIC backend,
	// independent of the tenant's `search_backend` column. The
	// worker calls this BEFORE SetBackend so a transient
	// destination-side failure (OpenSearch 502, network blip)
	// leaves the tenant readable on the source backend.
	ReindexTo(ctx context.Context, tenantID, backend string, msgs []Message) error
	// SetBackend flips the tenant's `search_backend` column.
	// Called only AFTER ReindexTo succeeds AND Validate passes so
	// reads route to a fully-populated, verified index.
	SetBackend(ctx context.Context, tenantID, backend string) error
	// Validate confirms the destination index is actually
	// queryable after ReindexTo — it samples a handful of the
	// just-migrated messages and searches for them in `backend`.
	// Called BETWEEN ReindexTo and SetBackend so a reindex that
	// "succeeded" but produced an unsearchable index (mapping
	// drift, silent bulk-import drop) is caught while the tenant
	// is still safely readable on the source backend. A non-nil
	// return aborts the cutover and leaves the tenant on source.
	Validate(ctx context.Context, tenantID, backend string, msgs []Message) error
}

// AuditLogger is the slice of `audit.Service` the cutover paths
// depend on. Kept as an interface so the worker / service can be
// unit-tested with a recording fake and so a nil value cleanly
// disables audit logging.
type AuditLogger interface {
	Log(ctx context.Context, e audit.Entry) (*audit.Entry, error)
}

// BackendGetter reports a tenant's current `search_backend`. The
// manual CutoverService uses it to record the source backend in
// the audit trail and to reject a no-op cutover (target already
// equals current). *Service satisfies it; it's optional on the
// service (nil simply skips the source-backend annotation).
type BackendGetter interface {
	GetBackend(ctx context.Context, tenantID string) (string, error)
}

// CutoverState enumerates the per-tenant cutover job states.
type CutoverState string

const (
	CutoverPending    CutoverState = "pending"
	CutoverInProgress CutoverState = "in_progress"
	CutoverCompleted  CutoverState = "completed"
	CutoverFailed     CutoverState = "failed"
)

// CandidateFilter parameterises CutoverStore.ListCandidates so the
// store can do the eligibility join in SQL (default impl) or in
// memory (test impl) without the worker reaching into store
// internals.
type CandidateFilter struct {
	// SourceBackend is the backend a tenant must currently be on
	// to be considered eligible. Together with TargetBackend it
	// uniquely identifies a transition.
	SourceBackend string
	// TargetBackend is the backend the worker intends to promote
	// the tenant TO. The store keys job rows by
	// `(tenant_id, target_backend)` so a tenant
	// previously promoted to a DIFFERENT target is NOT shielded
	// by an old `completed` row when a new transition needs to
	// run — e.g. a tenant who was promoted from legacy
	// `meilisearch` to `opensearch` (target=opensearch row
	// completed), later moved by an operator onto the modern
	// shared path (`search_backend = shared_meilisearch`), MUST
	// remain eligible for the `shared_meilisearch ->
	// shared_opensearch` transition when its mailbox crosses the
	// size threshold again. With a non-composite key the old
	// `completed` row would block the scan; with the composite
	// key the lookup is scoped to `target_backend = shared_opensearch`
	// and the row from the previous transition is invisible.
	//
	// Note: same-target re-promotion (e.g. operator reverts a
	// tenant from `shared_opensearch` back to `shared_meilisearch`
	// and wants the worker to immediately re-promote on the same
	// target) is intentionally NOT supported by the candidate
	// scan — the prior `completed` row on the same target still
	// matches and excludes the tenant. That path requires an
	// operator-issued job-row reset (DELETE the completed row, or
	// flip its state to `failed` so the back-off path picks it up).
	// Required.
	TargetBackend string
	// MaxFailures excludes tenants whose `failure_count` has
	// reached this value.
	MaxFailures int
	// RetryAfterBefore excludes `failed` rows whose `updated_at`
	// is more recent than this timestamp, implementing the
	// failure back-off window.
	RetryAfterBefore time.Time
}

// CutoverStore is the storage surface the worker depends on. The
// default Postgres-backed impl is `NewPostgresCutoverStore`; the
// `inMemoryCutoverStore` in `cutover_test.go` is what tests use.
// Decoupling the worker from pgx lets us exercise every state
// transition deterministically without a real database.
type CutoverStore interface {
	// ListCandidates returns tenant IDs that are eligible for a
	// cutover attempt under `f`. The store is responsible for
	// excluding rows that are already `in_progress` or `completed`
	// FOR THE SAME (tenant, target_backend) PAIR. A tenant
	// previously completed against a different target is still a
	// candidate for `f.TargetBackend`.
	ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error)
	// Claim transitions a tenant's job row to `in_progress` if no
	// other worker is currently holding it. Must be atomic — the
	// caller relies on a single winner across replicas.
	// `targetBackend` selects which (tenant, target) row to claim
	// so two transitions on the same tenant don't collide.
	Claim(ctx context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (bool, error)
	// MarkCompleted moves the row to `completed` and resets the
	// failure counter for the given (tenant, target).
	MarkCompleted(ctx context.Context, tenantID, targetBackend string, now time.Time) error
	// MarkFailed moves the row to `failed`, increments
	// `failure_count`, and records `reason`, scoped to the given
	// (tenant, target).
	MarkFailed(ctx context.Context, tenantID, targetBackend, reason string, now time.Time) error
	// ReconcileCompleted promotes any `in_progress` row to
	// `completed` when the tenant has ALREADY been flipped to
	// `targetBackend` (i.e., `Service.SetBackend` succeeded) AND
	// the row hasn't been updated since `before`. This is the
	// safety net for the unlucky case where SetBackend committed
	// but the follow-up MarkCompleted failed (Postgres blip, pod
	// killed mid-write). Without this, those rows would sit in
	// `in_progress` forever — they're not retryable via
	// ListCandidates (the tenant is already on OpenSearch) and
	// the bookkeeping eventually skews the cutover dashboard.
	// Idempotent: rows already in another state are left alone.
	// Returns the count of rows promoted so the worker can log it.
	// `now` is the timestamp written to `completed_at` / `updated_at`
	// — the worker passes `cfg.Now()` so the same injectable clock
	// used by Claim/MarkCompleted/MarkFailed governs this path too,
	// keeping integration tests deterministic.
	ReconcileCompleted(ctx context.Context, targetBackend string, before, now time.Time) (int64, error)
	// ReconcileStale demotes `in_progress` rows BACK to `failed`
	// when the tenant is still on `sourceBackend` (i.e., SetBackend
	// has NOT been called yet) AND the row hasn't been updated
	// since `before`. This is the complementary safety net to
	// ReconcileCompleted: it handles a pod that crashed AFTER
	// Claim but BEFORE either SetBackend or MarkFailed
	// (OOM-kill / SIGKILL / node failure during ReindexTo).
	// Without this, those rows would sit in `in_progress` forever:
	//
	//   - ListCandidates excludes `in_progress` rows (`cutover_state
	//     <> 'in_progress'`), so the next tick can't pick them up.
	//   - ReconcileCompleted only handles tenants ALREADY on the
	//     target backend.
	//
	// `targetBackend` scopes the demotion to the (tenant, target)
	// row owned by the current transition so a parallel transition
	// for the same tenant under a different target isn't touched.
	//
	// Demotion to `failed` (with an incremented `failure_count` and
	// a synthetic reason) lets the normal retry pathway re-promote
	// the row through ListCandidates on the next tick, governed by
	// the same `MaxFailures` / `MaxRetryGap` back-off as ordinary
	// failures so a wedged tenant doesn't pin a worker forever.
	// Idempotent: rows already in another state are left alone.
	// Returns the count of rows demoted so the worker can log it.
	// `now` is written to `updated_at` / `failed_at`; the worker
	// passes `cfg.Now()` so all four reconciliation/claim/mark
	// methods share one clock source.
	ReconcileStale(ctx context.Context, sourceBackend, targetBackend string, before, now time.Time) (int64, error)
	// UpsertPending inserts a `pending` job row for the given
	// (tenant, target) if none exists, refreshing `mailbox_size`
	// / `threshold` on an existing non-terminal row. It is the
	// entry point for the operator-triggered manual cutover: it
	// makes the tenant a claimable candidate without itself
	// running the migration. Returns the resulting job. If a row
	// is already `in_progress` it is returned unchanged (the
	// caller treats that as "a cutover is already running").
	UpsertPending(ctx context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (*CutoverJob, error)
	// Get returns the job row for a specific (tenant, target) or
	// ErrNotFound when none exists.
	Get(ctx context.Context, tenantID, targetBackend string) (*CutoverJob, error)
	// List returns every job row for a tenant ordered most-recent
	// first, so the admin UI can render the cutover history across
	// all targets the tenant has ever been promoted toward.
	List(ctx context.Context, tenantID string) ([]CutoverJob, error)
}

// CutoverJob is a row of `search_cutover_jobs` — the persisted
// state of one (tenant, target_backend) cutover. It is the shape
// returned by the store's Get / List / UpsertPending methods and
// serialised by the REST handlers for the admin UI.
type CutoverJob struct {
	TenantID      string       `json:"tenant_id"`
	TargetBackend string       `json:"target_backend"`
	State         CutoverState `json:"cutover_state"`
	MailboxSize   int64        `json:"mailbox_size"`
	Threshold     int64        `json:"threshold"`
	StartedAt     *time.Time   `json:"started_at,omitempty"`
	CompletedAt   *time.Time   `json:"completed_at,omitempty"`
	FailureCount  int          `json:"failure_count"`
	LastError     string       `json:"last_error,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

// PostgresCutoverStore is the default CutoverStore wired against
// Postgres. The schema lives in `migrations/001_baseline.sql`.
type PostgresCutoverStore struct {
	pool *pgxpool.Pool
}

// NewPostgresCutoverStore wraps a pool.
func NewPostgresCutoverStore(pool *pgxpool.Pool) *PostgresCutoverStore {
	return &PostgresCutoverStore{pool: pool}
}

// ListCandidates implements CutoverStore. The LEFT JOIN is scoped
// to the SAME `(tenant_id, target_backend)` as the candidate
// transition: a tenant with a `completed` row for a DIFFERENT
// target is still eligible. Filter SQL:
//
//   - tenant currently on f.SourceBackend, AND
//   - no job row for f.TargetBackend, OR the only row for
//     f.TargetBackend is a recoverable `failed` (failure_count
//     and back-off window predicates apply).
//
// `in_progress` rows are NOT candidates — they're either being
// actively driven by another worker (the Claim race-loser path)
// or about to be reconciled by ReconcileStale on a future tick.
func (s *PostgresCutoverStore) ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text
		FROM tenants t
		LEFT JOIN search_cutover_jobs j
		       ON j.tenant_id      = t.id
		      AND j.target_backend = $2
		WHERE t.search_backend = $1
		  AND (
		      j.tenant_id IS NULL
		      OR (j.cutover_state = 'failed'
		          AND j.failure_count < $3
		          AND j.updated_at < $4)
		  )
	`, f.SourceBackend, f.TargetBackend, f.MaxFailures, f.RetryAfterBefore)
	if err != nil {
		return nil, fmt.Errorf("scan tenants: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Claim implements CutoverStore. The UPSERT-then-conditional-UPDATE
// dance runs in one transaction so two replicas racing the same
// (tenant, target) pair land on a single winner. The composite
// `(tenant_id, target_backend)` PK lets the same tenant carry
// multiple rows (one per target) simultaneously.
func (s *PostgresCutoverStore) Claim(ctx context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (bool, error) {
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_cutover_jobs (tenant_id, target_backend, cutover_state, mailbox_size, threshold, started_at, updated_at)
			VALUES ($1, $2, 'pending', $3, $4, NULL, $5)
			ON CONFLICT (tenant_id, target_backend) DO NOTHING
		`, tenantID, targetBackend, size, threshold, now)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE search_cutover_jobs
			   SET cutover_state = 'in_progress',
			       mailbox_size  = $3,
			       started_at    = $4,
			       updated_at    = $4
			 WHERE tenant_id      = $1
			   AND target_backend = $2
			   AND cutover_state IN ('pending', 'failed')
		`, tenantID, targetBackend, size, now)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// MarkCompleted implements CutoverStore. Scoped to the specific
// (tenant, target) row so a parallel transition on the same
// tenant isn't accidentally finalised.
func (s *PostgresCutoverStore) MarkCompleted(ctx context.Context, tenantID, targetBackend string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'completed',
		       completed_at  = $3,
		       updated_at    = $3,
		       failure_count = 0,
		       last_error    = ''
		 WHERE tenant_id      = $1
		   AND target_backend = $2
	`, tenantID, targetBackend, now)
	return err
}

// MarkFailed implements CutoverStore. Scoped to the specific
// (tenant, target) row so two transitions sharing a tenant don't
// pollute each other's failure_count / last_error.
func (s *PostgresCutoverStore) MarkFailed(ctx context.Context, tenantID, targetBackend, reason string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'failed',
		       failure_count = failure_count + 1,
		       last_error    = $3,
		       updated_at    = $4
		 WHERE tenant_id      = $1
		   AND target_backend = $2
	`, tenantID, targetBackend, reason, now)
	return err
}

// ReconcileCompleted implements CutoverStore. The UPDATE joins
// against `tenants.search_backend` so the promotion is gated on
// the actual production state: only rows whose tenant is ALREADY
// on `targetBackend` AND whose job row keys to the same target
// get promoted. The `target_backend` scope prevents a parallel
// transition's `in_progress` row (different target) from being
// accidentally promoted because the tenant happens to be on this
// reconcile's target. `updated_at` is bumped to `now`
// (caller-provided via `cfg.Now()`) so the dashboard reflects the
// recovery and integration tests can drive a deterministic clock.
func (s *PostgresCutoverStore) ReconcileCompleted(ctx context.Context, targetBackend string, before, now time.Time) (int64, error) {
	// Both `j.target_backend = $1` and `t.search_backend = $1`
	// intentionally compare the same `$1` parameter: the row is
	// only a reconcile candidate when the JOB targeted this
	// backend AND the tenant has already been flipped to this
	// backend (SetBackend committed but MarkCompleted didn't). A
	// mismatched pair is either the wrong transition or the
	// stuck state ReconcileStale handles.
	tag, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs j
		   SET cutover_state = 'completed',
		       completed_at  = $3,
		       updated_at    = $3,
		       failure_count = 0,
		       last_error    = ''
		  FROM tenants t
		 WHERE j.tenant_id       = t.id
		   AND j.target_backend  = $1
		   AND j.cutover_state   = 'in_progress'
		   AND j.updated_at      < $2
		   AND t.search_backend  = $1
	`, targetBackend, before, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReconcileStale implements CutoverStore. Mirror of
// ReconcileCompleted for the opposite outcome: when a pod crashes
// between Claim and SetBackend, the row sits in `in_progress` with
// the tenant still on `sourceBackend`. The UPDATE demotes those
// rows back to `failed` (incrementing `failure_count` and stamping
// a synthetic reason) so the normal retry pathway can pick them up
// on the next tick.
//
// Predicates:
//   - `updated_at < $2` gates by the reconciliation horizon so
//     an in-flight migration on another worker isn't incorrectly
//     demoted.
//   - `tenants.search_backend = $1` guarantees we only touch
//     tenants still on the source backend — a tenant already on
//     the target would be handled by ReconcileCompleted instead.
//   - `target_backend` scopes the demotion to the row owned by
//     THIS transition. Without it, a parallel `in_progress` row
//     for a different (tenant, target) pair (same tenant, other
//     target) would be incorrectly demoted by this reconcile pass.
//
// The caller passes the source-side target_backend value, which
// the worker reads from `tr.Target` for the transition being
// reconciled. The PostgreSQL parameterisation packs both source
// and target into the same query: source for the tenants join,
// target for the job-row scope.
func (s *PostgresCutoverStore) ReconcileStale(ctx context.Context, sourceBackend, targetBackend string, before, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs j
		   SET cutover_state = 'failed',
		       failure_count = j.failure_count + 1,
		       last_error    = 'stale in_progress: assumed crash recovery',
		       updated_at    = $4
		  FROM tenants t
		 WHERE j.tenant_id       = t.id
		   AND j.target_backend  = $2
		   AND j.cutover_state   = 'in_progress'
		   AND j.updated_at      < $3
		   AND t.search_backend  = $1
	`, sourceBackend, targetBackend, before, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// cutoverJobColumns is the SELECT/RETURNING projection shared by
// the Get / List / UpsertPending readers so the scan order stays
// in lockstep with scanCutoverJob.
const cutoverJobColumns = `tenant_id::text, target_backend, cutover_state,
	mailbox_size, threshold, started_at, completed_at,
	failure_count, last_error, created_at, updated_at`

// rowScanner is the minimal surface shared by pgx.Row and
// pgx.Rows so scanCutoverJob works for both single-row and
// iterating reads.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanCutoverJob reads one `search_cutover_jobs` row into a
// CutoverJob. started_at / completed_at are nullable in the
// schema, so they scan into *time.Time (nil == SQL NULL).
func scanCutoverJob(row rowScanner) (*CutoverJob, error) {
	var j CutoverJob
	if err := row.Scan(
		&j.TenantID,
		&j.TargetBackend,
		&j.State,
		&j.MailboxSize,
		&j.Threshold,
		&j.StartedAt,
		&j.CompletedAt,
		&j.FailureCount,
		&j.LastError,
		&j.CreatedAt,
		&j.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &j, nil
}

// UpsertPending implements CutoverStore. Runs inside a
// tenant-scoped transaction so the RLS policy's WITH CHECK clause
// (`tenant_id = app.tenant_id`) is satisfied for the INSERT/UPDATE
// — unlike the worker's cross-tenant scans, the manual path is
// always operating on a single known tenant.
//
// An existing `in_progress` row is returned UNCHANGED so a manual
// re-initiate can't trample a cutover the worker (or a prior
// request) is actively running. Any other state (`pending`,
// `failed`, `completed`) is reset to `pending` with a cleared
// failure_count / last_error so the operator's explicit trigger
// overrides the failure back-off and the completed-row guard,
// making the tenant immediately claimable again.
func (s *PostgresCutoverStore) UpsertPending(ctx context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (*CutoverJob, error) {
	var job *CutoverJob
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO search_cutover_jobs
				(tenant_id, target_backend, cutover_state, mailbox_size, threshold, updated_at)
			VALUES ($1::uuid, $2, 'pending', $3, $4, $5)
			ON CONFLICT (tenant_id, target_backend) DO UPDATE
			   SET cutover_state = CASE WHEN search_cutover_jobs.cutover_state = 'in_progress'
			                            THEN search_cutover_jobs.cutover_state ELSE 'pending' END,
			       mailbox_size  = EXCLUDED.mailbox_size,
			       threshold     = EXCLUDED.threshold,
			       failure_count = CASE WHEN search_cutover_jobs.cutover_state = 'in_progress'
			                            THEN search_cutover_jobs.failure_count ELSE 0 END,
			       last_error    = CASE WHEN search_cutover_jobs.cutover_state = 'in_progress'
			                            THEN search_cutover_jobs.last_error ELSE '' END,
			       updated_at    = $5
			RETURNING `+cutoverJobColumns,
			tenantID, targetBackend, size, threshold, now.UTC())
		j, err := scanCutoverJob(row)
		if err != nil {
			return err
		}
		job = j
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("upsert pending cutover: %w", err)
	}
	return job, nil
}

// Get implements CutoverStore.
func (s *PostgresCutoverStore) Get(ctx context.Context, tenantID, targetBackend string) (*CutoverJob, error) {
	var job *CutoverJob
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `SELECT `+cutoverJobColumns+`
			FROM search_cutover_jobs
			WHERE tenant_id = $1::uuid AND target_backend = $2`, tenantID, targetBackend)
		j, err := scanCutoverJob(row)
		if err != nil {
			return err
		}
		job = j
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get cutover job: %w", err)
	}
	return job, nil
}

// List implements CutoverStore. Ordered most-recently-updated
// first so the admin UI shows the freshest cutover at the top.
func (s *PostgresCutoverStore) List(ctx context.Context, tenantID string) ([]CutoverJob, error) {
	var jobs []CutoverJob
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+cutoverJobColumns+`
			FROM search_cutover_jobs
			WHERE tenant_id = $1::uuid
			ORDER BY updated_at DESC, target_backend`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			j, err := scanCutoverJob(rows)
			if err != nil {
				return err
			}
			jobs = append(jobs, *j)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list cutover jobs: %w", err)
	}
	return jobs, nil
}

// CutoverMetrics is the Prometheus metric set for the cutover
// subsystem (both the auto-worker and the manual service share
// it). Exposed so main.go can register the collectors with the
// same registry the BFF serves on `/metrics`.
type CutoverMetrics struct {
	Completed  prometheus.Counter
	Failed     prometheus.Counter
	InProgress prometheus.Gauge
}

// NewCutoverMetrics builds the metric set and registers it with
// `reg`. Pass `nil` to skip registration (tests, and the
// worker/service defaults) — the collectors are still constructed
// so Inc/Dec calls are always safe, they're just not exported.
func NewCutoverMetrics(reg prometheus.Registerer) *CutoverMetrics {
	m := &CutoverMetrics{
		Completed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_search_cutover_completed_total",
			Help: "Total search cutovers that migrated, validated, and flipped a tenant to the target backend.",
		}),
		Failed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_search_cutover_failed_total",
			Help: "Total search cutovers that failed and left the tenant on the source backend.",
		}),
		InProgress: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kmail_search_cutover_in_progress",
			Help: "Search cutovers currently executing (claimed, not yet completed or failed).",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Completed, m.Failed, m.InProgress)
	}
	return m
}

// Register attaches the collectors to `reg`, tolerating a
// double-registration (returns the already-registered collector
// rather than panicking). Used when the metric set is built before
// the serving registry exists — e.g. the cutover worker is
// constructed earlier in main() than the Prometheus registry, so
// it's created with `nil` and registered here once the registry is
// available.
func (m *CutoverMetrics) Register(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	for _, c := range []prometheus.Collector{m.Completed, m.Failed, m.InProgress} {
		if err := reg.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				panic(fmt.Errorf("search: register cutover metric: %w", err))
			}
		}
	}
}

// migrationDeps bundles the collaborators the post-claim cutover
// dance needs. Both the auto-cutover worker and the manual
// CutoverService build one so the export→reindex→validate→flip→
// mark sequence (and its audit + metric side effects) stays
// byte-for-byte identical across the automatic and
// operator-triggered paths.
type migrationDeps struct {
	store                CutoverStore
	flipper              BackendFlipper
	source               MessageSource
	audit                AuditLogger
	metrics              *CutoverMetrics
	logger               *log.Logger
	now                  func() time.Time
	sleep                func(time.Duration)
	markCompletedRetries int
	// actorType / actorID attribute the audit entry. The worker
	// leaves these zero (defaults to ActorSystem); the manual
	// service sets ActorAdmin + the operator's user ID per call.
	actorType audit.ActorType
	actorID   string
}

// runMigration performs the export→reindex→validate→flip→mark
// dance for a tenant whose (tenant, target) row is ALREADY claimed
// (in_progress). `sourceBackend` is the backend the tenant is
// migrating off of; it's used for log + audit context. On any
// step failure the row is moved to `failed` (preserving the tenant
// on the source backend, since SetBackend hasn't run) and the
// error is returned. The in-flight gauge is held for the duration.
func runMigration(ctx context.Context, d migrationDeps, tenantID, sourceBackend, targetBackend string) error {
	d.metrics.InProgress.Inc()
	defer d.metrics.InProgress.Dec()

	msgs, err := d.source.MessagesForTenant(ctx, tenantID)
	if err != nil {
		return d.markFailed(ctx, tenantID, sourceBackend, targetBackend, "fetch messages", err)
	}
	// Reindex into the target FIRST so reads keep going to the
	// source until the target is fully populated and verified.
	if err := d.flipper.ReindexTo(ctx, tenantID, targetBackend, msgs); err != nil {
		return d.markFailed(ctx, tenantID, sourceBackend, targetBackend, "reindex", err)
	}
	// Validate the freshly-written index is actually queryable
	// before flipping reads onto it. A validation failure leaves
	// the tenant on the source backend (SetBackend hasn't run).
	if err := d.flipper.Validate(ctx, tenantID, targetBackend, msgs); err != nil {
		return d.markFailed(ctx, tenantID, sourceBackend, targetBackend, "validate", err)
	}
	// Target is warm and verified; atomically flip reads over.
	if err := d.flipper.SetBackend(ctx, tenantID, targetBackend); err != nil {
		return d.markFailed(ctx, tenantID, sourceBackend, targetBackend, "set backend", err)
	}
	// SetBackend committed; the tenant is live on the target.
	// MarkCompleted is bookkeeping — a transient failure here must
	// not re-migrate, so retry with backoff and let the next
	// tick's ReconcileCompleted pass promote the row if the budget
	// is exhausted.
	if err := d.markCompletedWithRetry(ctx, tenantID, targetBackend); err != nil {
		d.logger.Printf("search.cutover[%s->%s]: tenant=%s SetBackend OK but MarkCompleted persistently failed; will be reconciled on next tick: %v", sourceBackend, targetBackend, tenantID, err)
		return fmt.Errorf("mark completed: %w", err)
	}
	d.metrics.Completed.Inc()
	d.recordAudit(ctx, tenantID, sourceBackend, targetBackend, "search_cutover_completed", len(msgs), "")
	d.logger.Printf("search.cutover[%s->%s]: tenant=%s migrated %d messages", sourceBackend, targetBackend, tenantID, len(msgs))
	return nil
}

// markFailed flips the row to `failed`, bumps the failure metric,
// emits an audit entry, and returns the wrapped error. The
// MarkFailed store write uses a fresh `now()` so the back-off
// window is measured from the moment of failure.
func (d migrationDeps) markFailed(ctx context.Context, tenantID, sourceBackend, targetBackend, stage string, cause error) error {
	reason := fmt.Sprintf("%s: %v", stage, cause)
	if err := d.store.MarkFailed(ctx, tenantID, targetBackend, reason, d.now()); err != nil {
		d.logger.Printf("search.cutover[%s->%s]: tenant=%s mark failed after %s error: %v", sourceBackend, targetBackend, tenantID, stage, err)
	}
	d.metrics.Failed.Inc()
	d.recordAudit(ctx, tenantID, sourceBackend, targetBackend, "search_cutover_failed", 0, reason)
	return fmt.Errorf("%s: %w", stage, cause)
}

// markCompletedWithRetry retries MarkCompleted with exponential
// backoff up to markCompletedRetries times. Ctx-cancellation
// short-circuits immediately so a pod shutdown isn't delayed by
// the backoff.
func (d migrationDeps) markCompletedWithRetry(ctx context.Context, tenantID, targetBackend string) error {
	var lastErr error
	for attempt := 0; attempt < d.markCompletedRetries; attempt++ {
		if err := d.store.MarkCompleted(ctx, tenantID, targetBackend, d.now()); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < d.markCompletedRetries-1 {
			d.sleep(cutoverMarkCompletedBaseBackoff << attempt)
		}
	}
	return lastErr
}

// recordAudit writes one cutover audit entry. No-op when no
// AuditLogger is wired. Audit failures are logged but never abort
// the cutover — the migration's source of truth is the job row,
// not the audit trail.
func (d migrationDeps) recordAudit(ctx context.Context, tenantID, sourceBackend, targetBackend, action string, messages int, failure string) {
	if d.audit == nil {
		return
	}
	actorType := d.actorType
	if actorType == "" {
		actorType = audit.ActorSystem
	}
	meta := map[string]any{"target_backend": targetBackend}
	if sourceBackend != "" {
		meta["source_backend"] = sourceBackend
	}
	if action == "search_cutover_completed" {
		meta["messages_migrated"] = messages
	}
	if failure != "" {
		meta["error"] = failure
	}
	if _, err := d.audit.Log(ctx, audit.Entry{
		TenantID:     tenantID,
		ActorID:      d.actorID,
		ActorType:    actorType,
		Action:       action,
		ResourceType: "search_backend",
		ResourceID:   targetBackend,
		Metadata:     meta,
	}); err != nil {
		d.logger.Printf("search.cutover[%s->%s]: tenant=%s audit %s: %v", sourceBackend, targetBackend, tenantID, action, err)
	}
}

// cutoverValidationSampleSize bounds how many migrated messages
// Service.Validate searches for in the destination index. Ten is
// enough to catch a wholesale mapping/import failure without
// turning validation into a second full scan.
const cutoverValidationSampleSize = 10

// Validate implements BackendFlipper.Validate. After a reindex it
// samples up to cutoverValidationSampleSize of the just-migrated
// messages and confirms each is searchable in `backend` by a
// free-text query on its subject (falling back to sender). A
// reindex that "succeeded" but produced an empty / unsearchable
// index (mapping drift, silently-dropped bulk import) is caught
// here, BEFORE SetBackend flips reads onto it.
//
// Messages with neither a message ID nor any searchable term are
// skipped — they can't be round-tripped through a query. If
// nothing in the batch is searchable (e.g. an empty mailbox) the
// reindex itself is the only signal and validation passes.
func (s *Service) Validate(ctx context.Context, tenantID, backend string, msgs []Message) error {
	if tenantID == "" || backend == "" {
		return fmt.Errorf("%w: tenantID and backend required", ErrInvalidInput)
	}
	b, ok := s.backends[backend]
	if !ok {
		return fmt.Errorf("%w: backend %q not configured", ErrNotFound, backend)
	}
	searchable := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.MessageID == "" || validationQuery(m) == "" {
			continue
		}
		searchable = append(searchable, m)
	}
	if len(searchable) == 0 {
		return nil
	}
	for _, m := range sampleMessages(searchable, cutoverValidationSampleSize) {
		hits, err := b.SearchMessages(ctx, tenantID, validationQuery(m), 50)
		if err != nil {
			return fmt.Errorf("validate: search in %q: %w", backend, err)
		}
		if !containsMessageID(hits, m.MessageID) {
			return fmt.Errorf("validate: message %q not searchable in %q after reindex", m.MessageID, backend)
		}
	}
	return nil
}

// validationQuery picks the free-text term used to look a message
// back up after reindex: the subject, or the sender when the
// subject is empty.
func validationQuery(m Message) string {
	if m.Subject != "" {
		return m.Subject
	}
	return m.From
}

// sampleMessages returns up to n messages chosen at random (no
// replacement). Random sampling means a corrupt index can't hide
// behind always-checking the same first-N documents.
func sampleMessages(msgs []Message, n int) []Message {
	if len(msgs) <= n {
		return msgs
	}
	perm := mathrand.Perm(len(msgs))
	out := make([]Message, 0, n)
	for _, idx := range perm[:n] {
		out = append(out, msgs[idx])
	}
	return out
}

// containsMessageID reports whether any hit carries the given
// message ID.
func containsMessageID(hits []SearchHit, id string) bool {
	for _, h := range hits {
		if h.MessageID == id {
			return true
		}
	}
	return false
}

// ErrCutoverInProgress is returned by ExecuteCutover when the
// tenant's (tenant, target) row is already `in_progress` — the
// auto-worker or a prior request holds the claim, so the manual
// trigger must not double-run it.
var ErrCutoverInProgress = errors.New("search: cutover already in progress for tenant/target")

// CutoverService is the operator-facing, synchronous counterpart
// to the auto-cutover worker. Where the worker scans the fleet on
// a timer, the service exposes explicit InitiateCutover /
// ExecuteCutover / ListCutoverJobs entry points the admin REST
// surface drives. Both paths share the same migrationDeps machine,
// so a manual cutover validates + flips + audits exactly like the
// automatic one.
type CutoverService struct {
	store     CutoverStore
	sizer     MailboxSizer
	getter    BackendGetter
	audit     AuditLogger
	logger    *log.Logger
	exec      migrationDeps
	threshold int64
	now       func() time.Time
}

// CutoverServiceConfig wires NewCutoverService.
type CutoverServiceConfig struct {
	Store                CutoverStore
	Flipper              BackendFlipper
	Source               MessageSource
	Sizer                MailboxSizer
	Getter               BackendGetter
	Audit                AuditLogger
	Metrics              *CutoverMetrics
	Logger               *log.Logger
	Threshold            int64
	Now                  func() time.Time
	Sleep                func(time.Duration)
	MarkCompletedRetries int
}

// NewCutoverService validates required deps and applies defaults.
func NewCutoverService(cfg CutoverServiceConfig) (*CutoverService, error) {
	if cfg.Store == nil {
		return nil, errors.New("search.NewCutoverService: Store is required")
	}
	if cfg.Flipper == nil {
		return nil, errors.New("search.NewCutoverService: Flipper is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("search.NewCutoverService: Source is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = NewCutoverMetrics(nil)
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultCutoverThreshold
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.MarkCompletedRetries <= 0 {
		cfg.MarkCompletedRetries = defaultCutoverMarkCompletedTries
	}
	return &CutoverService{
		store:     cfg.Store,
		sizer:     cfg.Sizer,
		getter:    cfg.Getter,
		audit:     cfg.Audit,
		logger:    cfg.Logger,
		threshold: cfg.Threshold,
		now:       cfg.Now,
		exec: migrationDeps{
			store:                cfg.Store,
			flipper:              cfg.Flipper,
			source:               cfg.Source,
			audit:                cfg.Audit,
			metrics:              cfg.Metrics,
			logger:               cfg.Logger,
			now:                  cfg.Now,
			sleep:                cfg.Sleep,
			markCompletedRetries: cfg.MarkCompletedRetries,
		},
	}, nil
}

// InitiateCutover records operator intent to migrate `tenantID`
// onto `targetBackend`: it upserts a `pending` job row (resetting
// any prior failed/completed row's back-off) so the tenant becomes
// immediately claimable. It does NOT run the migration — call
// ExecuteCutover to drive it synchronously, or let the auto-worker
// pick the pending row up on its next tick. Returns the resulting
// job. Rejects an unknown backend value and a no-op cutover (the
// tenant is already on the target).
func (c *CutoverService) InitiateCutover(ctx context.Context, tenantID, targetBackend string) (*CutoverJob, error) {
	if tenantID == "" || targetBackend == "" {
		return nil, fmt.Errorf("%w: tenantID and targetBackend required", ErrInvalidInput)
	}
	if !IsValidBackend(targetBackend) {
		return nil, fmt.Errorf("%w: backend %q is not a recognised value", ErrInvalidInput, targetBackend)
	}
	if c.getter != nil {
		cur, err := c.getter.GetBackend(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if cur == targetBackend {
			return nil, fmt.Errorf("%w: tenant already on backend %q", ErrInvalidInput, targetBackend)
		}
	}
	size := c.tenantSize(ctx, tenantID)
	job, err := c.store.UpsertPending(ctx, tenantID, targetBackend, size, c.threshold, c.now())
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ExecuteCutover synchronously runs the migration for an already
// initiated (or auto-discovered) tenant: it claims the (tenant,
// target) row and, if the claim wins, drives the full
// export→reindex→validate→flip→mark dance. A failed claim means
// the row is already `in_progress` or not in a claimable state, so
// it returns ErrCutoverInProgress rather than racing the holder.
// `actorID` attributes the audit trail to the operator who
// triggered the manual cutover.
func (c *CutoverService) ExecuteCutover(ctx context.Context, tenantID, targetBackend, actorID string) error {
	if tenantID == "" || targetBackend == "" {
		return fmt.Errorf("%w: tenantID and targetBackend required", ErrInvalidInput)
	}
	if !IsValidBackend(targetBackend) {
		return fmt.Errorf("%w: backend %q is not a recognised value", ErrInvalidInput, targetBackend)
	}
	sourceBackend := ""
	if c.getter != nil {
		cur, err := c.getter.GetBackend(ctx, tenantID)
		if err != nil {
			return err
		}
		if cur == targetBackend {
			return fmt.Errorf("%w: tenant already on backend %q", ErrInvalidInput, targetBackend)
		}
		sourceBackend = cur
	}
	size := c.tenantSize(ctx, tenantID)
	claimed, err := c.store.Claim(ctx, tenantID, targetBackend, size, c.threshold, c.now())
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !claimed {
		return ErrCutoverInProgress
	}
	// Per-call deps copy so the audit entry is attributed to the
	// triggering operator (ActorAdmin) instead of the system.
	d := c.exec
	d.actorType = audit.ActorAdmin
	d.actorID = actorID
	return runMigration(ctx, d, tenantID, sourceBackend, targetBackend)
}

// ListCutoverJobs returns the tenant's cutover history across all
// targets, most-recent first.
func (c *CutoverService) ListCutoverJobs(ctx context.Context, tenantID string) ([]CutoverJob, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	return c.store.List(ctx, tenantID)
}

// GetCutoverJob returns the single (tenant, target) job row, or
// ErrNotFound if no cutover has ever been initiated for that pair.
// The REST layer uses it to report the terminal state back to the
// operator after a synchronous ExecuteCutover.
func (c *CutoverService) GetCutoverJob(ctx context.Context, tenantID, targetBackend string) (*CutoverJob, error) {
	if tenantID == "" || targetBackend == "" {
		return nil, fmt.Errorf("%w: tenantID and targetBackend required", ErrInvalidInput)
	}
	return c.store.Get(ctx, tenantID, targetBackend)
}

// tenantSize best-effort reads the tenant's mailbox size for the
// job row's bookkeeping. A sizer error is non-fatal — the manual
// path is operator-driven, so we record 0 and proceed rather than
// blocking the cutover on a transient sizer hiccup.
func (c *CutoverService) tenantSize(ctx context.Context, tenantID string) int64 {
	if c.sizer == nil {
		return 0
	}
	size, err := c.sizer.TenantMailboxSize(ctx, tenantID)
	if err != nil {
		c.logger.Printf("search.cutover: tenant=%s size lookup: %v", tenantID, err)
		return 0
	}
	return size
}
