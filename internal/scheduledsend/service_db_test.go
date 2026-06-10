package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// newDBService builds a real Service over the test pool plus a
// seeded tenant. nowFunc lets a caller move the service clock into
// the past so Schedule-validated rows land with a `send_at` that is
// already due from the DB's real `now()` perspective (used by the
// worker integration test).
func newDBService(t *testing.T, nowFunc func() time.Time) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc, err := NewService(Config{Pool: pool, NowFunc: nowFunc})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, tenant
}

func dbScheduleInput(tenant string, sendAt time.Time, user string) ScheduleInput {
	return ScheduleInput{
		TenantID:          tenant,
		KChatUserID:       user,
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		IdentityID:        "ident-1",
		SubmissionPayload: json.RawMessage(`{"emailId":"email-1","identityId":"ident-1"}`),
		SendAt:            sendAt,
	}
}

// TestServiceScheduleGetListCancelDB exercises the full per-user CRUD
// surface against Postgres: Schedule → Get → ListByUser → Cancel,
// plus the cross-user / cross-tenant authz fences and the
// idempotent double-cancel path.
func TestServiceScheduleGetListCancelDB(t *testing.T) {
	svc, tenant := newDBService(t, nil)
	ctx := context.Background()
	now := time.Now()

	ss, err := svc.Schedule(ctx, dbScheduleInput(tenant, now.Add(10*time.Minute), "user-1"))
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if ss.ID == "" || ss.Status != StatusPending {
		t.Fatalf("unexpected scheduled row: %+v", ss)
	}

	// Get round-trips the row for its owner.
	got, err := svc.Get(ctx, tenant, "user-1", ss.ID)
	if err != nil || got.ID != ss.ID {
		t.Fatalf("Get owner: %v %+v", err, got)
	}

	// A different user in the same tenant must NOT see it.
	if _, err := svc.Get(ctx, tenant, "user-2", ss.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user Get: want ErrNotFound got %v", err)
	}

	// ListByUser returns the row for the owner and nothing for a peer.
	rows, err := svc.ListByUser(ctx, tenant, "user-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListByUser owner: %v len=%d", err, len(rows))
	}
	if peer, err := svc.ListByUser(ctx, tenant, "user-2"); err != nil || len(peer) != 0 {
		t.Errorf("ListByUser peer: %v len=%d", err, len(peer))
	}

	// A peer cannot cancel the owner's send (surfaces as not-found).
	if err := svc.Cancel(ctx, tenant, "user-2", ss.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user Cancel: want ErrNotFound got %v", err)
	}

	// Owner cancels → success, second cancel is idempotent.
	if err := svc.Cancel(ctx, tenant, "user-1", ss.ID); err != nil {
		t.Fatalf("Cancel owner: %v", err)
	}
	if err := svc.Cancel(ctx, tenant, "user-1", ss.ID); !errors.Is(err, ErrAlreadyCancelled) {
		t.Errorf("double Cancel: want ErrAlreadyCancelled got %v", err)
	}

	// Get on an unknown id → ErrNotFound.
	if _, err := svc.Get(ctx, tenant, "user-1", "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: want ErrNotFound got %v", err)
	}
}

// TestNewHookDB covers the production NewHook constructor (which
// requires a real *Service) and its required-field validation.
func TestNewHookDB(t *testing.T) {
	svc, _ := newDBService(t, nil)
	resolver := func(context.Context, string, string) (string, error) { return "acct", nil }
	sub := &fakeSubmitter{resp: okSubmissionResponse()}

	h, err := NewHook(HookConfig{Service: svc, Forwarder: sub, AccountResolver: resolver})
	if err != nil || h == nil {
		t.Fatalf("NewHook: %v", err)
	}

	// Missing Service → error (not panic), since main.go chains hooks.
	if _, err := NewHook(HookConfig{Forwarder: sub, AccountResolver: resolver}); err == nil {
		t.Error("NewHook without Service should error")
	}
	// Missing forwarder / resolver also error.
	if _, err := NewHook(HookConfig{Service: svc, AccountResolver: resolver}); err == nil {
		t.Error("NewHook without Forwarder should error")
	}
	if _, err := NewHook(HookConfig{Service: svc, Forwarder: sub}); err == nil {
		t.Error("NewHook without AccountResolver should error")
	}
}

