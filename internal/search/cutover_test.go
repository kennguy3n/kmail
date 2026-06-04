package search

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/kmail/internal/audit"
)

// inMemoryCutoverStore exercises the worker's state machine
// without a real Postgres. It mirrors the production schema
// closely: a row per (tenant, target_backend) with state, failure
// count, and updated_at, plus a per-tenant `search_backend` value
// to mirror the production SQL JOIN against the `tenants` table.
// The composite key matches the baseline schema — a single tenant can
// carry multiple rows, one per target_backend, so cross-transition
// state is fully isolated. SourceBackend / TargetBackend filters
// are honoured exactly the same way as the Postgres impl so a test
// failure here mirrors a production regression.
type inMemoryCutoverStore struct {
	mu sync.Mutex
	// rows is the search_cutover_jobs equivalent — one entry per
	// (tenantID, targetBackend) that's ever been claimed.
	rows map[memRowKey]*memRow
	// tenantBackends mirrors `tenants.search_backend` so the
	// store can filter by SourceBackend exactly like the
	// production SQL JOIN does. Tests that need to simulate a
	// SetBackend mid-cutover update this map directly via
	// flipBackend().
	tenantBackends map[string]string
}

// memRowKey is the in-memory equivalent of the (tenant_id,
// target_backend) composite PK on `search_cutover_jobs`. Tests
// that pre-date the composite key used `string` keys; the
// `rowByTenant` helper preserves the old single-row-per-tenant
// assertion ergonomics for tests that only ever exercise the
// default `meilisearch -> opensearch` transition.
type memRowKey struct {
	tenantID      string
	targetBackend string
}

type memRow struct {
	state        CutoverState
	mailboxSize  int64
	threshold    int64
	startedAt    *time.Time
	completedAt  *time.Time
	failureCount int
	lastError    string
	updatedAt    time.Time
}

// newInMemoryStore seeds every tenant on Meilisearch (matching the
// pre-cutover state). Callers that want to simulate a tenant
// already on OpenSearch update the map afterwards.
func newInMemoryStore(tenants []string) *inMemoryCutoverStore {
	backends := make(map[string]string, len(tenants))
	for _, id := range tenants {
		backends[id] = BackendMeilisearch
	}
	return &inMemoryCutoverStore{
		rows:           map[memRowKey]*memRow{},
		tenantBackends: backends,
	}
}

// rowByTenant is a convenience helper that returns the FIRST row
// matching `tenantID` regardless of target_backend. Tests that
// only drive the default `meilisearch -> opensearch` transition
// can use this to keep assertions terse — there's exactly one
// row in the store for each tenant in that scenario. Tests that
// exercise multi-transition behaviour should index `rows` by an
// explicit `memRowKey{tenantID, targetBackend}` instead.
func (s *inMemoryCutoverStore) rowByTenant(tenantID string) *memRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range s.rows {
		if k.tenantID == tenantID {
			return r
		}
	}
	return nil
}

// rowFor returns the row keyed by the explicit (tenant, target)
// pair, or nil if none exists. Used by multi-transition tests.
func (s *inMemoryCutoverStore) rowFor(tenantID, targetBackend string) *memRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[memRowKey{tenantID: tenantID, targetBackend: targetBackend}]
}

// flipBackend mirrors production's SetBackend write so the test
// store stays consistent with what the production SQL JOIN would
// see after the worker calls Service.SetBackend.
func (s *inMemoryCutoverStore) flipBackend(tenantID, backend string) {
	s.mu.Lock()
	s.tenantBackends[tenantID] = backend
	s.mu.Unlock()
}

