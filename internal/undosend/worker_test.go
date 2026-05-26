package undosend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

type fakeSubmitter struct {
	calls    atomic.Int32
	err      error
	respond  func(req jmap.JmapRequest) *jmap.JmapResponse
	lastReq  jmap.JmapRequest
}

func (f *fakeSubmitter) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.calls.Add(1)
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	if f.respond != nil {
		return f.respond(req), nil
	}
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"EmailSubmission/set", map[string]any{
				"accountId": "acct-a",
				"created": map[string]any{
					"submission": map[string]any{"id": "real-sub-1"},
				},
			}, "0"},
		},
	}, nil
}

func newTestWorker(t *testing.T, svc *Service, sub InternalSubmitter, now func() time.Time) *DispatchWorker {
	t.Helper()
	w, err := NewDispatchWorker(WorkerConfig{
		Service:     svc,
		Internal:    sub,
		Interval:    50 * time.Millisecond,
		MaxBatch:    50,
		MaxAttempts: 3,
		NowFunc:     now,
	})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}
	return w
}

func TestNewDispatchWorker_RequiresService(t *testing.T) {
	if _, err := NewDispatchWorker(WorkerConfig{Internal: &fakeSubmitter{}}); err == nil {
		t.Fatalf("expected error without Service")
	}
}

func TestNewDispatchWorker_RequiresInternal(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := NewDispatchWorker(WorkerConfig{Service: svc}); err == nil {
		t.Fatalf("expected error without Internal")
	}
}

func TestTick_DispatchesDueEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, mr, _ := newTestService(t, func(c *Config) {
		c.Delay = 1 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	sub := &fakeSubmitter{}
	w := newTestWorker(t, svc, sub, func() time.Time { return now.Add(5 * time.Second) })

	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		IdentityID:        "ident-1",
		SubmissionPayload: []byte(`{"emailId":"email-1","identityId":"ident-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	w.Tick(context.Background())

	if got := sub.calls.Load(); got != 1 {
		t.Fatalf("Dispatch calls = %d, want 1", got)
	}
	// Companion key should be gone (markDispatched).
	if mr.Exists(companionKey(ps.ID)) {
		t.Fatalf("companion key still exists after dispatch")
	}
	// Sorted set entry should be gone (claim).
	if _, err := mr.ZScore(sortedSetKey, ps.ID); err == nil {
		t.Fatalf("sorted set still present after dispatch")
	}
}

func TestTick_RewritesBackReferenceToRealEmailID(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, _, _ := newTestService(t, func(c *Config) {
		c.Delay = 1 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	sub := &fakeSubmitter{}
	w := newTestWorker(t, svc, sub, func() time.Time { return now.Add(5 * time.Second) })

	if _, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "REAL-email-1",
		CreateID:          "submission",
		// Stale back-reference; worker must overwrite with EmailID.
		SubmissionPayload: []byte(`{"emailId":"#draft","identityId":"ident-1"}`),
	}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	w.Tick(context.Background())

	got := sub.lastReq.MethodCalls[0][1].(map[string]any)
	create := got["create"].(map[string]any)
	sub1 := create["submission"].(map[string]any)
	if sub1["emailId"] != "REAL-email-1" {
		t.Fatalf("worker did not rewrite back-ref: %v", sub1)
	}
}

func TestTick_RetriesOnDispatchError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, mr, _ := newTestService(t, func(c *Config) {
		c.Delay = 1 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	sub := &fakeSubmitter{err: errors.New("stalwart down")}
	clock := atomic.Int64{}
	clock.Store(now.Add(5 * time.Second).Unix())
	w := newTestWorker(t, svc, sub, func() time.Time { return time.Unix(clock.Load(), 0) })

	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	w.Tick(context.Background())

	// First failure: row should be re-queued, companion key present.
	if !mr.Exists(companionKey(ps.ID)) {
		t.Fatalf("companion key gone after first failure")
	}
	score, err := mr.ZScore(sortedSetKey, ps.ID)
	if err != nil {
		t.Fatalf("sorted set after retry: %v", err)
	}
	if score <= float64(now.Unix()) {
		t.Fatalf("retry deadline not advanced: %f", score)
	}
}

func TestTick_FailsAfterMaxAttempts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, mr, _ := newTestService(t, func(c *Config) {
		c.Delay = 1 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	sub := &fakeSubmitter{err: errors.New("stalwart down")}
	clock := atomic.Int64{}
	clock.Store(now.Add(5 * time.Second).Unix())
	w := newTestWorker(t, svc, sub, func() time.Time { return time.Unix(clock.Load(), 0) })
	w.maxAttempts = 2

	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	w.Tick(context.Background()) // attempt 1: re-queue
	// Advance clock past the new deadline.
	clock.Store(now.Add(120 * time.Second).Unix())
	w.Tick(context.Background()) // attempt 2: mark failed

	if mr.Exists(companionKey(ps.ID)) {
		t.Fatalf("companion key still present after failure")
	}
	items, err := mr.List(failedListKey)
	if err != nil {
		t.Fatalf("List dead-letter: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("dead-letter len = %d, want 1", len(items))
	}
	var failed PendingSend
	if err := json.Unmarshal([]byte(items[0]), &failed); err != nil {
		t.Fatalf("decode dead-letter: %v", err)
	}
	if failed.Status != StatusFailed || !strings.Contains(failed.LastError, "stalwart down") {
		t.Fatalf("dead-letter shape: %+v", failed)
	}
}

func TestTick_StalwartNotCreatedSurfacesAsFailure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, mr, _ := newTestService(t, func(c *Config) {
		c.Delay = 1 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	sub := &fakeSubmitter{respond: func(jmap.JmapRequest) *jmap.JmapResponse {
		return &jmap.JmapResponse{
			MethodResponses: [][]any{
				{"EmailSubmission/set", map[string]any{
					"accountId": "acct-a",
					"notCreated": map[string]any{
						"submission": map[string]any{
							"type":        "tooManyKeywords",
							"description": "limit hit",
						},
					},
				}, "0"},
			},
		}
	}}
	w := newTestWorker(t, svc, sub, func() time.Time { return now.Add(5 * time.Second) })
	w.maxAttempts = 1

	if _, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	w.Tick(context.Background())
	items, _ := mr.List(failedListKey)
	if len(items) != 1 {
		t.Fatalf("notCreated should land in dead-letter, got %d items", len(items))
	}
}

func TestTick_ClaimRaceLoses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, _, _ := newTestService(t, func(c *Config) {
		c.Delay = 1 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	sub := &fakeSubmitter{}
	w := newTestWorker(t, svc, sub, func() time.Time { return now.Add(5 * time.Second) })

	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// Simulate another worker grabbing the row first.
	if owned, err := svc.claim(context.Background(), ps.ID); err != nil || !owned {
		t.Fatalf("pre-claim should succeed")
	}
	w.Tick(context.Background())
	if sub.calls.Load() != 0 {
		t.Fatalf("expected lost-race to skip dispatch; got %d", sub.calls.Load())
	}
}
