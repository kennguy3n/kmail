package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// fakeStore is an in-memory workerStore for tests. It serves
// rows in FIFO claim order and records every state transition
// so the assertions can verify the worker's bookkeeping without
// a real Postgres.
type fakeStore struct {
	mu          sync.Mutex
	pending     []*ScheduledSend
	dispatched  map[string]time.Time
	failed      map[string]string
	retries     map[string]time.Time
	retryErrs   map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		dispatched: make(map[string]time.Time),
		failed:     make(map[string]string),
		retries:    make(map[string]time.Time),
		retryErrs:  make(map[string]string),
	}
}

func (f *fakeStore) push(ss *ScheduledSend) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, ss)
}

func (f *fakeStore) claimDue(_ context.Context) (*ScheduledSend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, ErrNotFound
	}
	ss := f.pending[0]
	f.pending = f.pending[1:]
	ss.Attempts++
	return ss, nil
}

func (f *fakeStore) markDispatched(_ context.Context, id string, sentAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatched[id] = sentAt
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
	mu       sync.Mutex
	calls    int
	err      error
	resp     *jmap.JmapResponse
	lastReq  jmap.JmapRequest
	tenantID string
}

func (f *fakeSubmitter) Dispatch(_ context.Context, tenantID, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	f.tenantID = tenantID
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func okSubmissionResponse() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"EmailSubmission/set", map[string]any{
				"accountId": "acct-a",
				"created": map[string]any{
					"submission": map[string]any{"id": "sub-1"},
				},
			}, "0"},
		},
	}
}

func notCreatedResponse() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"EmailSubmission/set", map[string]any{
				"accountId":  "acct-a",
				"notCreated": map[string]any{"submission": map[string]any{"type": "forbidden", "description": "blocked"}},
			}, "0"},
		},
	}
}

func sampleScheduledSend(id string) *ScheduledSend {
	return &ScheduledSend{
		ID:                id,
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		IdentityID:        "ident-1",
		SubmissionPayload: json.RawMessage(`{"emailId":"email-1","identityId":"ident-1"}`),
		SendAt:            time.Now().Add(-time.Second),
		Status:            StatusPending,
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
	store.push(sampleScheduledSend("ss-1"))
	sub := &fakeSubmitter{resp: okSubmissionResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if sub.calls != 1 {
		t.Fatalf("expected 1 dispatch, got %d", sub.calls)
	}
	if _, ok := store.dispatched["ss-1"]; !ok {
		t.Fatalf("expected markDispatched on ss-1")
	}
	if _, ok := store.failed["ss-1"]; ok {
		t.Fatalf("did not expect markFailed on happy path")
	}
}

func TestWorker_TransientErrorSchedulesRetry(t *testing.T) {
	store := newFakeStore()
	ss := sampleScheduledSend("ss-1")
	store.push(ss)
	sub := &fakeSubmitter{err: errors.New("connect refused")}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if _, ok := store.dispatched["ss-1"]; ok {
		t.Fatalf("did not expect markDispatched on error path")
	}
	if _, ok := store.retries["ss-1"]; !ok {
		t.Fatalf("expected scheduleRetry on transient error")
	}
	if msg := store.retryErrs["ss-1"]; msg == "" {
		t.Fatalf("expected non-empty retry error message")
	}
}

func TestWorker_ExhaustedAttemptsMarksFailed(t *testing.T) {
	store := newFakeStore()
	ss := sampleScheduledSend("ss-1")
	// Simulate a row that has already retried up to one shy of
	// the cap.
	ss.Attempts = DefaultMaxAttempts - 1
	store.push(ss)
	sub := &fakeSubmitter{err: errors.New("connect refused")}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if _, ok := store.failed["ss-1"]; !ok {
		t.Fatalf("expected markFailed once attempts hit cap")
	}
	if _, ok := store.retries["ss-1"]; ok {
		t.Fatalf("did not expect scheduleRetry after cap")
	}
}

func TestWorker_NotCreatedTreatedAsError(t *testing.T) {
	store := newFakeStore()
	store.push(sampleScheduledSend("ss-1"))
	sub := &fakeSubmitter{resp: notCreatedResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if _, ok := store.dispatched["ss-1"]; ok {
		t.Fatalf("did not expect markDispatched when notCreated is non-empty")
	}
	if _, ok := store.retries["ss-1"]; !ok {
		t.Fatalf("expected scheduleRetry on notCreated path")
	}
}

func TestWorker_TickDrainsToEmpty(t *testing.T) {
	store := newFakeStore()
	for i := 0; i < 3; i++ {
		store.push(sampleScheduledSend("ss-" + string(rune('a'+i))))
	}
	sub := &fakeSubmitter{resp: okSubmissionResponse()}
	w := newWorker(t, store, sub, DefaultMaxAttempts)

	w.Tick(context.Background())

	if sub.calls != 3 {
		t.Fatalf("expected 3 dispatches in one tick, got %d", sub.calls)
	}
	if len(store.dispatched) != 3 {
		t.Fatalf("expected 3 dispatched entries, got %d", len(store.dispatched))
	}
}

func TestBuildSubmissionCreate_OverwritesEmailID(t *testing.T) {
	ss := &ScheduledSend{
		EmailID:           "real-email-1",
		IdentityID:        "ident-1",
		SubmissionPayload: json.RawMessage(`{"emailId":"#draft","identityId":"old-ident"}`),
	}
	create, err := buildSubmissionCreate(ss)
	if err != nil {
		t.Fatalf("buildSubmissionCreate: %v", err)
	}
	if create["emailId"] != "real-email-1" {
		t.Fatalf("emailId = %v, want real-email-1", create["emailId"])
	}
	if create["identityId"] != "ident-1" {
		t.Fatalf("identityId = %v, want ident-1", create["identityId"])
	}
}

func TestBuildSubmissionCreate_EmptyPayloadErrors(t *testing.T) {
	ss := &ScheduledSend{}
	_, err := buildSubmissionCreate(ss)
	if err == nil {
		t.Fatalf("expected error for empty payload")
	}
}