func (s *inMemoryCutoverStore) ListCandidates(_ context.Context, f CandidateFilter) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, backend := range s.tenantBackends {
		// SourceBackend filter — mirrors `WHERE
		// t.search_backend = $1` in the production SQL.
		if f.SourceBackend != "" && backend != f.SourceBackend {
			continue
		}
		// TargetBackend scopes the LEFT JOIN — a row for a
		// different target leaves the tenant eligible.
		r, ok := s.rows[memRowKey{tenantID: id, targetBackend: f.TargetBackend}]
		switch {
		case !ok:
			ids = append(ids, id)
		case r.state == CutoverFailed && r.failureCount < f.MaxFailures && r.updatedAt.Before(f.RetryAfterBefore):
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *inMemoryCutoverStore) Claim(_ context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memRowKey{tenantID: tenantID, targetBackend: targetBackend}
	r, ok := s.rows[k]
	if !ok {
		r = &memRow{state: CutoverPending, mailboxSize: size, threshold: threshold, updatedAt: now}
		s.rows[k] = r
	}
	if r.state != CutoverPending && r.state != CutoverFailed {
		return false, nil
	}
	r.state = CutoverInProgress
	r.mailboxSize = size
	r.startedAt = &now
	r.updatedAt = now
	return true, nil
}

func (s *inMemoryCutoverStore) MarkCompleted(_ context.Context, tenantID, targetBackend string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[memRowKey{tenantID: tenantID, targetBackend: targetBackend}]
	if !ok {
		return errors.New("no row")
	}
	r.state = CutoverCompleted
	r.completedAt = &now
	r.updatedAt = now
	r.failureCount = 0
	r.lastError = ""
	return nil
}

func (s *inMemoryCutoverStore) MarkFailed(_ context.Context, tenantID, targetBackend, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[memRowKey{tenantID: tenantID, targetBackend: targetBackend}]
	if !ok {
		return errors.New("no row")
	}
	r.state = CutoverFailed
	r.failureCount++
	r.lastError = reason
	r.updatedAt = now
	return nil
}

// ReconcileCompleted mirrors the production Postgres impl: walk
// the rows, promote any `in_progress` row whose tenant is already
// on `targetBackend` AND whose row keys to the same target AND
// whose updated_at predates `before`. The completion timestamp is
// the caller-supplied `now` so the test's injected clock
// (CutoverConfig.Now) drives both the SQL path and the in-memory
// test double identically.
func (s *inMemoryCutoverStore) ReconcileCompleted(_ context.Context, targetBackend string, before, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, r := range s.rows {
		if k.targetBackend != targetBackend {
			continue
		}
		if r.state != CutoverInProgress {
			continue
		}
		if !r.updatedAt.Before(before) {
			continue
		}
		if s.tenantBackends[k.tenantID] != targetBackend {
			continue
		}
		r.state = CutoverCompleted
		r.completedAt = &now
		r.updatedAt = now
		r.failureCount = 0
		r.lastError = ""
		n++
	}
	return n, nil
}

// ReconcileStale is the mirror of ReconcileCompleted: it demotes
// stale `in_progress` rows whose tenant is still on `sourceBackend`
// AND whose row keys to `targetBackend` back to `failed`.
// Increments `failureCount` and stamps a synthetic `lastError` so
// the test can assert the recovery hook fired exactly the same way
// the Postgres impl does.
func (s *inMemoryCutoverStore) ReconcileStale(_ context.Context, sourceBackend, targetBackend string, before, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, r := range s.rows {
		if k.targetBackend != targetBackend {
			continue
		}
		if r.state != CutoverInProgress {
			continue
		}
		if !r.updatedAt.Before(before) {
			continue
		}
		if s.tenantBackends[k.tenantID] != sourceBackend {
			continue
		}
		r.state = CutoverFailed
		r.failureCount++
		r.lastError = "stale in_progress: assumed crash recovery"
		r.updatedAt = now
		n++
	}
	return n, nil
}

// toJob projects a memRow into the CutoverJob shape the store's
// Get / List / UpsertPending readers return, synthesising the
// created_at the in-memory model doesn't track separately.
func (r *memRow) toJob(tenantID, targetBackend string) CutoverJob {
	return CutoverJob{
		TenantID:      tenantID,
		TargetBackend: targetBackend,
		State:         r.state,
		MailboxSize:   r.mailboxSize,
		Threshold:     r.threshold,
		StartedAt:     r.startedAt,
		CompletedAt:   r.completedAt,
		FailureCount:  r.failureCount,
		LastError:     r.lastError,
		CreatedAt:     r.updatedAt,
		UpdatedAt:     r.updatedAt,
	}
}

// UpsertPending mirrors the Postgres impl: leave an in_progress
// row entirely untouched (including updatedAt, so ReconcileStale's
// crash-recovery clock keeps running); otherwise (re)set the row to
// pending with refreshed size/threshold and a cleared back-off so
// the manual trigger overrides the failure / completed guards.
func (s *inMemoryCutoverStore) UpsertPending(_ context.Context, tenantID, targetBackend string, size, threshold int64, now time.Time) (*CutoverJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memRowKey{tenantID: tenantID, targetBackend: targetBackend}
	r, ok := s.rows[k]
	if !ok {
		r = &memRow{state: CutoverPending, mailboxSize: size, threshold: threshold, updatedAt: now}
		s.rows[k] = r
		j := r.toJob(tenantID, targetBackend)
		return &j, nil
	}
	if r.state == CutoverInProgress {
		j := r.toJob(tenantID, targetBackend)
		return &j, nil
	}
	r.state = CutoverPending
	r.failureCount = 0
	r.lastError = ""
	r.mailboxSize = size
	r.threshold = threshold
	r.updatedAt = now
	j := r.toJob(tenantID, targetBackend)
	return &j, nil
}

// Get mirrors the Postgres impl, returning ErrNotFound for a
// missing (tenant, target) row.
func (s *inMemoryCutoverStore) Get(_ context.Context, tenantID, targetBackend string) (*CutoverJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[memRowKey{tenantID: tenantID, targetBackend: targetBackend}]
	if !ok {
		return nil, ErrNotFound
	}
	j := r.toJob(tenantID, targetBackend)
	return &j, nil
}

// List returns every row for a tenant. Order is unspecified in the
// in-memory model (map iteration); callers that assert ordering
// should use the Postgres impl.
func (s *inMemoryCutoverStore) List(_ context.Context, tenantID string) ([]CutoverJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []CutoverJob
	for k, r := range s.rows {
		if k.tenantID != tenantID {
			continue
		}
		jobs = append(jobs, r.toJob(k.tenantID, k.targetBackend))
	}
	return jobs, nil
}

// fakeFlipper records ReindexTo / SetBackend invocations so tests
// can assert what the worker drove and inject per-tenant failures.
// SetBackend writes back to the linked inMemoryCutoverStore so the
// store's tenantBackends map stays consistent — that mirrors the
// production behavior where `Service.SetBackend` updates the
// `tenants.search_backend` column that `ListCandidates` filters on.
type fakeFlipper struct {
	mu             sync.Mutex
	store          *inMemoryCutoverStore
	setBackendCall map[string]string
	reindexCall    map[string]reindexCallRecord
	setBackendErr  map[string]error
	reindexErr     map[string]error
	// validateErr injects a per-tenant validation failure so
	// tests can assert a reindex that "succeeded" but produced an
	// unsearchable index leaves the tenant on the source backend.
	validateErr map[string]error
	// validateCall records the (backend, count) Validate saw so
	// tests can confirm validation ran AFTER reindex and BEFORE
	// the backend flip.
	validateCall map[string]reindexCallRecord
}

type reindexCallRecord struct {
	backend string
	count   int
}

func newFakeFlipper(store *inMemoryCutoverStore) *fakeFlipper {
	return &fakeFlipper{
		store:          store,
		setBackendCall: map[string]string{},
		reindexCall:    map[string]reindexCallRecord{},
		setBackendErr:  map[string]error{},
		reindexErr:     map[string]error{},
		validateErr:    map[string]error{},
		validateCall:   map[string]reindexCallRecord{},
	}
}

// Validate records the call and returns any injected per-tenant
// error. It does NOT write back to the store, so a validation
// failure leaves `tenantBackends` (the source backend) untouched —
// exactly the safety property the cutover guarantees.
func (f *fakeFlipper) Validate(_ context.Context, tenantID, backend string, msgs []Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.validateErr[tenantID]; err != nil {
		return err
	}
	f.validateCall[tenantID] = reindexCallRecord{backend: backend, count: len(msgs)}
	return nil
}

func (f *fakeFlipper) SetBackend(_ context.Context, tenantID, backend string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.setBackendErr[tenantID]; err != nil {
		return err
	}
	f.setBackendCall[tenantID] = backend
	if f.store != nil {
		f.store.flipBackend(tenantID, backend)
	}
	return nil
}

func (f *fakeFlipper) ReindexTo(_ context.Context, tenantID, backend string, msgs []Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reindexErr[tenantID]; err != nil {
		return err
	}
	f.reindexCall[tenantID] = reindexCallRecord{backend: backend, count: len(msgs)}
	return nil
}

// silentLogger discards all worker log output so the test runner
// stays clean.
func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func buildWorker(t *testing.T, store CutoverStore, flipper BackendFlipper, sizer MailboxSizer, source MessageSource, threshold int64, now func() time.Time) *CutoverWorker {
	t.Helper()
	w, err := NewCutoverWorker(CutoverConfig{
		Store:       store,
		Service:     flipper,
		Sizer:       sizer,
		Source:      source,
		Logger:      silentLogger(),
		Threshold:   threshold,
		Interval:    time.Hour,
		MaxFailures: 5,
		MaxRetryGap: time.Hour,
		Now:         now,
		// Tests don't want to wait for the bounded retry backoff
		// inside markCompletedWithRetry; the production default
		// (time.Sleep) is exercised separately by the dedicated
		// retry test below.
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}
	return w
}

// TestCutoverWorker_BelowThresholdNoCutover pins the headline
// guard: a tenant whose mailbox hasn't grown past the threshold
// stays on Meilisearch and the worker emits no side effects.
func TestCutoverWorker_BelowThresholdNoCutover(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 1024, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())
	if got := flipper.setBackendCall["tenant-a"]; got != "" {
		t.Fatalf("SetBackend called for tenant-a: %q; want no-op", got)
	}
	if r := store.rowByTenant("tenant-a"); r != nil {
		t.Fatalf("under-threshold tenant got a state row: %+v", r)
	}
}

// TestCutoverWorker_AboveThresholdFullCutover walks a happy-path
// migration: oversized tenant → reindex into OpenSearch → backend
// flipped → row marked completed. Crucially verifies the order
// of operations (ReindexTo BEFORE SetBackend).
func TestCutoverWorker_AboveThresholdFullCutover(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	msgs := []Message{
		{TenantID: "tenant-a", MessageID: "m1"},
		{TenantID: "tenant-a", MessageID: "m2"},
	}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) { return msgs, nil })
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())

	rec := flipper.reindexCall["tenant-a"]
	if rec.backend != BackendOpenSearch || rec.count != 2 {
		t.Fatalf("ReindexTo tenant-a = %+v, want {opensearch, 2}", rec)
	}
	if got := flipper.setBackendCall["tenant-a"]; got != BackendOpenSearch {
		t.Fatalf("SetBackend tenant-a = %q, want %q", got, BackendOpenSearch)
	}
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverCompleted {
		t.Fatalf("row state = %+v, want CutoverCompleted", r)
	}
	if store.tenantBackends["tenant-a"] != BackendOpenSearch {
		t.Fatalf("tenant backend = %q, want opensearch", store.tenantBackends["tenant-a"])
	}
}