// TestWorkerRunLifecycleDB starts the worker's Run loop on a short
// interval and confirms it dispatches a due row and then returns
// cleanly when the context is cancelled.
func TestWorkerRunLifecycleDB(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	svc, tenant := newDBService(t, func() time.Time { return past })
	ctx, cancel := context.WithCancel(context.Background())

	ss, err := svc.Schedule(ctx, dbScheduleInput(tenant, past.Add(2*time.Minute), "user-1"))
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	sub := &fakeSubmitter{resp: okSubmissionResponse()}
	w, err := NewDispatchWorker(WorkerConfig{
		Service:  svc,
		Internal: sub,
		Interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Poll until the row is dispatched.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, gErr := svc.Get(context.Background(), tenant, "user-1", ss.ID)
		if gErr == nil && got.Status == StatusSent {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	got, err := svc.Get(context.Background(), tenant, "user-1", ss.ID)
	if err != nil || got.Status != StatusSent {
		t.Fatalf("row not dispatched by Run loop: %v status=%v", err, got.Status)
	}
}

// TestWorkerDispatchSuccessDB drives the real Service-backed worker
// store through a successful dispatch: claimDue picks the due row,
// the fake submitter accepts it, and markDispatched flips the row to
// `sent`.
func TestWorkerDispatchSuccessDB(t *testing.T) {
	// Service clock 2h in the past so the scheduled row's send_at
	// lands before the DB's real now() and is immediately due.
	past := time.Now().Add(-2 * time.Hour)
	svc, tenant := newDBService(t, func() time.Time { return past })
	ctx := context.Background()

	ss, err := svc.Schedule(ctx, dbScheduleInput(tenant, past.Add(2*time.Minute), "user-1"))
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	sub := &fakeSubmitter{resp: okSubmissionResponse()}
	w, err := NewDispatchWorker(WorkerConfig{Service: svc, Internal: sub, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}
	w.Tick(ctx)

	if sub.calls != 1 {
		t.Errorf("expected exactly 1 dispatch, got %d", sub.calls)
	}
	got, err := svc.Get(ctx, tenant, "user-1", ss.ID)
	if err != nil {
		t.Fatalf("Get after dispatch: %v", err)
	}
	if got.Status != StatusSent {
		t.Errorf("status=%q want sent", got.Status)
	}
	if got.SentAt == nil {
		t.Error("sent_at not populated after dispatch")
	}
}

// TestWorkerRetryThenFailDB drives the failure path end-to-end: a
// submitter that always errors first schedules a retry (status stays
// pending, next_retry_at pushed out, last_error recorded), and once
// attempts hit the budget the row flips to `failed`.
func TestWorkerRetryThenFailDB(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	svc, tenant := newDBService(t, func() time.Time { return past })
	ctx := context.Background()

	ss, err := svc.Schedule(ctx, dbScheduleInput(tenant, past.Add(2*time.Minute), "user-1"))
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	sub := &fakeSubmitter{err: errors.New("stalwart down")}
	// maxAttempts=1 so the first failure immediately dead-letters
	// the row via markFailed.
	w, err := NewDispatchWorker(WorkerConfig{Service: svc, Internal: sub, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}
	w.Tick(ctx)

	got, err := svc.Get(ctx, tenant, "user-1", ss.ID)
	if err != nil {
		t.Fatalf("Get after fail: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status=%q want failed", got.Status)
	}
	if got.LastError == "" {
		t.Error("last_error not recorded on failure")
	}
}

// TestWorkerSchedulesRetryDB verifies the retry (not dead-letter)
// branch: with a generous attempt budget the failing row stays
// pending with next_retry_at pushed into the future.
func TestWorkerSchedulesRetryDB(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	svc, tenant := newDBService(t, func() time.Time { return past })
	ctx := context.Background()

	ss, err := svc.Schedule(ctx, dbScheduleInput(tenant, past.Add(2*time.Minute), "user-1"))
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	sub := &fakeSubmitter{err: errors.New("transient")}
	w, err := NewDispatchWorker(WorkerConfig{Service: svc, Internal: sub, MaxAttempts: 5})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}
	w.Tick(ctx)

	got, err := svc.Get(ctx, tenant, "user-1", ss.ID)
	if err != nil {
		t.Fatalf("Get after retry: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("status=%q want still pending (retry scheduled)", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts=%d want 1", got.Attempts)
	}
	if !got.NextRetryAt.After(time.Now()) {
		t.Errorf("next_retry_at=%v should be in the future", got.NextRetryAt)
	}
}
