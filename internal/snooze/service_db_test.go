package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func dbService(t *testing.T, now func() time.Time) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc, err := NewService(Config{Pool: pool, Logger: log.New(io.Discard, "", 0), NowFunc: now})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, tenant
}

func sampleInput(tenant string, until time.Time) SnoozeInput {
	return SnoozeInput{
		TenantID:           tenant,
		KChatUserID:        "kchat-" + tenant[:8],
		StalwartAccountID:  "acct-" + tenant[:8],
		EmailID:            "email-1",
		OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
		SnoozedMailboxID:   "mb-snoozed",
		SnoozeUntil:        until,
		MarkUnreadOnWake:   true,
	}
}

// TestSnoozeLifecycleDB covers Snooze → Get → ListByUser → Cancel
// against a live DB, including the per-user authz fence and the
// duplicate-active guard.
func TestSnoozeLifecycleDB(t *testing.T) {
	svc, tenant := dbService(t, time.Now)
	ctx := context.Background()
	in := sampleInput(tenant, time.Now().Add(time.Hour))

	row, err := svc.Snooze(ctx, in)
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	if row.Status != StatusSnoozed {
		t.Errorf("status=%q want %q", row.Status, StatusSnoozed)
	}

	// Duplicate active snooze on the same email → ErrAlreadySnoozed.
	if _, err := svc.Snooze(ctx, in); !errors.Is(err, ErrAlreadySnoozed) {
		t.Errorf("duplicate Snooze=%v want ErrAlreadySnoozed", err)
	}

	// Get by owner → match.
	got, err := svc.Get(ctx, tenant, in.KChatUserID, row.ID)
	if err != nil || got.ID != row.ID {
		t.Fatalf("Get owner=%v row=%+v", err, got)
	}

	// Cross-user Get → ErrNotFound (per-user authz fence).
	if _, err := svc.Get(ctx, tenant, "someone-else", row.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user Get=%v want ErrNotFound", err)
	}

	// ListByUser → contains the row.
	list, err := svc.ListByUser(ctx, tenant, in.KChatUserID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByUser len=%d err=%v", len(list), err)
	}

	// Cancel → ok; second Cancel → ErrAlreadyCancelled.
	if err := svc.Cancel(ctx, tenant, in.KChatUserID, row.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := svc.Cancel(ctx, tenant, in.KChatUserID, row.ID); !errors.Is(err, ErrAlreadyCancelled) {
		t.Errorf("double Cancel=%v want ErrAlreadyCancelled", err)
	}
}

// TestSnoozeWorkerTickDB exercises the real Service store methods
// (claimDue, markUnsnoozed, scanRow) through a worker tick: a row
// whose snooze_until is in the past is claimed, dispatched, and
// marked unsnoozed.
func TestSnoozeWorkerTickDB(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	svc, tenant := dbService(t, func() time.Time { return past })
	ctx := context.Background()

	// snooze_until just past the fixed "now" so validation passes
	// but the stored timestamp is due against SQL now().
	in := sampleInput(tenant, past.Add(2*MinSnoozeHorizon))
	row, err := svc.Snooze(ctx, in)
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}

	sub := &fakeSubmitter{resp: okEmailSetResponse()}
	w, err := NewDispatchWorker(WorkerConfig{
		Service: svc, Internal: sub, Logger: log.New(io.Discard, "", 0),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}
	w.Tick(ctx)

	if sub.calls == 0 {
		t.Fatal("worker never dispatched the due snooze")
	}
	got, err := svc.Get(ctx, tenant, in.KChatUserID, row.ID)
	if err != nil {
		t.Fatalf("Get after tick: %v", err)
	}
	if got.Status != StatusUnsnoozed {
		t.Errorf("status after wake=%q want %q", got.Status, StatusUnsnoozed)
	}
	if got.WokenAt == nil {
		t.Error("WokenAt should be stamped after wake")
	}
}

// TestSnoozeHandlersRegisterAndWorkerRunDB covers NewHandlers/Register
// (production constructors) and the worker Run loop's ctx-cancel exit.
func TestSnoozeHandlersRegisterAndWorkerRunDB(t *testing.T) {
	svc, _ := dbService(t, time.Now)
	sub := &fakeSubmitter{resp: okEmailSetResponse()}

	h := NewHandlers(svc, &fakeDispatcher{}, log.New(io.Discard, "", 0))
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{DevBypassToken: "x", Env: middleware.EnvDevelopment})
	mux := http.NewServeMux()
	h.Register(mux, authMW)
	for _, p := range []string{"/api/v1/snooze", "/api/v1/snoozed"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s not mounted (404)", p)
		}
	}

	w, err := NewDispatchWorker(WorkerConfig{
		Service: svc, Internal: sub, Logger: log.New(io.Discard, "", 0),
		Interval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestSnoozeHandlersE2EDB drives list/get/wakeNow against the real
// Service so the handler success paths and toResponse run end-to-end.
func TestSnoozeHandlersE2EDB(t *testing.T) {
	svc, tenant := dbService(t, time.Now)
	ctx := context.Background()
	in := sampleInput(tenant, time.Now().Add(time.Hour))
	row, err := svc.Snooze(ctx, in)
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}

	fd := &fakeDispatcher{}
	h := newHandlersWith(svc, fd, time.Now)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snoozed", h.list)
	mux.HandleFunc("GET /api/v1/snoozed/{id}", h.get)
	mux.HandleFunc("DELETE /api/v1/snoozed/{id}", h.wakeNow)

	authed := func(method, target string) *http.Request {
		r := httptest.NewRequest(method, target, nil)
		c := middleware.WithTenantID(r.Context(), tenant)
		c = middleware.WithKChatUserID(c, in.KChatUserID)
		return r.WithContext(c)
	}

	// list → 200 containing the row.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authed(http.MethodGet, "/api/v1/snoozed"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), row.ID) {
		t.Fatalf("list=%d body=%s", rec.Code, rec.Body.String())
	}

	// get → 200.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authed(http.MethodGet, "/api/v1/snoozed/"+row.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("get=%d body=%s", rec.Code, rec.Body.String())
	}

	// get unknown id → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authed(http.MethodGet, "/api/v1/snoozed/00000000-0000-0000-0000-000000000000"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("get missing=%d want 404", rec.Code)
	}

	// wakeNow → 200, dispatch fired, row flips to cancelled.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authed(http.MethodDelete, "/api/v1/snoozed/"+row.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("wakeNow=%d body=%s", rec.Code, rec.Body.String())
	}
	if fd.calls == 0 {
		t.Error("wakeNow should have dispatched a JMAP move")
	}
	got, _ := svc.Get(ctx, tenant, in.KChatUserID, row.ID)
	if got != nil && got.Status != StatusCancelled {
		t.Errorf("status after wakeNow=%q want %q", got.Status, StatusCancelled)
	}
}

