package tenant

import (
	"context"
	"errors"
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
	tests := []struct {
		nextAttempt int
		want        time.Duration
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
		if got := nextAliasSyncBackoff(tc.nextAttempt); got != tc.want {
			t.Errorf("nextAliasSyncBackoff(%d) = %v, want %v", tc.nextAttempt, got, tc.want)
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
