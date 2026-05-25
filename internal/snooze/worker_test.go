package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// fakeStore is an in-memory workerStore for tests.
type fakeStore struct {
	mu         sync.Mutex
	pending    []*Snooze
	unsnoozed  map[string]time.Time
	failed     map[string]string
	retries    map[string]time.Time
	retryErrs  map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		unsnoozed: make(map[string]time.Time),
		failed:    make(map[string]string),
		retries:   make(map[string]time.Time),
		retryErrs: make(map[string]string),
	}
}

func (f *fakeStore) push(s *Snooze) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, s)
}

func (f *fakeStore) claimDue(_ context.Context) (*Snooze, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, ErrNotFound
	}
	s := f.pending[0]
	f.pending = f.pending[1:]
	s.Attempts++
	return s, nil
}

func (f *fakeStore) markUnsnoozed(_ context.Context, id string, wokenAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsnoozed[id] = wokenAt
	return nil
}

func (f *fakeStore) markFailed(_ context.Context, id, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = lastErr
	return nil
}

func (f *fakeStore) scheduleRetry(_ context.Context, id string, nextRetryAt time.Time, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries[id] = nextRetryAt
	f.retryErrs[id] = lastErr
	return nil
}

// fakeSubmitter records dispatches and returns a canned response.
type fakeSubmitter struct {
	mu      sync.Mutex
	calls   int
	err     error
	resp    *jmap.JmapResponse
	lastReq jmap.JmapRequest
}

func (f *fakeSubmitter) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func okEmailSetResponse() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Email/set", map[string]any{
				"accountId": "acct-a",
				"updated":   map[string]any{"email-1": nil},
			}, "wake"},
		},
	}
}

func notUpdatedResponse() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Email/set", map[string]any{
				"accountId":  "acct-a",
				"notUpdated": map[string]any{"email-1": map[string]any{"type": "notFound", "description": "vanished"}},
			}, "wake"},
		},
	}
}

func sampleSnooze(id string) *Snooze {
	return &Snooze{
		ID:                 id,
		TenantID:           "tenant-a",
		KChatUserID:        "kchat-a",
		StalwartAccountID:  "acct-a",
		EmailID:            "email-1",
		OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
		SnoozedMailboxID:   "mb-snoozed",
		SnoozeUntil:        time.Now().Add(-time.Second),
		MarkUnreadOnWake:   true,
		Status:             StatusSnoozed,
	}
}

func newWorker(t *testing.T, store workerStore, submitter InternalSubmitter, maxAttempts int) *DispatchWorker {
	t.Helper()
	w, err := newDispatchWorkerWithStore(WorkerConfig{
		Internal:    submitter,
		MaxAttempts: maxAttempts,
		NowFunc:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	}, store)
	if err != nil {
		t.Fatalf("newDispatchWorkerWithStore: %v", err)
	}
	return w
}