// TestSnoozeWorkerRetryAndFailDB covers scheduleRetry and markFailed
// on the real store: a dispatch that always errors retries until
// MaxAttempts, then terminally fails.
func TestSnoozeWorkerRetryAndFailDB(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	svc, tenant := dbService(t, func() time.Time { return past })
	ctx := context.Background()

	in := sampleInput(tenant, past.Add(2*MinSnoozeHorizon))
	row, err := svc.Snooze(ctx, in)
	if err != nil {
		t.Fatalf("Snooze: %v", err)
	}

	// Transient dispatch error → scheduleRetry path. Use a NowFunc
	// in the past so the retry's next_retry_at stays <= SQL now()
	// and the row remains immediately re-claimable each tick.
	sub := &fakeSubmitter{err: errors.New("stalwart 503")}
	w, err := NewDispatchWorker(WorkerConfig{
		Service: svc, Internal: sub, Logger: log.New(io.Discard, "", 0),
		MaxAttempts: 2,
		NowFunc:     func() time.Time { return past },
	})
	if err != nil {
		t.Fatalf("NewDispatchWorker: %v", err)
	}

	// Two ticks: first schedules a retry, second hits MaxAttempts
	// and marks the row failed.
	w.Tick(ctx)
	w.Tick(ctx)

	got, err := svc.Get(ctx, tenant, in.KChatUserID, row.ID)
	if err != nil {
		t.Fatalf("Get after retries: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status=%q want %q (attempts=%d)", got.Status, StatusFailed, got.Attempts)
	}
	if got.LastError == "" {
		t.Error("LastError should record the dispatch failure")
	}
}
