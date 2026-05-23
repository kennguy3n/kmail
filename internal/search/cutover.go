package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
// needs: flip a tenant's backend and bulk-reindex into it. Stating
// the surface explicitly keeps the worker testable (the test
// substitutes a recorder) and decouples the state machine from
// the full Service surface.
type BackendFlipper interface {
	SetBackend(ctx context.Context, tenantID, backend string) error
	Reindex(ctx context.Context, tenantID string, msgs []Message) error
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
	// to be considered eligible — only Meilisearch tenants get
	// promoted to OpenSearch.
	SourceBackend string
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
	// excluding rows that are already `in_progress` or `completed`.
	ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error)
	// Claim transitions a tenant's job row to `in_progress` if no
	// other worker is currently holding it. Must be atomic — the
	// caller relies on a single winner across replicas.
	Claim(ctx context.Context, tenantID string, size, threshold int64, now time.Time) (bool, error)
	// MarkCompleted moves the row to `completed` and resets the
	// failure counter.
	MarkCompleted(ctx context.Context, tenantID string, now time.Time) error
	// MarkFailed moves the row to `failed`, increments
	// `failure_count`, and records `reason`.
	MarkFailed(ctx context.Context, tenantID, reason string, now time.Time) error
}

// CutoverConfig parameterises the auto-cutover worker.
//
//	Threshold:    mailbox-size (bytes) at or above which a tenant
//	              becomes a candidate. Defaults to the per-tenant
//	              equivalent of 100k messages × ~16 KiB / message.
//	Interval:     how often the worker scans for eligible tenants.
//	              Defaults to 1h. The worker also runs once at start.
//	MaxFailures:  after this many consecutive Reindex failures,
//	              the worker leaves the tenant in `failed` state
//	              and stops retrying. Defaults to 5.
//	MaxRetryGap:  the worker won't retry a `failed` job until this
//	              long after its last `updated_at`. Defaults to 1h.
//	              Mostly relevant when a transient OpenSearch
//	              outage causes a wave of failures the worker
//	              shouldn't bash on every tick.
type CutoverConfig struct {
	Pool         *pgxpool.Pool
	Store        CutoverStore
	Service      BackendFlipper
	Sizer        MailboxSizer
	Source       MessageSource
	Logger       *log.Logger
	Threshold    int64
	Interval     time.Duration
	MaxFailures  int
	MaxRetryGap  time.Duration
	// Now is the wall-clock source; defaults to time.Now. Tests
	// inject a fixed clock so retry-backoff is deterministic.
	Now func() time.Time
}

// CutoverWorker auto-promotes tenants from Meilisearch to
// OpenSearch when their mailbox size crosses the configured
// threshold. Run it once per pod via `Run(ctx)` — the worker
// claims tenants atomically through the store so two replicas can
// race the same tick without double-migrating the same tenant.
type CutoverWorker struct {
	cfg CutoverConfig
}

const (
	defaultCutoverThreshold   = 100_000 * 16 * 1024 // ~100k messages × 16 KiB
	defaultCutoverInterval    = time.Hour
	defaultCutoverMaxFailures = 5
	defaultCutoverMaxRetryGap = time.Hour
)

