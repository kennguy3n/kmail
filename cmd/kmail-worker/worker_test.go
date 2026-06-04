package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/kmail/internal/config"
)

func testSupervisor(t *testing.T) *supervisor {
	t.Helper()
	sup := newSupervisor(newWorkerMetrics(prometheus.NewRegistry()), log.New(io.Discard, "", 0))
	sup.baseBackoff = time.Millisecond
	return sup
}

func TestHealthzHandlerReturns200(t *testing.T) {
	rec := httptest.NewRecorder()
	healthzHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Fatalf("healthz body = %q, want %q", got, "ok\n")
	}
}

func TestReadyzHandlerUnreachablePostgres(t *testing.T) {
	// Point the pool at a port nothing listens on so Ping fails
	// fast and the readiness probe reports 503.
	pool, err := pgxpool.New(context.Background(), "postgres://kmail:kmail@127.0.0.1:1/kmail")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	rec := httptest.NewRecorder()
	readyzHandler(pool)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestSupervisorGracefulStopDoesNotRestart(t *testing.T) {
	sup := testSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	var once sync.Once
	sup.start(ctx, workerRegistration{
		name: "blocker",
		run: func(ctx context.Context) {
			once.Do(func() { close(started) })
			<-ctx.Done() // well-behaved worker: returns on cancel
		},
	})

	<-started
	cancel()

	if !sup.wait(2 * time.Second) {
		t.Fatal("supervisor.wait timed out; worker did not stop on cancel")
	}
	if got := testutil.ToFloat64(sup.metrics.restarts.WithLabelValues("blocker")); got != 0 {
		t.Fatalf("restarts = %v, want 0 for a clean shutdown", got)
	}
	if got := testutil.ToFloat64(sup.metrics.up.WithLabelValues("blocker")); got != 0 {
		t.Fatalf("up gauge = %v, want 0 after stop", got)
	}
}

func TestSupervisorRestartsOnEarlyReturn(t *testing.T) {
	sup := testSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	blocked := make(chan struct{})
	var once sync.Once
	sup.start(ctx, workerRegistration{
		name: "flaky",
		run: func(ctx context.Context) {
			// First two invocations return immediately (simulating an
			// unexpected early exit); the third blocks until shutdown.
			if atomic.AddInt32(&calls, 1) >= 3 {
				once.Do(func() { close(blocked) })
				<-ctx.Done()
				return
			}
		},
	})

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("worker was not restarted up to its blocking invocation")
	}

	// Two early returns => two restarts recorded.
	if got := testutil.ToFloat64(sup.metrics.restarts.WithLabelValues("flaky")); got < 2 {
		t.Fatalf("restarts = %v, want >= 2", got)
	}

	cancel()
	if !sup.wait(2 * time.Second) {
		t.Fatal("supervisor.wait timed out after cancel")
	}
}

func TestSupervisorRecoversPanic(t *testing.T) {
	sup := testSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	blocked := make(chan struct{})
	var once sync.Once
	sup.start(ctx, workerRegistration{
		name: "panicky",
		run: func(ctx context.Context) {
			if atomic.AddInt32(&calls, 1) == 1 {
				panic("boom")
			}
			once.Do(func() { close(blocked) })
			<-ctx.Done()
		},
	})

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not recover and restart after panic")
	}

	if got := testutil.ToFloat64(sup.metrics.panics.WithLabelValues("panicky")); got != 1 {
		t.Fatalf("panics = %v, want 1", got)
	}

	cancel()
	if !sup.wait(2 * time.Second) {
		t.Fatal("supervisor.wait timed out after cancel")
	}
}

func TestSupervisorWaitTimesOut(t *testing.T) {
	sup := testSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	running := make(chan struct{})
	var once sync.Once
	sup.start(ctx, workerRegistration{
		name: "stubborn",
		run: func(ctx context.Context) {
			once.Do(func() { close(running) })
			time.Sleep(500 * time.Millisecond) // ignores ctx
		},
	})

	<-running
	if sup.wait(20 * time.Millisecond) {
		t.Fatal("wait returned true but the worker should still be running")
	}
}