// TestCutoverWorker_ReindexFailureMarksFailedAndRetries verifies
// the failure path: when Reindex errors, the row goes to `failed`
// and a later tick (after the retry window) picks it up again.
func TestCutoverWorker_ReindexFailureMarksFailedAndRetries(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	flipper.reindexErr["tenant-a"] = errors.New("opensearch 502")
	clock := &fakeNow{now: time.Unix(1_700_000_000, 0)}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a"}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, clock.Now)
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverFailed || r.failureCount != 1 {
		t.Fatalf("after failed reindex row = %+v, want failed/1", r)
	}

	// Immediately re-tick inside the back-off window: should NOT
	// retry (failure_count still 1).
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r.failureCount != 1 {
		t.Fatalf("back-off ignored: failureCount = %d, want 1", r.failureCount)
	}

	// Advance past the back-off window AND clear the underlying
	// error so the retry succeeds.
	clock.Advance(2 * time.Hour)
	delete(flipper.reindexErr, "tenant-a")
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r.state != CutoverCompleted {
		t.Fatalf("retry did not succeed: state = %s", r.state)
	}
}

// TestCutoverWorker_MaxFailuresGivesUp verifies the worker stops
// retrying once `failure_count` reaches `MaxFailures` — a tenant
// in failed state with the cap reached is excluded from the
// candidate list.
func TestCutoverWorker_MaxFailuresGivesUp(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	flipper.reindexErr["tenant-a"] = errors.New("permanent error")
	clock := &fakeNow{now: time.Unix(1_700_000_000, 0)}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a"}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, clock.Now)

	// Drive 5 ticks past the back-off; expect failure_count to
	// cap at 5 and no further attempts.
	for i := 0; i < 5; i++ {
		w.Tick(context.Background())
		clock.Advance(2 * time.Hour)
	}
	if got := store.rowByTenant("tenant-a").failureCount; got != 5 {
		t.Fatalf("failureCount = %d, want exactly 5", got)
	}

	// 6th tick: the candidate filter excludes this tenant.
	w.Tick(context.Background())
	if got := store.rowByTenant("tenant-a").failureCount; got != 5 {
		t.Fatalf("worker retried past MaxFailures: count = %d", got)
	}
}

// TestCutoverWorker_ConcurrentClaimsExactlyOne sanity-checks the
// store's atomic-claim invariant: two workers running Tick at the
// same instant each call Claim on the same tenant, but only one
// gets through to SetBackend / Reindex.
func TestCutoverWorker_ConcurrentClaimsExactlyOne(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	// Block ReindexTo on a channel so the first claim holds the
	// in_progress state long enough for the second worker to
	// race.
	gate := make(chan struct{})
	var inFlight sync.WaitGroup
	inFlight.Add(1)
	flipper2 := &gatedFlipper{
		inner: flipper,
		gate:  gate,
		hit:   make(chan struct{}, 1),
	}
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) { return []Message{{}}, nil })
	w := buildWorker(t, store, flipper2, sizer, source, 100_000, func() time.Time { return now })
	go func() {
		defer inFlight.Done()
		w.Tick(context.Background())
	}()
	// Wait for the first goroutine to have claimed the tenant
	// and entered ReindexTo.
	<-flipper2.hit
	// Second concurrent tick — must observe `in_progress` and
	// short-circuit without calling SetBackend again.
	beforeBackend := flipper.setBackendCall["tenant-a"]
	w.Tick(context.Background())
	afterBackend := flipper.setBackendCall["tenant-a"]
	if beforeBackend != afterBackend {
		t.Fatal("concurrent tick double-flipped SetBackend")
	}
	close(gate)
	inFlight.Wait()
}

type gatedFlipper struct {
	inner *fakeFlipper
	gate  chan struct{}
	hit   chan struct{}
}

func (g *gatedFlipper) SetBackend(ctx context.Context, tenantID, backend string) error {
	return g.inner.SetBackend(ctx, tenantID, backend)
}

func (g *gatedFlipper) Validate(ctx context.Context, tenantID, backend string, msgs []Message) error {
	return g.inner.Validate(ctx, tenantID, backend, msgs)
}

func (g *gatedFlipper) ReindexTo(ctx context.Context, tenantID, backend string, msgs []Message) error {
	select {
	case g.hit <- struct{}{}:
	default:
	}
	<-g.gate
	return g.inner.ReindexTo(ctx, tenantID, backend, msgs)
}

// TestCutoverWorker_MultipleTenants verifies the worker iterates
// the candidate list and migrates each independently — one
// tenant's failure must not stop the next from migrating.
func TestCutoverWorker_MultipleTenants(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a", "tenant-b", "tenant-c"})
	flipper := newFakeFlipper(store)
	flipper.reindexErr["tenant-b"] = errors.New("transient")
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(_ context.Context, id string) (int64, error) {
		if id == "tenant-c" {
			return 1024, nil // below threshold — skipped
		}
		return 200_000, nil
	})
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverCompleted {
		t.Errorf("tenant-a state = %v, want completed", r)
	}
	if r := store.rowByTenant("tenant-b"); r == nil || r.state != CutoverFailed {
		t.Errorf("tenant-b state = %v, want failed", r)
	}
	if r := store.rowByTenant("tenant-c"); r != nil {
		t.Errorf("tenant-c got a row despite being below threshold: %+v", r)
	}
}