// NewCutoverWorker wires the worker. Validates required deps so a
// misconfiguration surfaces at startup, not on the first tick.
func NewCutoverWorker(cfg CutoverConfig) (*CutoverWorker, error) {
	if cfg.Service == nil {
		return nil, errors.New("search.NewCutoverWorker: Service is required")
	}
	if cfg.Sizer == nil {
		return nil, errors.New("search.NewCutoverWorker: Sizer is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("search.NewCutoverWorker: Source is required")
	}
	if cfg.Store == nil {
		if cfg.Pool == nil {
			return nil, errors.New("search.NewCutoverWorker: Store or Pool is required")
		}
		cfg.Store = NewPostgresCutoverStore(cfg.Pool)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultCutoverThreshold
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultCutoverInterval
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = defaultCutoverMaxFailures
	}
	if cfg.MaxRetryGap <= 0 {
		cfg.MaxRetryGap = defaultCutoverMaxRetryGap
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CutoverWorker{cfg: cfg}, nil
}

// Run drives the worker in a loop until ctx is cancelled. Each
// tick is independent — a failing tick logs and waits out the
// interval rather than terminating the goroutine.
func (w *CutoverWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	// One immediate tick so a pod restart doesn't add up to a
	// full interval of delay on tenants who are already past
	// the threshold.
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick scans for eligible tenants and migrates each one. Exposed
// (capitalised) so tests can drive the worker deterministically
// without spinning the time.Ticker. Errors per tenant are logged
// and counted; they do NOT abort the tick (one bad tenant must not
// stop the rest of the fleet from migrating).
func (w *CutoverWorker) Tick(ctx context.Context) {
	now := w.cfg.Now()
	ids, err := w.cfg.Store.ListCandidates(ctx, CandidateFilter{
		SourceBackend:    BackendMeilisearch,
		MaxFailures:      w.cfg.MaxFailures,
		RetryAfterBefore: now.Add(-w.cfg.MaxRetryGap),
	})
	if err != nil {
		w.cfg.Logger.Printf("search.cutover: list candidates: %v", err)
		return
	}
	for _, tenantID := range ids {
		size, err := w.cfg.Sizer.TenantMailboxSize(ctx, tenantID)
		if err != nil {
			w.cfg.Logger.Printf("search.cutover: sizer tenant=%s: %v", tenantID, err)
			continue
		}
		if size < w.cfg.Threshold {
			continue
		}
		if err := w.cutoverOne(ctx, tenantID, size); err != nil {
			w.cfg.Logger.Printf("search.cutover: tenant %s: %v", tenantID, err)
		}
	}
}

// cutoverOne runs the full cutover dance for a single tenant. The
// state machine is the load-bearing piece: every state transition
// is a separate store write so a crash mid-flight resumes from the
// last persisted state. Concurrent workers race the `Claim` call;
// the loser short-circuits and lets the winner finish.
func (w *CutoverWorker) cutoverOne(ctx context.Context, tenantID string, size int64) error {
	now := w.cfg.Now()
	claimed, err := w.cfg.Store.Claim(ctx, tenantID, size, w.cfg.Threshold, now)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !claimed {
		// Another worker grabbed it; let them finish.
		return nil
	}
	// From here on, every error path MUST flip the row to
	// `failed` so the next tick can retry.
	msgs, fetchErr := w.cfg.Source.MessagesForTenant(ctx, tenantID)
	if fetchErr != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, fmt.Sprintf("fetch messages: %v", fetchErr), w.cfg.Now())
		return fmt.Errorf("fetch messages: %w", fetchErr)
	}
	// Flip the backend column BEFORE the reindex. If Reindex
	// crashes halfway, the tenant ends up on OpenSearch with a
	// partial index — which Reindex re-creates on retry (it does
	// a DeleteIndex first). Doing it in the other order would
	// leave a fully-populated OpenSearch index that's never read
	// (because the backend column still says Meilisearch).
	if err := w.cfg.Service.SetBackend(ctx, tenantID, BackendOpenSearch); err != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, fmt.Sprintf("set backend: %v", err), w.cfg.Now())
		return fmt.Errorf("set backend: %w", err)
	}
	if err := w.cfg.Service.Reindex(ctx, tenantID, msgs); err != nil {
		_ = w.cfg.Store.MarkFailed(ctx, tenantID, fmt.Sprintf("reindex: %v", err), w.cfg.Now())
		return fmt.Errorf("reindex: %w", err)
	}
	if err := w.cfg.Store.MarkCompleted(ctx, tenantID, w.cfg.Now()); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	w.cfg.Logger.Printf("search.cutover: tenant=%s migrated %d messages to OpenSearch", tenantID, len(msgs))
	return nil
}

// PostgresCutoverStore is the default CutoverStore wired against
// Postgres. The schema lives in migration 046.
type PostgresCutoverStore struct {
	pool *pgxpool.Pool
}

// NewPostgresCutoverStore wraps a pool.
func NewPostgresCutoverStore(pool *pgxpool.Pool) *PostgresCutoverStore {
	return &PostgresCutoverStore{pool: pool}
}

// ListCandidates implements CutoverStore.
func (s *PostgresCutoverStore) ListCandidates(ctx context.Context, f CandidateFilter) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text
		FROM tenants t
		LEFT JOIN search_cutover_jobs j ON j.tenant_id = t.id
		WHERE t.search_backend = $1
		  AND (
		      j.tenant_id IS NULL
		      OR (j.cutover_state = 'failed'
		          AND j.failure_count < $2
		          AND j.updated_at < $3)
		  )
	`, f.SourceBackend, f.MaxFailures, f.RetryAfterBefore)
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
// tenant land on a single winner.
func (s *PostgresCutoverStore) Claim(ctx context.Context, tenantID string, size, threshold int64, now time.Time) (bool, error) {
	var claimed bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_cutover_jobs (tenant_id, cutover_state, mailbox_size, threshold, started_at, updated_at)
			VALUES ($1, 'pending', $2, $3, NULL, $4)
			ON CONFLICT (tenant_id) DO NOTHING
		`, tenantID, size, threshold, now)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE search_cutover_jobs
			   SET cutover_state = 'in_progress',
			       mailbox_size  = $2,
			       started_at    = $3,
			       updated_at    = $3
			 WHERE tenant_id = $1
			   AND cutover_state IN ('pending', 'failed')
		`, tenantID, size, now)
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

// MarkCompleted implements CutoverStore.
func (s *PostgresCutoverStore) MarkCompleted(ctx context.Context, tenantID string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'completed',
		       completed_at  = $2,
		       updated_at    = $2,
		       failure_count = 0,
		       last_error    = ''
		 WHERE tenant_id = $1
	`, tenantID, now)
	return err
}

// MarkFailed implements CutoverStore.
func (s *PostgresCutoverStore) MarkFailed(ctx context.Context, tenantID, reason string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE search_cutover_jobs
		   SET cutover_state = 'failed',
		       failure_count = failure_count + 1,
		       last_error    = $2,
		       updated_at    = $3
		 WHERE tenant_id = $1
	`, tenantID, reason, now)
	return err
}
