package search

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
)

// fakeShardLister returns a configured set of shards and an
// optional listing error.
type fakeShardLister struct {
	ids []string
	err error
}

func (f *fakeShardLister) ListShardIDs(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ids, nil
}

// fakeEnsurer records (name, shardID) pairs that EnsureIndex
// was invoked with and can inject a per-shard error.
type fakeEnsurer struct {
	mu    sync.Mutex
	name  string
	calls []string
	errs  map[string]error
}

func (e *fakeEnsurer) Name() string { return e.name }
func (e *fakeEnsurer) EnsureIndex(_ context.Context, shardID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, shardID)
	if err, ok := e.errs[shardID]; ok {
		return err
	}
	return nil
}

// TestEnsureSharedIndexes_HappyPathHitsEverybody pins the
// architectural contract: every (backend, shard) pair must be
// visited once, in order. Without this the initialiser could
// silently skip a backend and leave OpenSearch auto-creating
// mappings without the keyword type for `tenant_id`.
func TestEnsureSharedIndexes_HappyPathHitsEverybody(t *testing.T) {
	shards := &fakeShardLister{ids: []string{"shard-a", "shard-b", "shard-c"}}
	meili := &fakeEnsurer{name: "shared_meilisearch"}
	open := &fakeEnsurer{name: "shared_opensearch"}
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	if err := EnsureSharedIndexes(context.Background(), logger, shards, []SharedIndexEnsurer{meili, open}); err != nil {
		t.Fatalf("EnsureSharedIndexes: %v", err)
	}
	want := []string{"shard-a", "shard-b", "shard-c"}
	if !equalSlices(meili.calls, want) {
		t.Errorf("meili calls = %v, want %v", meili.calls, want)
	}
	if !equalSlices(open.calls, want) {
		t.Errorf("open calls = %v, want %v", open.calls, want)
	}
}

// TestEnsureSharedIndexes_NoBackendsIsNoOp pins that an
// operator who has not wired the shared backends (e.g. a
// staging env on the legacy per-tenant backends) does NOT
// crash the BFF startup loop.
func TestEnsureSharedIndexes_NoBackendsIsNoOp(t *testing.T) {
	shards := &fakeShardLister{ids: []string{"shard-a"}}
	if err := EnsureSharedIndexes(context.Background(), nil, shards, nil); err != nil {
		t.Fatalf("EnsureSharedIndexes(nil backends): %v", err)
	}
}

// TestEnsureSharedIndexes_PerShardFailureIsLoggedAndAggregated
// pins the partial-failure contract: a single misbehaving shard
// (e.g., one OpenSearch node in the fleet is down) must NOT
// block the rest of the fleet from being initialised. The error
// MUST be logged per-shard AND the function must return an
// aggregate `EnsureSharedIndexesError` so the caller has ONE
// high-signal log line (`N of M pairs failed`) in addition to
// the per-shard ones — operators don't have to grep + count.
func TestEnsureSharedIndexes_PerShardFailureIsLoggedAndAggregated(t *testing.T) {
	shards := &fakeShardLister{ids: []string{"shard-good", "shard-bad", "shard-also-good"}}
	connRefused := errors.New("connection refused")
	open := &fakeEnsurer{
		name: "shared_opensearch",
		errs: map[string]error{"shard-bad": connRefused},
	}
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	err := EnsureSharedIndexes(context.Background(), logger, shards, []SharedIndexEnsurer{open})
	if err == nil {
		t.Fatal("EnsureSharedIndexes returned nil; want aggregate EnsureSharedIndexesError")
	}
	wantAll := []string{"shard-good", "shard-bad", "shard-also-good"}
	if !equalSlices(open.calls, wantAll) {
		t.Errorf("open.calls = %v, want all three shards visited %v (a partial-failure must NOT abort the loop)", open.calls, wantAll)
	}
	if !strings.Contains(logBuf.String(), "shard=shard-bad") || !strings.Contains(logBuf.String(), "connection refused") {
		t.Errorf("log did not record the per-shard failure: %q", logBuf.String())
	}
	var agg *EnsureSharedIndexesError
	if !errors.As(err, &agg) {
		t.Fatalf("err is not *EnsureSharedIndexesError: %T (%v)", err, err)
	}
	if agg.Attempted != 3 {
		t.Errorf("agg.Attempted = %d, want 3", agg.Attempted)
	}
	if len(agg.Failures) != 1 {
		t.Fatalf("agg.Failures len = %d, want 1", len(agg.Failures))
	}
	f := agg.Failures[0]
	if f.Backend != "shared_opensearch" || f.ShardID != "shard-bad" {
		t.Errorf("agg.Failures[0] = %+v, want (shared_opensearch, shard-bad)", f)
	}
	if !errors.Is(f.Err, connRefused) {
		t.Errorf("agg.Failures[0].Err = %v, want connRefused sentinel", f.Err)
	}
	// errors.Is on the aggregate MUST surface the underlying
	// error (via Unwrap) so future callers can branch on e.g.
	// context.DeadlineExceeded without losing the count via
	// Error().
	if !errors.Is(err, connRefused) {
		t.Errorf("errors.Is(err, connRefused) = false; want true (Unwrap should surface first failure)")
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("aggregate Error() = %q, want it to include count '1 of 3'", err.Error())
	}
}