// TestInMemoryCutoverStore_ListCandidatesFiltersByBackend is the
// targeted unit test for the SourceBackend filter the test store
// previously ignored. Without this filter, a tenant whose backend
// was already flipped would still be reported as a candidate
// (because the store had no concept of which backend the tenant
// is on) and the test suite would never catch the bug where the
// worker flipped the column too eagerly.
func TestInMemoryCutoverStore_ListCandidatesFiltersByBackend(t *testing.T) {
	store := newInMemoryStore([]string{"on-meilisearch", "on-opensearch"})
	store.flipBackend("on-opensearch", BackendOpenSearch)

	now := time.Unix(1_700_000_000, 0)
	ids, err := store.ListCandidates(context.Background(), CandidateFilter{
		SourceBackend:    BackendMeilisearch,
		TargetBackend:    BackendOpenSearch,
		MaxFailures:      5,
		RetryAfterBefore: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(ids) != 1 || ids[0] != "on-meilisearch" {
		t.Fatalf("ids = %v, want [on-meilisearch]", ids)
	}

	// Empty SourceBackend disables the filter — returns both.
	// Tests still pass a TargetBackend so the LEFT JOIN scope is
	// well-defined; with no rows in the store, every tenant
	// passing the SourceBackend filter is a candidate.
	ids, err = store.ListCandidates(context.Background(), CandidateFilter{
		TargetBackend:    BackendOpenSearch,
		MaxFailures:      5,
		RetryAfterBefore: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want both tenants", ids)
	}
}

// TestCutoverWorker_NewConfigValidates pins required-field
// validation at construction time.
func TestCutoverWorker_NewConfigValidates(t *testing.T) {
	_, err := NewCutoverWorker(CutoverConfig{})
	if err == nil {
		t.Fatal("expected error on empty CutoverConfig")
	}
	store := newInMemoryStore(nil)
	cases := []struct {
		name string
		cfg  CutoverConfig
	}{
		{"missing service", CutoverConfig{Store: store, Sizer: MailboxSizerFunc(func(context.Context, string) (int64, error) { return 0, nil }), Source: MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })}},
		{"missing sizer", CutoverConfig{Store: store, Service: newFakeFlipper(store), Source: MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })}},
		{"missing source", CutoverConfig{Store: store, Service: newFakeFlipper(store), Sizer: MailboxSizerFunc(func(context.Context, string) (int64, error) { return 0, nil })}},
		{"missing store and pool", CutoverConfig{Service: newFakeFlipper(store), Sizer: MailboxSizerFunc(func(context.Context, string) (int64, error) { return 0, nil }), Source: MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewCutoverWorker(c.cfg); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestCutoverWorker_SetBackendFailureLeavesTenantRetryable is the
// regression test for the original bug: if SetBackend failed
// AFTER ReindexTo succeeded, the tenant must remain visible to
// `ListCandidates` so the next tick can retry. ReindexTo is
// idempotent (DeleteIndex first), so the retry safely re-fills
// the destination index and re-tries SetBackend.
func TestCutoverWorker_SetBackendFailureLeavesTenantRetryable(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	flipper.setBackendErr["tenant-a"] = errors.New("transient DB error")
	clock := &fakeNow{now: time.Unix(1_700_000_000, 0)}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a"}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, clock.Now)
	w.Tick(context.Background())
	// SetBackend failed → row marked failed; tenant
	// backend column is STILL meilisearch (the worker never got
	// to call SetBackend successfully), so the next tick can
	// see the tenant under the SourceBackend=meilisearch filter.
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverFailed {
		t.Fatalf("row = %+v, want failed", r)
	}
	if store.tenantBackends["tenant-a"] != BackendMeilisearch {
		t.Fatalf("tenant backend = %q, want meilisearch (failed SetBackend must not flip)", store.tenantBackends["tenant-a"])
	}
	// Sanity: candidate filter still picks up the tenant.
	ids, _ := store.ListCandidates(context.Background(), CandidateFilter{
		SourceBackend:    BackendMeilisearch,
		TargetBackend:    BackendOpenSearch,
		MaxFailures:      5,
		RetryAfterBefore: clock.Now().Add(time.Hour),
	})
	if len(ids) != 1 || ids[0] != "tenant-a" {
		t.Fatalf("candidate list = %v, want [tenant-a]", ids)
	}

	// Clear the SetBackend error, advance past the back-off
	// window, retry. The reindex is idempotent — the worker
	// wipes & re-fills OpenSearch, then SetBackend succeeds.
	clock.Advance(2 * time.Hour)
	delete(flipper.setBackendErr, "tenant-a")
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r.state != CutoverCompleted {
		t.Fatalf("retry did not succeed: state = %s", r.state)
	}
}

// TestCutoverWorker_MarkCompletedTransientFailureRetries verifies
// the retry path: when MarkCompleted fails for the first N-1
// attempts and succeeds on the Nth, the row ends up in
// `completed`. This is the headline Postgres-blip recovery case.
func TestCutoverWorker_MarkCompletedTransientFailureRetries(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)

	// Wrap the store to inject N-1 transient MarkCompleted
	// failures. The Nth call passes through to the real store.
	wrapped := &flakeyMarkCompleted{CutoverStore: store, failuresLeft: 2}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:                wrapped,
		Service:              flipper,
		Sizer:                sizer,
		Source:               source,
		Logger:               silentLogger(),
		Threshold:            100_000,
		Interval:             time.Hour,
		MaxFailures:          5,
		MaxRetryGap:          time.Hour,
		Now:                  func() time.Time { return now },
		Sleep:                func(time.Duration) {},
		MarkCompletedRetries: 3,
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverCompleted {
		t.Fatalf("row = %+v, want completed", r)
	}
	if wrapped.calls != 3 {
		t.Fatalf("MarkCompleted called %d times, want 3 (retries exhausted twice)", wrapped.calls)
	}
}

// TestCutoverWorker_MarkCompletedPersistentFailureReconcilesNextTick
// verifies the safety net: when MarkCompleted fails on every
// retry, the row sits in `in_progress` after the tick — but the
// NEXT tick's ReconcileCompleted pass promotes it to `completed`
// because the tenant is already on OpenSearch (SetBackend
// succeeded).
func TestCutoverWorker_MarkCompletedPersistentFailureReconcilesNextTick(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	clock := &fakeNow{now: time.Unix(1_700_000_000, 0)}

	// Always fail MarkCompleted in the first tick. After the
	// tick we'll clear the flake so reconciliation can run
	// cleanly.
	wrapped := &flakeyMarkCompleted{CutoverStore: store, failuresLeft: 999}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:                wrapped,
		Service:              flipper,
		Sizer:                sizer,
		Source:               source,
		Logger:               silentLogger(),
		Threshold:            100_000,
		Interval:             time.Hour,
		MaxFailures:          5,
		MaxRetryGap:          time.Hour,
		ReconcileAfter:       30 * time.Minute,
		MarkCompletedRetries: 3,
		Now:                  clock.Now,
		Sleep:                func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}

	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverInProgress {
		t.Fatalf("after MarkCompleted exhaust, row = %+v, want in_progress", r)
	}
	if store.tenantBackends["tenant-a"] != BackendOpenSearch {
		t.Fatalf("tenant backend = %q, want opensearch (SetBackend ran)", store.tenantBackends["tenant-a"])
	}

	// Drop the flake so the reconciler can stick.
	wrapped.failuresLeft = 0
	// Advance past the ReconcileAfter window so the row is
	// eligible for promotion.
	clock.Advance(time.Hour)

	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverCompleted {
		t.Fatalf("after reconcile tick, row = %+v, want completed", r)
	}
}

// TestCutoverWorker_CrashDuringReindexRecoversViaReconcileStale
// pins the recovery path for the unlucky case where a pod is
// SIGKILLed (OOM, node failure, preemption) AFTER Claim but BEFORE
// either SetBackend or MarkFailed has a chance to run. Without
// ReconcileStale the row sits in `in_progress` forever — neither
// ListCandidates (which excludes `in_progress`) nor
// ReconcileCompleted (which only fires when the tenant is already
// on the target backend) recovers it.
//
// The test simulates the crash by claiming the row manually
// (bypassing the worker) and then advancing the clock past the
// reconciliation horizon. The next Tick must demote the row to
// `failed` so the normal `ListCandidates` retry path can pick it
// up. A follow-up Tick (with a clear MailboxSizer / Source) then
// completes the cutover normally — proving the recovery actually
// reopens the retry path.
func TestCutoverWorker_CrashDuringReindexRecoversViaReconcileStale(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	clock := &fakeNow{now: time.Unix(1_700_000_000, 0)}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:                store,
		Service:              flipper,
		Sizer:                sizer,
		Source:               source,
		Logger:               silentLogger(),
		Threshold:            100_000,
		Interval:             time.Hour,
		MaxFailures:          5,
		MaxRetryGap:          time.Minute, // small so retry is eligible after the advance
		ReconcileAfter:       30 * time.Minute,
		MarkCompletedRetries: 3,
		Now:                  clock.Now,
		Sleep:                func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}

	// Simulate the crash: claim the row but don't let the
	// worker run. The row is now `in_progress`, tenant still on
	// `meilisearch`, no follow-up MarkFailed/MarkCompleted.
	if _, err := store.Claim(context.Background(), "tenant-a", BackendOpenSearch, 200_000, 100_000, clock.Now()); err != nil {
		t.Fatalf("manual Claim: %v", err)
	}
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverInProgress {
		t.Fatalf("after simulated crash, row = %+v, want in_progress", r)
	}
	if store.tenantBackends["tenant-a"] != BackendMeilisearch {
		t.Fatalf("tenant backend = %q, want meilisearch (no SetBackend yet)", store.tenantBackends["tenant-a"])
	}

	// Before the reconciliation horizon, Tick must NOT demote
	// the row — it could be a legitimate in-flight migration
	// on another worker.
	w.Tick(context.Background())
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverInProgress {
		t.Fatalf("pre-horizon Tick wrongly mutated row: %+v, want still in_progress", r)
	}

	// Cross the horizon; ReconcileStale must demote the row to
	// `failed` so the normal retry path can pick it up.
	clock.Advance(time.Hour)
	w.Tick(context.Background())
	r := store.rowByTenant("tenant-a")
	if r == nil || r.state != CutoverFailed {
		t.Fatalf("post-horizon Tick row = %+v, want failed (demoted by ReconcileStale)", r)
	}
	if r.failureCount != 1 {
		t.Fatalf("post-horizon failure_count = %d, want 1 (incremented by ReconcileStale)", r.failureCount)
	}
	if r.lastError != "stale in_progress: assumed crash recovery" {
		t.Fatalf("post-horizon last_error = %q, want synthetic crash-recovery reason", r.lastError)
	}

	// Advance past MaxRetryGap so ListCandidates re-considers
	// the failed row; the next Tick should complete the cutover.
	clock.Advance(2 * time.Minute)
	w.Tick(context.Background())
	r = store.rowByTenant("tenant-a")
	if r == nil || r.state != CutoverCompleted {
		t.Fatalf("recovery Tick row = %+v, want completed", r)
	}
	if store.tenantBackends["tenant-a"] != BackendOpenSearch {
		t.Fatalf("recovery Tick tenant backend = %q, want opensearch", store.tenantBackends["tenant-a"])
	}
}