func TestWorker_HappyPath(t *testing.T) {
	store := newFakeStore()
	store.push(sampleSnooze("s-1"))
	sub := &fakeSubmitter{resp: okEmailSetResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if sub.calls != 1 {
		t.Fatalf("expected 1 dispatch, got %d", sub.calls)
	}
	if _, ok := store.unsnoozed["s-1"]; !ok {
		t.Fatalf("expected markUnsnoozed on s-1")
	}
	if _, ok := store.failed["s-1"]; ok {
		t.Fatalf("did not expect markFailed on happy path")
	}
}

func TestWorker_PatchShape(t *testing.T) {
	store := newFakeStore()
	s := sampleSnooze("s-1")
	s.OriginalMailboxIDs = json.RawMessage(`{"mb-inbox":true,"mb-imp":true}`)
	store.push(s)
	sub := &fakeSubmitter{resp: okEmailSetResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if len(sub.lastReq.MethodCalls) != 1 {
		t.Fatalf("expected 1 method call, got %d", len(sub.lastReq.MethodCalls))
	}
	args, _ := sub.lastReq.MethodCalls[0][1].(map[string]any)
	update, _ := args["update"].(map[string]any)
	patch, _ := update["email-1"].(map[string]any)
	if got, want := patch["mailboxIds/mb-snoozed"], any(nil); got != want {
		t.Errorf("expected snoozed mailbox to be unset, got %v", got)
	}
	if got, want := patch["mailboxIds/mb-inbox"], any(true); got != want {
		t.Errorf("expected mb-inbox restored, got %v", got)
	}
	if got, want := patch["mailboxIds/mb-imp"], any(true); got != want {
		t.Errorf("expected mb-imp restored, got %v", got)
	}
	if got, want := patch["keywords/$seen"], any(nil); got != want {
		t.Errorf("expected $seen cleared, got %v", got)
	}
}

func TestWorker_MarkUnreadFalseOmitsSeenClear(t *testing.T) {
	store := newFakeStore()
	s := sampleSnooze("s-1")
	s.MarkUnreadOnWake = false
	store.push(s)
	sub := &fakeSubmitter{resp: okEmailSetResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	args, _ := sub.lastReq.MethodCalls[0][1].(map[string]any)
	update, _ := args["update"].(map[string]any)
	patch, _ := update["email-1"].(map[string]any)
	if _, has := patch["keywords/$seen"]; has {
		t.Fatalf("expected $seen NOT to be touched when MarkUnreadOnWake=false")
	}
}

func TestWorker_TransientErrorSchedulesRetry(t *testing.T) {
	store := newFakeStore()
	store.push(sampleSnooze("s-1"))
	sub := &fakeSubmitter{err: errors.New("connect refused")}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if _, ok := store.unsnoozed["s-1"]; ok {
		t.Fatalf("did not expect markUnsnoozed on error path")
	}
	if _, ok := store.retries["s-1"]; !ok {
		t.Fatalf("expected scheduleRetry on transient error")
	}
}

func TestWorker_NotUpdatedTreatedAsError(t *testing.T) {
	store := newFakeStore()
	store.push(sampleSnooze("s-1"))
	sub := &fakeSubmitter{resp: notUpdatedResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if _, ok := store.unsnoozed["s-1"]; ok {
		t.Fatalf("did not expect markUnsnoozed when stalwart refused")
	}
	if _, ok := store.retries["s-1"]; !ok {
		t.Fatalf("expected scheduleRetry when stalwart refused")
	}
}

func TestWorker_ExhaustedAttemptsMarksFailed(t *testing.T) {
	store := newFakeStore()
	s := sampleSnooze("s-1")
	s.Attempts = DefaultMaxAttempts - 1
	store.push(s)
	sub := &fakeSubmitter{err: errors.New("dial timeout")}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if msg, ok := store.failed["s-1"]; !ok || msg == "" {
		t.Fatalf("expected markFailed on last attempt, got failed=%v", store.failed)
	}
	if _, ok := store.retries["s-1"]; ok {
		t.Fatalf("did not expect retry after attempts exhausted")
	}
}

func TestWorker_EmptyQueueIsNoop(t *testing.T) {
	store := newFakeStore()
	sub := &fakeSubmitter{resp: okEmailSetResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if sub.calls != 0 {
		t.Fatalf("expected 0 dispatches on empty queue, got %d", sub.calls)
	}
}

func TestSnoozeBackoff_Progression(t *testing.T) {
	if got := snoozeBackoff(1); got != time.Minute {
		t.Errorf("attempts=1 backoff want 1m got %v", got)
	}
	if got := snoozeBackoff(2); got != 5*time.Minute {
		t.Errorf("attempts=2 backoff want 5m got %v", got)
	}
	if got := snoozeBackoff(3); got != 30*time.Minute {
		t.Errorf("attempts=3 backoff want 30m got %v", got)
	}
	if got := snoozeBackoff(99); got != 30*time.Minute {
		t.Errorf("attempts=99 backoff want 30m (capped) got %v", got)
	}
}

func TestBuildWakePatch_RejectsBadJSON(t *testing.T) {
	s := &Snooze{
		OriginalMailboxIDs: json.RawMessage(`not json`),
		SnoozedMailboxID:   "mb-snoozed",
	}
	if _, err := buildWakePatch(s); err == nil {
		t.Fatalf("expected error for malformed mailbox ids")
	}
}

func TestBuildWakePatch_RejectsEmpty(t *testing.T) {
	s := &Snooze{
		OriginalMailboxIDs: json.RawMessage(`{}`),
		SnoozedMailboxID:   "mb-snoozed",
	}
	if _, err := buildWakePatch(s); err == nil {
		t.Fatalf("expected error for empty mailbox ids")
	}
}
