package search

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// inMemoryCutoverStore exercises the worker's state machine
// without a real Postgres. It mirrors the production schema
// closely: a row per tenant with state, failure count, and
// updated_at, plus a per-tenant `search_backend` value to mirror
// the production SQL JOIN against the `tenants` table. The
// SourceBackend filter is honoured exactly the same way as the
// Postgres impl so a test failure here mirrors a production
// regression.
type inMemoryCutoverStore struct {
	mu sync.Mutex
	// rows is the search_cutover_jobs equivalent — one entry per
	// tenant that's ever been claimed.
	rows map[string]*memRow
	// tenantBackends mirrors `tenants.search_backend` so the
	// store can filter by SourceBackend exactly like the
	// production SQL JOIN does. Tests that need to simulate a
	// SetBackend mid-cutover update this map directly via
	// flipBackend().
	tenantBackends map[string]string
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
		rows:           map[string]*memRow{},
		tenantBackends: backends,
	}
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
		r, ok := s.rows[id]
		switch {
		case !ok:
			ids = append(ids, id)
		case r.state == CutoverFailed && r.failureCount < f.MaxFailures && r.updatedAt.Before(f.RetryAfterBefore):
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *inMemoryCutoverStore) Claim(_ context.Context, tenantID string, size, threshold int64, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[tenantID]
	if !ok {
		r = &memRow{state: CutoverPending, mailboxSize: size, threshold: threshold, updatedAt: now}
		s.rows[tenantID] = r
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

func (s *inMemoryCutoverStore) MarkCompleted(_ context.Context, tenantID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[tenantID]
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

func (s *inMemoryCutoverStore) MarkFailed(_ context.Context, tenantID, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[tenantID]
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
// on `targetBackend` AND whose updated_at predates `before`. The
// completion timestamp uses time.Now() to match the production
// query (which uses NOW() so the dashboard reflects when the
// reconciliation actually fired, not when the migration started).
func (s *inMemoryCutoverStore) ReconcileCompleted(_ context.Context, targetBackend string, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	now := time.Now()
	for tenantID, r := range s.rows {
		if r.state != CutoverInProgress {
			continue
		}
		if !r.updatedAt.Before(before) {
			continue
		}
		if s.tenantBackends[tenantID] != targetBackend {
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
	}
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
	if _, ok := store.rows["tenant-a"]; ok {
		t.Fatal("under-threshold tenant got a state row")
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverCompleted {
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverFailed || r.failureCount != 1 {
		t.Fatalf("after failed reindex row = %+v, want failed/1", r)
	}

	// Immediately re-tick inside the back-off window: should NOT
	// retry (failure_count still 1).
	w.Tick(context.Background())
	if r := store.rows["tenant-a"]; r.failureCount != 1 {
		t.Fatalf("back-off ignored: failureCount = %d, want 1", r.failureCount)
	}

	// Advance past the back-off window AND clear the underlying
	// error so the retry succeeds.
	clock.Advance(2 * time.Hour)
	delete(flipper.reindexErr, "tenant-a")
	w.Tick(context.Background())
	if r := store.rows["tenant-a"]; r.state != CutoverCompleted {
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
	if got := store.rows["tenant-a"].failureCount; got != 5 {
		t.Fatalf("failureCount = %d, want exactly 5", got)
	}

	// 6th tick: the candidate filter excludes this tenant.
	w.Tick(context.Background())
	if got := store.rows["tenant-a"].failureCount; got != 5 {
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverCompleted {
		t.Errorf("tenant-a state = %v, want completed", r)
	}
	if r := store.rows["tenant-b"]; r == nil || r.state != CutoverFailed {
		t.Errorf("tenant-b state = %v, want failed", r)
	}
	if _, ok := store.rows["tenant-c"]; ok {
		t.Errorf("tenant-c got a row despite being below threshold")
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
	ids, err = store.ListCandidates(context.Background(), CandidateFilter{
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverFailed {
		t.Fatalf("row = %+v, want failed", r)
	}
	if store.tenantBackends["tenant-a"] != BackendMeilisearch {
		t.Fatalf("tenant backend = %q, want meilisearch (failed SetBackend must not flip)", store.tenantBackends["tenant-a"])
	}
	// Sanity: candidate filter still picks up the tenant.
	ids, _ := store.ListCandidates(context.Background(), CandidateFilter{
		SourceBackend:    BackendMeilisearch,
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
	if r := store.rows["tenant-a"]; r.state != CutoverCompleted {
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverCompleted {
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverInProgress {
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
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverCompleted {
		t.Fatalf("after reconcile tick, row = %+v, want completed", r)
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

func (s *flakeyMarkCompleted) MarkCompleted(ctx context.Context, tenantID string, now time.Time) error {
	s.calls++
	if s.failuresLeft > 0 {
		s.failuresLeft--
		return errors.New("transient: postgres conn reset")
	}
	return s.CutoverStore.MarkCompleted(ctx, tenantID, now)
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