// flakeyMarkCompleted wraps a real CutoverStore and injects
// transient MarkCompleted failures up to `failuresLeft`. All
// other methods pass through unchanged so the in-memory store
// stays the source of truth for the rest of the state machine.
type flakeyMarkCompleted struct {
	CutoverStore
	failuresLeft int
	calls        int
}

func (s *flakeyMarkCompleted) MarkCompleted(ctx context.Context, tenantID, targetBackend string, now time.Time) error {
	s.calls++
	if s.failuresLeft > 0 {
		s.failuresLeft--
		return errors.New("transient: postgres conn reset")
	}
	return s.CutoverStore.MarkCompleted(ctx, tenantID, targetBackend, now)
}

type fakeNow struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeNow) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeNow) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// TestCutoverWorker_MultiTransitionRunsAllConfiguredPairs is the
// architectural pin that the worker handles ≥2 (source, target)
// pairs in a single Tick. Without this guard the worker silently
// drops back to a single pair (legacy meilisearch->opensearch)
// and tenants on the shared variant stagnate forever.
//
// Setup:
//   - tenant-legacy is on `meilisearch` with a 200k mailbox.
//   - tenant-shared is on `shared_meilisearch` with a 200k mailbox.
//
// One Tick using DefaultCutoverTransitions must promote both:
// the first transition flips tenant-legacy to `opensearch`, the
// second flips tenant-shared to `shared_opensearch`.
func TestCutoverWorker_MultiTransitionRunsAllConfiguredPairs(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-legacy", "tenant-shared"})
	store.flipBackend("tenant-shared", BackendSharedMeilisearch)
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) {
		return 200_000, nil
	})
	source := MessageSourceFunc(func(_ context.Context, tenantID string) ([]Message, error) {
		return []Message{{TenantID: tenantID, MessageID: tenantID + "-m1"}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())

	// tenant-legacy: meilisearch -> opensearch (transition 1).
	if got := store.tenantBackends["tenant-legacy"]; got != BackendOpenSearch {
		t.Errorf("tenant-legacy backend = %q, want %q", got, BackendOpenSearch)
	}
	if rec := flipper.reindexCall["tenant-legacy"]; rec.backend != BackendOpenSearch {
		t.Errorf("tenant-legacy reindex target = %q, want %q", rec.backend, BackendOpenSearch)
	}
	if r := store.rowByTenant("tenant-legacy"); r == nil || r.state != CutoverCompleted {
		t.Errorf("tenant-legacy row state = %+v, want CutoverCompleted", r)
	}

	// tenant-shared: shared_meilisearch -> shared_opensearch
	// (transition 2). The worker must NOT promote it to plain
	// `opensearch` — that would silently strip tenant isolation.
	if got := store.tenantBackends["tenant-shared"]; got != BackendSharedOpenSearch {
		t.Errorf("tenant-shared backend = %q, want %q", got, BackendSharedOpenSearch)
	}
	if rec := flipper.reindexCall["tenant-shared"]; rec.backend != BackendSharedOpenSearch {
		t.Errorf("tenant-shared reindex target = %q, want %q", rec.backend, BackendSharedOpenSearch)
	}
	if r := store.rowByTenant("tenant-shared"); r == nil || r.state != CutoverCompleted {
		t.Errorf("tenant-shared row state = %+v, want CutoverCompleted", r)
	}
}

// TestCutoverWorker_CustomTransitionsOverride pins that the
// operator-supplied `CutoverConfig.Transitions` actually replaces
// the default — a misconfigured worker that fell back to the
// defaults would silently broaden the set of tenants it touches
// (e.g., a runbook intending to migrate ONLY shared tenants would
// also flip every legacy tenant past the threshold).
func TestCutoverWorker_CustomTransitionsOverride(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-legacy", "tenant-shared"})
	store.flipBackend("tenant-shared", BackendSharedMeilisearch)
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) {
		return 200_000, nil
	})
	source := MessageSourceFunc(func(_ context.Context, tenantID string) ([]Message, error) {
		return []Message{{TenantID: tenantID, MessageID: "m1"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:       store,
		Service:     flipper,
		Sizer:       sizer,
		Source:      source,
		Logger:      silentLogger(),
		Threshold:   100_000,
		Interval:    time.Hour,
		MaxFailures: 5,
		MaxRetryGap: time.Hour,
		Now:         func() time.Time { return now },
		Sleep:       func(time.Duration) {},
		Transitions: []CutoverTransition{
			{Source: BackendSharedMeilisearch, Target: BackendSharedOpenSearch},
		},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}
	w.Tick(context.Background())

	// Custom transitions: shared moves, legacy must not.
	if got := store.tenantBackends["tenant-shared"]; got != BackendSharedOpenSearch {
		t.Errorf("tenant-shared backend = %q, want %q", got, BackendSharedOpenSearch)
	}
	if got := store.tenantBackends["tenant-legacy"]; got != BackendMeilisearch {
		t.Errorf("tenant-legacy backend = %q, want %q (untouched)", got, BackendMeilisearch)
	}
}

// TestCutoverWorker_SameTenantBothTransitionsKeyedIndependently is
// the regression test for the (tenant_id, target_backend) composite
// PK on `search_cutover_jobs`. The same tenant can participate in TWO
// transitions over time — first `shared_meilisearch ->
// shared_opensearch`, then a runbook reverts the tenant back to
// `shared_meilisearch` and the operator wants the next cutover
// tick to re-promote it. Under the old (tenant_id-only) PK the
// completed row for `shared_opensearch` would silently hide the
// tenant from `ListCandidates` on the next tick. With the new
// composite PK, each (tenant, target) pair carries its own state
// row so the second promotion sees an empty slot and runs.
//
// We exercise the principle with a single tick covering both
// DefaultCutoverTransitions: a tenant that's already-on
// `opensearch` (transition 1 has completed historically) but
// still on the source of transition 2 (`shared_meilisearch`)
// must be promoted by transition 2 in this tick. Without the
// composite key, the historical row for transition 1 hides the
// tenant from transition 2's candidate list.
func TestCutoverWorker_SameTenantBothTransitionsKeyedIndependently(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-x"})
	// Tenant is on the SECOND transition's source. The FIRST
	// transition has historically completed for this tenant
	// (legacy meilisearch -> opensearch), so the worker's job
	// row for opensearch is pre-seeded as completed.
	store.flipBackend("tenant-x", BackendSharedMeilisearch)
	store.rows[memRowKey{tenantID: "tenant-x", targetBackend: BackendOpenSearch}] = &memRow{
		state:       CutoverCompleted,
		completedAt: ptrTime(time.Unix(1_600_000_000, 0)),
		updatedAt:   time.Unix(1_600_000_000, 0),
	}

	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) {
		return 200_000, nil
	})
	source := MessageSourceFunc(func(_ context.Context, tenantID string) ([]Message, error) {
		return []Message{{TenantID: tenantID, MessageID: tenantID + "-m1"}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())

	// transition 2 must promote tenant-x to shared_opensearch.
	if got := store.tenantBackends["tenant-x"]; got != BackendSharedOpenSearch {
		t.Errorf("tenant-x backend = %q, want %q (transition 2 must promote)", got, BackendSharedOpenSearch)
	}
	if rec := flipper.reindexCall["tenant-x"]; rec.backend != BackendSharedOpenSearch {
		t.Errorf("tenant-x reindex target = %q, want %q", rec.backend, BackendSharedOpenSearch)
	}
	// The shared_opensearch job row must exist and be completed.
	if r := store.rowFor("tenant-x", BackendSharedOpenSearch); r == nil || r.state != CutoverCompleted {
		t.Errorf("tenant-x shared_opensearch row = %+v, want CutoverCompleted", r)
	}
	// The PRE-EXISTING opensearch row must remain untouched —
	// the new transition operates entirely on its own composite
	// key, so the legacy row's completed_at / failure_count must
	// not be perturbed.
	legacy := store.rowFor("tenant-x", BackendOpenSearch)
	if legacy == nil || legacy.state != CutoverCompleted {
		t.Errorf("legacy opensearch row mutated: %+v, want still CutoverCompleted", legacy)
	}
	if legacy != nil && legacy.completedAt != nil && !legacy.completedAt.Equal(time.Unix(1_600_000_000, 0)) {
		t.Errorf("legacy opensearch row completed_at = %v, want unchanged", legacy.completedAt)
	}
}

// ptrTime is the canonical "take address of a time.Time literal"
// helper. Used by the multi-transition test above so we can
// inject a pre-existing job row with a populated `completedAt`.
func ptrTime(t time.Time) *time.Time { return &t }

// fakeGetter reports a tenant's current backend by reading the
// linked in-memory store's tenantBackends map, so it reflects a
// SetBackend flip immediately (mirroring production's GetBackend
// reading `tenants.search_backend`).
type fakeGetter struct{ store *inMemoryCutoverStore }

func (g fakeGetter) GetBackend(_ context.Context, tenantID string) (string, error) {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	b, ok := g.store.tenantBackends[tenantID]
	if !ok {
		return "", ErrNotFound
	}
	return b, nil
}

// recordingAudit captures every audit.Entry the cutover paths emit
// so tests can assert action / actor / metadata wiring.
type recordingAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *recordingAudit) Log(_ context.Context, e audit.Entry) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return &e, nil
}

