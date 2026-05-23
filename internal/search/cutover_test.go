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
// updated_at, with the same atomic-claim semantics.
type inMemoryCutoverStore struct {
	mu        sync.Mutex
	rows      map[string]*memRow
	// tenants is the list of tenants currently on Meilisearch.
	// ListCandidates uses this to mimic the SQL JOIN against
	// the tenants table.
	tenants []string
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

func newInMemoryStore(tenants []string) *inMemoryCutoverStore {
	return &inMemoryCutoverStore{rows: map[string]*memRow{}, tenants: tenants}
}

func (s *inMemoryCutoverStore) ListCandidates(_ context.Context, f CandidateFilter) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, id := range s.tenants {
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

// fakeFlipper records SetBackend / Reindex invocations so tests
// can assert what the worker actually drove. setBackendErr /
// reindexErr inject per-tenant failures.
type fakeFlipper struct {
	mu             sync.Mutex
	setBackendCall map[string]string
	reindexCall    map[string]int // number of messages reindexed
	setBackendErr  map[string]error
	reindexErr     map[string]error
}

func newFakeFlipper() *fakeFlipper {
	return &fakeFlipper{
		setBackendCall: map[string]string{},
		reindexCall:    map[string]int{},
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
	return nil
}

func (f *fakeFlipper) Reindex(_ context.Context, tenantID string, msgs []Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reindexErr[tenantID]; err != nil {
		return err
	}
	f.reindexCall[tenantID] = len(msgs)
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
	flipper := newFakeFlipper()
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
// migration: oversized tenant → backend flipped → messages
// reindexed → row marked completed.
func TestCutoverWorker_AboveThresholdFullCutover(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper()
	now := time.Unix(1_700_000_000, 0)
	msgs := []Message{
		{TenantID: "tenant-a", MessageID: "m1"},
		{TenantID: "tenant-a", MessageID: "m2"},
	}
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 200_000, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) { return msgs, nil })
	w := buildWorker(t, store, flipper, sizer, source, 100_000, func() time.Time { return now })
	w.Tick(context.Background())

	if got := flipper.setBackendCall["tenant-a"]; got != BackendOpenSearch {
		t.Fatalf("SetBackend tenant-a = %q, want %q", got, BackendOpenSearch)
	}
	if got := flipper.reindexCall["tenant-a"]; got != 2 {
		t.Fatalf("Reindex tenant-a = %d msgs, want 2", got)
	}
	if r := store.rows["tenant-a"]; r == nil || r.state != CutoverCompleted {
		t.Fatalf("row state = %+v, want CutoverCompleted", r)
	}
}

// TestCutoverWorker_ReindexFailureMarksFailedAndRetries verifies
// the failure path: when Reindex errors, the row goes to `failed`
// and a later tick (after the retry window) picks it up again.
func TestCutoverWorker_ReindexFailureMarksFailedAndRetries(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper()
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
	flipper := newFakeFlipper()
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
	flipper := newFakeFlipper()
	// Block Reindex on a channel so the first claim holds the
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
	// and entered Reindex.
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

func (g *gatedFlipper) Reindex(ctx context.Context, tenantID string, msgs []Message) error {
	select {
	case g.hit <- struct{}{}:
	default:
	}
	<-g.gate
	return g.inner.Reindex(ctx, tenantID, msgs)
}

// TestCutoverWorker_MultipleTenants verifies the worker iterates
// the candidate list and migrates each independently — one
// tenant's failure must not stop the next from migrating.
func TestCutoverWorker_MultipleTenants(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a", "tenant-b", "tenant-c"})
	flipper := newFakeFlipper()
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
		{"missing sizer", CutoverConfig{Store: store, Service: newFakeFlipper(), Source: MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })}},
		{"missing source", CutoverConfig{Store: store, Service: newFakeFlipper(), Sizer: MailboxSizerFunc(func(context.Context, string) (int64, error) { return 0, nil })}},
		{"missing store and pool", CutoverConfig{Service: newFakeFlipper(), Sizer: MailboxSizerFunc(func(context.Context, string) (int64, error) { return 0, nil }), Source: MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewCutoverWorker(c.cfg); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
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