func TestBuildWorkersBaselineRegistry(t *testing.T) {
	// No live backend required: pgxpool.New is lazy and buildWorkers
	// is construction-only (see the INVARIANT note on buildWorkers in
	// workers.go). The DSN below is never dialed, and passing
	// valkey == nil (ValkeyURL: "" / workerDeps.valkey: nil) skips the
	// one construction-time Valkey Ping in buildInternalJMAP — so this
	// test asserts purely on the registered worker set with nothing
	// listening. If buildWorkers ever gains a constructor that probes
	// Postgres (or unconditionally probes Valkey) at build time, that
	// invariant breaks and this test would need a real backend / stub.
	//
	// Clear the optional gates so the baseline (always-on) set is
	// deterministic regardless of the host environment.
	for _, k := range []string{
		"KMAIL_STALWART_ADMIN_USER",
		"KMAIL_MEILISEARCH_URL",
		"KMAIL_OPENSEARCH_URL",
		"KMAIL_QUOTA_WORKER_ENABLED",
		"KMAIL_CLAMAV_ADDR",
	} {
		t.Setenv(k, "")
	}

	pool, err := pgxpool.New(context.Background(), "postgres://kmail:kmail@127.0.0.1:5432/kmail")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	cfg := &config.Config{
		DatabaseURL: "postgres://kmail:kmail@127.0.0.1:5432/kmail",
		StalwartURL: "http://localhost:8080",
		Env:         "development",
		ValkeyURL:   "", // disables undo-send + shared breaker
	}

	regs, err := buildWorkers(context.Background(), workerDeps{
		cfg:    cfg,
		pool:   pool,
		valkey: nil,
		reg:    prometheus.NewRegistry(),
		logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}

	got := map[string]bool{}
	for _, r := range regs {
		if r.run == nil {
			t.Errorf("worker %q has nil run", r.name)
		}
		if got[r.name] {
			t.Errorf("duplicate worker registration %q", r.name)
		}
		got[r.name] = true
	}

	// Always-on workers (no optional env required).
	wantPresent := []string{
		"calendar-reminder",
		"scheduledsend-dispatch",
		"snooze-dispatch",
		"deliverability-alert-evaluator",
		"shard-health",
		"retention",
		"export",
		"adminproxy-expiry",
		"webhooks",
	}
	for _, name := range wantPresent {
		if !got[name] {
			t.Errorf("expected worker %q to be registered", name)
		}
	}

	// Gated-off workers must be absent with the cleared env / nil Valkey.
	wantAbsent := []string{
		"alias-stalwart-sync", // no KMAIL_STALWART_ADMIN_USER
		"undosend-dispatch",   // nil Valkey
		"billing-quota",       // KMAIL_QUOTA_WORKER_ENABLED unset
		"search-cutover",      // no search backends
	}
	for _, name := range wantAbsent {
		if got[name] {
			t.Errorf("did not expect worker %q to be registered", name)
		}
	}

	if len(regs) != len(wantPresent) {
		t.Fatalf("registered %d workers (%v), want %d (%v)", len(regs), keys(got), len(wantPresent), wantPresent)
	}
}

func TestBuildWorkersOptionalGatesEnabled(t *testing.T) {
	t.Setenv("KMAIL_STALWART_ADMIN_USER", "admin")
	t.Setenv("KMAIL_STALWART_ADMIN_PASS", "secret")
	t.Setenv("KMAIL_QUOTA_WORKER_ENABLED", "true")
	t.Setenv("KMAIL_MEILISEARCH_URL", "http://localhost:7700")
	t.Setenv("KMAIL_OPENSEARCH_URL", "http://localhost:9200")

	pool, err := pgxpool.New(context.Background(), "postgres://kmail:kmail@127.0.0.1:5432/kmail")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	cfg := &config.Config{
		DatabaseURL: "postgres://kmail:kmail@127.0.0.1:5432/kmail",
		StalwartURL: "http://localhost:8080",
		Env:         "development",
		ValkeyURL:   "", // keep Valkey off; undo-send stays gated
	}
	cfg.Billing.QuotaWorkerEnabled = true

	regs, err := buildWorkers(context.Background(), workerDeps{
		cfg:    cfg,
		pool:   pool,
		valkey: nil,
		reg:    prometheus.NewRegistry(),
		logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}

	got := map[string]bool{}
	for _, r := range regs {
		got[r.name] = true
	}
	for _, name := range []string{"alias-stalwart-sync", "billing-quota", "search-cutover"} {
		if !got[name] {
			t.Errorf("expected gated worker %q to be registered when its env is set", name)
		}
	}
}

func TestAdvanceBackoff(t *testing.T) {
	const base = time.Second
	const healthy = supervisorMaxBackoff

	// Consecutive rapid (unhealthy) exits escalate the delay,
	// doubling each time and capping at supervisorMaxBackoff.
	var prev time.Duration // first iteration carries base
	wantSeq := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, want := range wantSeq {
		delay, next := advanceBackoff(prev, base, 0 /* ran <0.1ms: unhealthy */, healthy)
		if delay != want*time.Second {
			t.Fatalf("iter %d: delay = %s, want %s", i, delay, want*time.Second)
		}
		prev = next
	}

	// A run that lasted at least healthyResetAfter resets the streak:
	// even after a long escalated backoff, the next delay drops to base.
	delay, next := advanceBackoff(supervisorMaxBackoff, base, healthy, healthy)
	if delay != base {
		t.Fatalf("healthy run: delay = %s, want %s (reset to base)", delay, base)
	}
	if next != 2*base {
		t.Fatalf("healthy run: next = %s, want %s", next, 2*base)
	}

	// A run just under the threshold does NOT reset.
	delay, _ = advanceBackoff(8*time.Second, base, healthy-time.Nanosecond, healthy)
	if delay != 8*time.Second {
		t.Fatalf("sub-threshold run: delay = %s, want 8s (no reset)", delay)
	}

	// Defensive: a non-positive base falls back to one second.
	delay, _ = advanceBackoff(0, 0, 0, healthy)
	if delay != time.Second {
		t.Fatalf("zero base: delay = %s, want 1s", delay)
	}
}

func TestGetenvHelpers(t *testing.T) {
	t.Setenv("KMAIL_WORKER_TEST_DUR", "45s")
	if got := getenvDuration("KMAIL_WORKER_TEST_DUR", time.Hour); got != 45*time.Second {
		t.Fatalf("getenvDuration = %v, want 45s", got)
	}
	if got := getenvDuration("KMAIL_WORKER_TEST_MISSING", time.Hour); got != time.Hour {
		t.Fatalf("getenvDuration fallback = %v, want 1h", got)
	}
	t.Setenv("KMAIL_WORKER_TEST_BADDUR", "not-a-duration")
	if got := getenvDuration("KMAIL_WORKER_TEST_BADDUR", 5*time.Second); got != 5*time.Second {
		t.Fatalf("getenvDuration bad value = %v, want fallback 5s", got)
	}

	t.Setenv("KMAIL_WORKER_TEST_STR", "value")
	if got := getenvString("KMAIL_WORKER_TEST_STR", "fallback"); got != "value" {
		t.Fatalf("getenvString = %q, want %q", got, "value")
	}
	if got := getenvString("KMAIL_WORKER_TEST_STR_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("getenvString fallback = %q, want %q", got, "fallback")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