func (a *recordingAudit) find(action string) (audit.Entry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.Action == action {
			return e, true
		}
	}
	return audit.Entry{}, false
}

// TestCutoverWorker_ValidationFailureLeavesTenantOnSource is the
// headline safety test: a reindex that "succeeds" but produces an
// index the validation step can't query must NOT flip the backend
// — the tenant stays readable on the source and the row is failed.
func TestCutoverWorker_ValidationFailureLeavesTenantOnSource(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	flipper.validateErr["tenant-a"] = errors.New("0 of 2 sample messages searchable")
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) {
		return []Message{{TenantID: "tenant-a", MessageID: "m1"}}, nil
	})
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())

	// Reindex ran (validation is downstream of it)...
	if rec := flipper.reindexCall["tenant-a"]; rec.backend != BackendOpenSearch {
		t.Fatalf("expected ReindexTo to run before validation, got %+v", rec)
	}
	// ...but the backend was NOT flipped.
	if got := flipper.setBackendCall["tenant-a"]; got != "" {
		t.Fatalf("SetBackend ran despite validation failure: %q", got)
	}
	if store.tenantBackends["tenant-a"] != BackendMeilisearch {
		t.Fatalf("tenant moved off source on validation failure: %q", store.tenantBackends["tenant-a"])
	}
	r := store.rowByTenant("tenant-a")
	if r == nil || r.state != CutoverFailed || r.failureCount != 1 {
		t.Fatalf("row = %+v, want failed/1", r)
	}
	if want := "validate: "; len(r.lastError) < len(want) || r.lastError[:len(want)] != want {
		t.Fatalf("last_error = %q, want a 'validate: ...' reason", r.lastError)
	}
}

