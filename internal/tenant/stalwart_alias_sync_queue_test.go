package tenant

import (
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------
// Pure-function tests for the queue helpers. The Exec/Query paths
// require a live pgx pool which is covered by the integration
// test suite; here we cover the in-process branches that gate
// every DB call.
// ---------------------------------------------------------------

func TestNextAliasSyncBackoff_Schedule(t *testing.T) {
	// `attempt` is the 1-indexed number of the attempt that *just
	// failed*, per the contract on `nextAliasSyncBackoff`. The
	// schedule under test is the documented webhook-mirrored
	// 30s -> 2m -> 10m -> 30m -> 1h. The fall-through case
	// (attempt past the schedule) returns 1h — the worker's
	// `AliasSyncMaxAttempts` guard gives up before that tier
	// would otherwise produce a too-large delay.
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, 2 * time.Minute},
		{3, 10 * time.Minute},
		{4, 30 * time.Minute},
		{5, time.Hour},
		{6, time.Hour},
		{100, time.Hour},
	}
	for _, tc := range tests {
		if got := nextAliasSyncBackoff(tc.attempt); got != tc.want {
			t.Errorf("nextAliasSyncBackoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestMarkAliasSyncSynced_NilPool(t *testing.T) {
	if err := markAliasSyncSynced(context.Background(), nil, "id"); err == nil {
		t.Error("expected error on nil pool")
	}
}

func TestMarkAliasSyncFailed_NilPool(t *testing.T) {
	if err := markAliasSyncFailed(context.Background(), nil, "id", "msg"); err == nil {
		t.Error("expected error on nil pool")
	}
}

func TestRecordAliasSyncFailure_NilPool(t *testing.T) {
	if err := recordAliasSyncFailure(context.Background(), nil, "id", "msg", time.Second); err == nil {
		t.Error("expected error on nil pool")
	}
}

// ---------------------------------------------------------------
// Worker — Run() exits cleanly on nil receiver / nil pool / nil
// sync. These are the only branches reachable without a pgx pool;
// processNext requires a live DB to exercise its claim path and
// is covered by integration tests.
// ---------------------------------------------------------------

func TestAliasStalwartSyncWorker_Run_NilReceiver(t *testing.T) {
	var w *AliasStalwartSyncWorker
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Should return immediately without panicking on the nil
	// receiver. We don't need the goroutine — the call is
	// synchronous when Run sees the bail-out condition.
	w.Run(ctx)
}

func TestAliasStalwartSyncWorker_Run_NilPool(t *testing.T) {
	w := NewAliasStalwartSyncWorker(nil, &recordingSync{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
}

func TestAliasStalwartSyncWorker_Run_NilSync(t *testing.T) {
	w := NewAliasStalwartSyncWorker(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
}

// recordingSync counts AddAlias / RemoveAlias calls and optionally
// returns a fixed error. Used by the inline-attempt tests once the
// integration suite spins up a pool, and by the worker's nil-pool
// branch test above.
type recordingSync struct {
	addCalls    atomic.Int64
	removeCalls atomic.Int64
	err         error
}

func (r *recordingSync) AddAlias(_ context.Context, _, _, _ string) error {
	r.addCalls.Add(1)
	return r.err
}

func (r *recordingSync) RemoveAlias(_ context.Context, _, _, _ string) error {
	r.removeCalls.Add(1)
	return r.err
}

// ---------------------------------------------------------------
// attemptAliasSyncInline — covers every in-process branch without
// needing a pool. The pool-touching branches (mark synced / record
// failure) error-out cleanly when the pool is nil, which is the
// behavior we assert here.
// ---------------------------------------------------------------

func TestAttemptAliasSyncInline_NoSync(t *testing.T) {
	s := &Service{}
	// No aliasSync wired → no-op, no panic, no Stalwart call.
	s.attemptAliasSyncInline(context.Background(), "tid", "qid", aliasSyncOpAdd, "acc", "a@b")
}

func TestAttemptAliasSyncInline_NoQueueID(t *testing.T) {
	rec := &recordingSync{}
	s := &Service{aliasSync: rec}
	// Empty queue id → no Stalwart call (the queue was skipped
	// because we have no sync wired upstream).
	s.attemptAliasSyncInline(context.Background(), "tid", "", aliasSyncOpAdd, "acc", "a@b")
	if rec.addCalls.Load() != 0 {
		t.Errorf("AddAlias called %d times, want 0 when queue id is empty", rec.addCalls.Load())
	}
}

func TestAttemptAliasSyncInline_UnknownOp(t *testing.T) {
	rec := &recordingSync{}
	s := &Service{aliasSync: rec}
	// Unknown op should log and return without touching Stalwart.
	s.attemptAliasSyncInline(context.Background(), "tid", "qid", aliasSyncOp("nope"), "acc", "a@b")
	if rec.addCalls.Load() != 0 || rec.removeCalls.Load() != 0 {
		t.Errorf("calls=%d/%d, want 0/0 for unknown op", rec.addCalls.Load(), rec.removeCalls.Load())
	}
}

func TestAttemptAliasSyncInline_AddSuccess_NilPool(t *testing.T) {
	rec := &recordingSync{}
	s := &Service{aliasSync: rec}
	// Stalwart call succeeds → mark synced runs against a nil
	// pool (errors out internally and is logged but the function
	// does not panic).
	s.attemptAliasSyncInline(context.Background(), "tid", "qid", aliasSyncOpAdd, "acc", "a@b")
	if rec.addCalls.Load() != 1 {
		t.Errorf("AddAlias called %d times, want 1", rec.addCalls.Load())
	}
}

func TestAttemptAliasSyncInline_RemoveSuccess_NilPool(t *testing.T) {
	rec := &recordingSync{}
	s := &Service{aliasSync: rec}
	s.attemptAliasSyncInline(context.Background(), "tid", "qid", aliasSyncOpRemove, "acc", "a@b")
	if rec.removeCalls.Load() != 1 {
		t.Errorf("RemoveAlias called %d times, want 1", rec.removeCalls.Load())
	}
}

func TestAttemptAliasSyncInline_StalwartFails_NilPool(t *testing.T) {
	rec := &recordingSync{err: errors.New("stalwart down")}
	s := &Service{aliasSync: rec}
	// Stalwart sync errors → recordAliasSyncFailure runs against
	// a nil pool; the function logs and returns. The contract is
	// "never panic, never bubble up", which is what we exercise.
	s.attemptAliasSyncInline(context.Background(), "tid", "qid", aliasSyncOpAdd, "acc", "a@b")
	if rec.addCalls.Load() != 1 {
		t.Errorf("AddAlias called %d times, want 1", rec.addCalls.Load())
	}
}

// ---------------------------------------------------------------
// Worker tick loop — per-tick batch cap and inter-call delay.
//
// `tickLoop` is the body of `tick()` with the per-row work
// injected as a function. That lets us assert the loop semantics
// (cap enforced, no cap when disabled, context cancellation
// honored, error short-circuit) without spinning up a pgx pool
// for `processNext`. The Stalwart-touching path is covered by
// the integration suite.
// ---------------------------------------------------------------

func newTestAliasWorker() *AliasStalwartSyncWorker {
	return &AliasStalwartSyncWorker{
		logger:         log.New(io.Discard, "", 0),
		interval:       30 * time.Second,
		batchCap:       DefaultAliasSyncBatchCap,
		interCallDelay: DefaultAliasSyncInterCallDelay,
	}
}

func TestAliasStalwartSyncWorker_TickLoop_BatchCapEnforced(t *testing.T) {
	w := newTestAliasWorker().WithBatchCap(3)
	var calls int
	w.tickLoop(context.Background(), func(_ context.Context) (bool, error) {
		calls++
		// Always more work available — the cap is the only thing
		// that should stop the loop.
		return true, nil
	})
	if calls != 3 {
		t.Errorf("processNext invoked %d times, want 3 (batch cap)", calls)
	}
}

func TestAliasStalwartSyncWorker_TickLoop_StopsOnEmptyQueue(t *testing.T) {
	w := newTestAliasWorker().WithBatchCap(10)
	var calls int
	w.tickLoop(context.Background(), func(_ context.Context) (bool, error) {
		calls++
		// Three rows of work, then "queue empty".
		return calls < 3, nil
	})
	if calls != 3 {
		t.Errorf("processNext invoked %d times, want 3 (empty queue stop)", calls)
	}
}

func TestAliasStalwartSyncWorker_TickLoop_StopsOnError(t *testing.T) {
	w := newTestAliasWorker().WithBatchCap(10)
	var calls int
	boom := errors.New("boom")
	w.tickLoop(context.Background(), func(_ context.Context) (bool, error) {
		calls++
		if calls == 2 {
			return false, boom
		}
		return true, nil
	})
	if calls != 2 {
		t.Errorf("processNext invoked %d times, want 2 (error stop)", calls)
	}
}

func TestAliasStalwartSyncWorker_TickLoop_BatchCapDisabled(t *testing.T) {
	// A non-positive batch cap disables the cap entirely; the
	// loop runs until the work function reports an empty queue.
	w := newTestAliasWorker().WithBatchCap(0)
	var calls int
	w.tickLoop(context.Background(), func(_ context.Context) (bool, error) {
		calls++
		return calls < 250, nil
	})
	if calls != 250 {
		t.Errorf("processNext invoked %d times, want 250 (cap disabled)", calls)
	}
}

func TestAliasStalwartSyncWorker_TickLoop_InterCallDelay(t *testing.T) {
	// With a 10ms inter-call delay and a cap of 4, the loop
	// inserts the delay 3 times (between calls 1↔2, 2↔3, 3↔4)
	// so the total runtime is at least 30ms. We assert ≥ 20ms
	// to keep the test resilient to coarse OS scheduler tick
	// resolution while still confirming the delay is in fact
	// applied between calls (and not before the first or after
	// the last).
	w := newTestAliasWorker().WithBatchCap(4).WithInterCallDelay(10 * time.Millisecond)
	var calls int
	start := time.Now()
	w.tickLoop(context.Background(), func(_ context.Context) (bool, error) {
		calls++
		return true, nil
	})
	elapsed := time.Since(start)
	if calls != 4 {
		t.Errorf("processNext invoked %d times, want 4", calls)
	}
	if elapsed < 20*time.Millisecond {
		t.Errorf("tickLoop ran in %v, want ≥ 20ms (inter-call delay applied)", elapsed)
	}
}

func TestAliasStalwartSyncWorker_TickLoop_ContextCancelDuringDelay(t *testing.T) {
	// Confirm that a context cancellation during the inter-call
	// delay returns promptly instead of waiting out the full
	// delay window. We use a 1-hour delay so any return inside
	// the test's deadline is conclusive evidence the select
	// woke on ctx.Done().
	//
	// `calls` is an atomic so both the success path (Load after
	// <-done, with a happens-before via close(done)) AND the
	// timeout path (Fatalf's read while the goroutine may still
	// be blocked in the select) are race-free under `go test
	// -race`. The Load in the timeout branch is the diagnostic
	// the test prints — making it atomic avoids relying on
	// "the goroutine isn't actually writing right now" for
	// memory-model correctness.
	w := newTestAliasWorker().WithBatchCap(10).WithInterCallDelay(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.tickLoop(ctx, func(_ context.Context) (bool, error) {
			n := calls.Add(1)
			if n == 1 {
				// Cancel right after the first call. The loop
				// will then sleep for the inter-call delay and
				// must wake on ctx.Done() instead of waiting
				// the full hour.
				cancel()
			}
			return true, nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("tickLoop did not return promptly on context cancel; calls=%d", calls.Load())
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("processNext invoked %d times, want 1 (canceled during delay)", got)
	}
}