// TestEnsureSharedIndexes_AllShardsFailReturnsAggregate pins
// the worst-case where every shard fails (e.g. the upstream
// search backend is entirely unreachable). The function must
// still return an aggregate error rather than nil, so an
// operator alert can fire on the high-signal log line.
func TestEnsureSharedIndexes_AllShardsFailReturnsAggregate(t *testing.T) {
	shards := &fakeShardLister{ids: []string{"shard-a", "shard-b"}}
	boom := errors.New("backend unreachable")
	open := &fakeEnsurer{
		name: "shared_opensearch",
		errs: map[string]error{"shard-a": boom, "shard-b": boom},
	}
	err := EnsureSharedIndexes(context.Background(), nil, shards, []SharedIndexEnsurer{open})
	var agg *EnsureSharedIndexesError
	if !errors.As(err, &agg) {
		t.Fatalf("err is not *EnsureSharedIndexesError: %T (%v)", err, err)
	}
	if agg.Attempted != 2 || len(agg.Failures) != 2 {
		t.Errorf("agg = {Attempted: %d, Failures: %d}, want {2, 2}", agg.Attempted, len(agg.Failures))
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("aggregate Error() = %q, want '2 of 2'", err.Error())
	}
}

// TestEnsureSharedIndexes_SuccessReturnsNil pins that the
// aggregate-error path is opt-in only on failure: the happy
// path stays `nil` so existing `if err != nil { log... }`
// pattern in main.go stays clean.
func TestEnsureSharedIndexes_SuccessReturnsNil(t *testing.T) {
	shards := &fakeShardLister{ids: []string{"shard-a", "shard-b"}}
	open := &fakeEnsurer{name: "shared_opensearch"}
	if err := EnsureSharedIndexes(context.Background(), nil, shards, []SharedIndexEnsurer{open}); err != nil {
		t.Fatalf("EnsureSharedIndexes (happy path) returned %v; want nil", err)
	}
}

// TestEnsureSharedIndexes_ShardListerErrorPropagates pins the
// opposite contract: if we cannot enumerate shards at all, we
// MUST return early so the operator sees the failure. A silent
// success here would leave the worker iterating over an empty
// set forever.
func TestEnsureSharedIndexes_ShardListerErrorPropagates(t *testing.T) {
	shards := &fakeShardLister{err: errors.New("pgx: connection closed")}
	open := &fakeEnsurer{name: "shared_opensearch"}
	if err := EnsureSharedIndexes(context.Background(), nil, shards, []SharedIndexEnsurer{open}); err == nil {
		t.Fatal("ShardLister error swallowed; want propagation")
	}
	if len(open.calls) != 0 {
		t.Errorf("open.calls = %v, want zero (shard listing failed before any ensure)", open.calls)
	}
}

// TestEnsureSharedIndexes_NilShardListerIsConfigError pins
// against a programmer error in main.go forgetting to pass a
// shard service. Returning an explicit error here is friendlier
// than a nil-pointer panic deep inside the loop.
func TestEnsureSharedIndexes_NilShardListerIsConfigError(t *testing.T) {
	open := &fakeEnsurer{name: "shared_opensearch"}
	if err := EnsureSharedIndexes(context.Background(), nil, nil, []SharedIndexEnsurer{open}); err == nil {
		t.Fatal("nil ShardLister accepted; want error")
	}
}

// TestEnsureSharedIndexes_SkipsNilBackend defends against an
// operator wiring an empty slot in the backends slice (e.g. a
// configured-but-not-built shared opensearch) — must skip it
// without panicking.
func TestEnsureSharedIndexes_SkipsNilBackend(t *testing.T) {
	shards := &fakeShardLister{ids: []string{"shard-a"}}
	meili := &fakeEnsurer{name: "shared_meilisearch"}
	if err := EnsureSharedIndexes(context.Background(), nil, shards, []SharedIndexEnsurer{nil, meili}); err != nil {
		t.Fatalf("EnsureSharedIndexes with nil backend in slice: %v", err)
	}
	if !equalSlices(meili.calls, []string{"shard-a"}) {
		t.Errorf("meili.calls = %v, want [shard-a]", meili.calls)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