// TestCutoverWorker_ValidationRunsBetweenReindexAndSetBackend pins
// the call ordering: validation sees the reindexed batch and runs
// before the backend flip on the happy path.
func TestCutoverWorker_ValidationRunsBetweenReindexAndSetBackend(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	msgs := []Message{{TenantID: "tenant-a", MessageID: "m1"}, {TenantID: "tenant-a", MessageID: "m2"}}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) { return msgs, nil })
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())

	vc, ok := flipper.validateCall["tenant-a"]
	if !ok {
		t.Fatal("Validate was never called on the happy path")
	}
	if vc.backend != BackendOpenSearch || vc.count != 2 {
		t.Fatalf("Validate saw %+v, want {opensearch, 2}", vc)
	}
	if flipper.setBackendCall["tenant-a"] != BackendOpenSearch {
		t.Fatal("SetBackend did not run after successful validation")
	}
}

// barrierFlipper blocks every ReindexTo at a barrier so a test can
// observe exactly how many cutovers the worker runs in parallel.
type barrierFlipper struct {
	inner   *fakeFlipper
	entered chan string
	release chan struct{}
}

func (b *barrierFlipper) ReindexTo(ctx context.Context, tenantID, backend string, msgs []Message) error {
	b.entered <- tenantID
	<-b.release
	return b.inner.ReindexTo(ctx, tenantID, backend, msgs)
}

func (b *barrierFlipper) SetBackend(ctx context.Context, tenantID, backend string) error {
	return b.inner.SetBackend(ctx, tenantID, backend)
}

func (b *barrierFlipper) Validate(ctx context.Context, tenantID, backend string, msgs []Message) error {
	return b.inner.Validate(ctx, tenantID, backend, msgs)
}

// TestCutoverWorker_ConcurrencyCapBoundsInFlight proves the worker
// never runs more than Concurrency cutovers at once. With a cap of
// 2 and 5 oversized tenants, exactly 2 reach ReindexTo while the
// rest wait; releasing the barrier lets the remaining tenants
// migrate.
func TestCutoverWorker_ConcurrencyCapBoundsInFlight(t *testing.T) {
	tenants := []string{"t1", "t2", "t3", "t4", "t5"}
	store := newInMemoryStore(tenants)
	flipper := newFakeFlipper(store)
	barrier := &barrierFlipper{
		inner:   flipper,
		entered: make(chan string, len(tenants)),
		release: make(chan struct{}),
	}
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: id + "-m1"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:       store,
		Service:     barrier,
		Sizer:       sizer,
		Source:      source,
		Logger:      silentLogger(),
		Threshold:   100_000,
		Interval:    time.Hour,
		MaxFailures: 5,
		MaxRetryGap: time.Hour,
		Concurrency: 2,
		Now:         func() time.Time { return now },
		Sleep:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}

	done := make(chan struct{})
	go func() {
		w.Tick(context.Background())
		close(done)
	}()

	// Exactly Concurrency (2) cutovers should reach the barrier.
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d cutovers started, want 2 in parallel", i)
		}
	}
	// A 3rd must NOT start while the first two hold their slots.
	select {
	case id := <-barrier.entered:
		t.Fatalf("concurrency cap exceeded: tenant %q started a 3rd parallel cutover", id)
	case <-time.After(150 * time.Millisecond):
	}

	// Release the barrier; every tenant should now migrate.
	close(barrier.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after releasing the barrier")
	}
	for _, id := range tenants {
		if r := store.rowByTenant(id); r == nil || r.state != CutoverCompleted {
			t.Fatalf("tenant %s row = %+v, want completed", id, r)
		}
	}
}

// TestCutoverMetrics_TrackCompletedAndFailed verifies the
// Prometheus counters move on the success and failure paths and
// the in-flight gauge settles back to zero.
func TestCutoverMetrics_TrackCompletedAndFailed(t *testing.T) {
	store := newInMemoryStore([]string{"ok-tenant", "bad-tenant"})
	flipper := newFakeFlipper(store)
	flipper.reindexErr["bad-tenant"] = errors.New("opensearch down")
	metrics := NewCutoverMetrics(nil)
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: id + "-m1"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:       store,
		Service:     flipper,
		Sizer:       sizer,
		Source:      source,
		Logger:      silentLogger(),
		Metrics:     metrics,
		Threshold:   100_000,
		Interval:    time.Hour,
		MaxFailures: 5,
		MaxRetryGap: time.Hour,
		Now:         func() time.Time { return now },
		Sleep:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}
	w.Tick(context.Background())

	if got := testutil.ToFloat64(metrics.Completed); got != 1 {
		t.Fatalf("completed counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.Failed); got != 1 {
		t.Fatalf("failed counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.InProgress); got != 0 {
		t.Fatalf("in-progress gauge = %v, want 0 after tick settles", got)
	}
}

// TestCutoverWorker_AuditsCompletedAndFailed verifies the worker
// emits system-actor audit entries for both terminal outcomes with
// the source/target backends recorded in the metadata.
func TestCutoverWorker_AuditsCompletedAndFailed(t *testing.T) {
	store := newInMemoryStore([]string{"ok-tenant", "bad-tenant"})
	flipper := newFakeFlipper(store)
	flipper.reindexErr["bad-tenant"] = errors.New("opensearch down")
	rec := &recordingAudit{}
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: id + "-m1"}}, nil
	})
	w, err := NewCutoverWorker(CutoverConfig{
		Store:       store,
		Service:     flipper,
		Sizer:       sizer,
		Source:      source,
		Audit:       rec,
		Logger:      silentLogger(),
		Threshold:   100_000,
		Interval:    time.Hour,
		MaxFailures: 5,
		MaxRetryGap: time.Hour,
		Now:         func() time.Time { return now },
		Sleep:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}
	w.Tick(context.Background())

	done, ok := rec.find("search_cutover_completed")
	if !ok {
		t.Fatal("no search_cutover_completed audit entry")
	}
	if done.ActorType != audit.ActorSystem {
		t.Fatalf("completed actorType = %q, want system", done.ActorType)
	}
	if done.Metadata["target_backend"] != BackendOpenSearch || done.Metadata["source_backend"] != BackendMeilisearch {
		t.Fatalf("completed metadata = %+v, want source meili / target opensearch", done.Metadata)
	}
	failed, ok := rec.find("search_cutover_failed")
	if !ok {
		t.Fatal("no search_cutover_failed audit entry")
	}
	if failed.ActorType != audit.ActorSystem {
		t.Fatalf("failed actorType = %q, want system", failed.ActorType)
	}
	if _, hasErr := failed.Metadata["error"]; !hasErr {
		t.Fatalf("failed metadata missing error: %+v", failed.Metadata)
	}
}

// buildCutoverService wires a CutoverService over the in-memory
// store + fake flipper used by the manual-path tests.
func buildCutoverService(t *testing.T, store *inMemoryCutoverStore, flipper BackendFlipper, source MessageSource, aud AuditLogger) *CutoverService {
	t.Helper()
	svc, err := NewCutoverService(CutoverServiceConfig{
		Store:     store,
		Flipper:   flipper,
		Source:    source,
		Sizer:     MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil }),
		Getter:    fakeGetter{store: store},
		Audit:     aud,
		Logger:    silentLogger(),
		Threshold: 100_000,
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
		Sleep:     func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewCutoverService: %v", err)
	}
	return svc
}

// TestCutoverService_InitiateExecuteList walks the full manual
// path: initiate creates a pending row, execute migrates + flips +
// audits as the operator, and list reflects the completed job.
func TestCutoverService_InitiateExecuteList(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	rec := &recordingAudit{}
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: "m1"}}, nil
	})
	svc := buildCutoverService(t, store, flipper, source, rec)
	ctx := context.Background()

	job, err := svc.InitiateCutover(ctx, "tenant-a", BackendOpenSearch)
	if err != nil {
		t.Fatalf("InitiateCutover: %v", err)
	}
	if job.State != CutoverPending {
		t.Fatalf("initiated job state = %q, want pending", job.State)
	}

	if err := svc.ExecuteCutover(ctx, "tenant-a", BackendOpenSearch, "admin-1"); err != nil {
		t.Fatalf("ExecuteCutover: %v", err)
	}
	if store.tenantBackends["tenant-a"] != BackendOpenSearch {
		t.Fatalf("backend not flipped: %q", store.tenantBackends["tenant-a"])
	}

	jobs, err := svc.ListCutoverJobs(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListCutoverJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != CutoverCompleted {
		t.Fatalf("jobs = %+v, want one completed", jobs)
	}

	entry, ok := rec.find("search_cutover_completed")
	if !ok {
		t.Fatal("no completed audit entry for manual cutover")
	}
	if entry.ActorType != audit.ActorAdmin || entry.ActorID != "admin-1" {
		t.Fatalf("manual audit actor = %q/%q, want admin/admin-1", entry.ActorType, entry.ActorID)
	}

	// Initiating again now that the tenant is already on the
	// target must be rejected as a no-op.
	if _, err := svc.InitiateCutover(ctx, "tenant-a", BackendOpenSearch); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("re-initiate on current backend err = %v, want ErrInvalidInput", err)
	}
}

// TestCutoverService_ValidationFailureKeepsSource confirms the
// manual path shares the worker's safety guarantee: a validation
// failure aborts the cutover and leaves the tenant on source.
func TestCutoverService_ValidationFailureKeepsSource(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	flipper.validateErr["tenant-a"] = errors.New("not searchable")
	rec := &recordingAudit{}
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: "m1"}}, nil
	})
	svc := buildCutoverService(t, store, flipper, source, rec)
	ctx := context.Background()

	if _, err := svc.InitiateCutover(ctx, "tenant-a", BackendOpenSearch); err != nil {
		t.Fatalf("InitiateCutover: %v", err)
	}
	err := svc.ExecuteCutover(ctx, "tenant-a", BackendOpenSearch, "admin-1")
	if err == nil {
		t.Fatal("ExecuteCutover succeeded despite validation failure")
	}
	if store.tenantBackends["tenant-a"] != BackendMeilisearch {
		t.Fatalf("tenant moved off source: %q", store.tenantBackends["tenant-a"])
	}
	if r := store.rowByTenant("tenant-a"); r == nil || r.state != CutoverFailed {
		t.Fatalf("row = %+v, want failed", r)
	}
	if _, ok := rec.find("search_cutover_failed"); !ok {
		t.Fatal("no failed audit entry for manual cutover")
	}
}

// TestCutoverService_ExecuteInProgressReturnsErr proves a manual
// execute won't double-run a cutover another actor already claimed.
func TestCutoverService_ExecuteInProgressReturnsErr(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: "m1"}}, nil
	})
	svc := buildCutoverService(t, store, flipper, source, nil)
	ctx := context.Background()

	// Simulate the auto-worker already holding the claim.
	if _, err := store.Claim(ctx, "tenant-a", BackendOpenSearch, 200_000, 100_000, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := svc.ExecuteCutover(ctx, "tenant-a", BackendOpenSearch, "admin-1"); !errors.Is(err, ErrCutoverInProgress) {
		t.Fatalf("ExecuteCutover err = %v, want ErrCutoverInProgress", err)
	}
}

// TestCutoverService_ReInitiateInProgressPreservesStaleClock proves a
// manual re-initiate against an already-running cutover leaves the
// in_progress row entirely untouched — in particular it must NOT bump
// updated_at. Bumping it would reset ReconcileStale's crash-recovery
// window on every retry, indefinitely postponing demotion of a
// genuinely crashed migration. Regression test for the UpsertPending
// unconditional `updated_at = $5`.
func TestCutoverService_ReInitiateInProgressPreservesStaleClock(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	source := MessageSourceFunc(func(_ context.Context, id string) ([]Message, error) {
		return []Message{{TenantID: id, MessageID: "m1"}}, nil
	})
	svc := buildCutoverService(t, store, flipper, source, nil)
	ctx := context.Background()

	// Claim the row well in the past; buildCutoverService's clock is
	// ~100M seconds later, so any bump would obviously reset the clock.
	claimedAt := time.Unix(1_600_000_000, 0)
	if ok, err := store.Claim(ctx, "tenant-a", BackendOpenSearch, 200_000, 100_000, claimedAt); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	job, err := svc.InitiateCutover(ctx, "tenant-a", BackendOpenSearch)
	if err != nil {
		t.Fatalf("InitiateCutover: %v", err)
	}
	if job.State != CutoverInProgress {
		t.Fatalf("re-initiate state = %q, want in_progress (row left untouched)", job.State)
	}
	if r := store.rowByTenant("tenant-a"); r == nil || !r.updatedAt.Equal(claimedAt) {
		t.Fatalf("updatedAt = %v, want unchanged %v (manual re-initiate must not reset the stale clock)", r, claimedAt)
	}

	// Because updated_at survived the re-initiate, the crash-recovery
	// sweep still sees the row as stale and demotes it to failed.
	before := claimedAt.Add(time.Hour)
	n, err := store.ReconcileStale(ctx, BackendMeilisearch, BackendOpenSearch, before, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("ReconcileStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReconcileStale demoted %d rows, want 1 (stale clock must survive re-initiate)", n)
	}
}
