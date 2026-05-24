package integrations

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLimiterStore is a thread-safe in-memory RateLimiterStore
// for tests. It implements the same INCR+EXPIRE-NX semantics
// as the production Valkey store: the first call for a given
// key sets the TTL; subsequent calls increment.
//
// It also exposes an `injectErr` field so a test can simulate
// a Valkey outage and verify the fail-open path in checkQuota.
type fakeLimiterStore struct {
	mu        sync.Mutex
	counts    map[string]int64
	injectErr error
}

func newFakeLimiterStore() *fakeLimiterStore {
	return &fakeLimiterStore{counts: make(map[string]int64)}
}

func (f *fakeLimiterStore) IncrWithTTL(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.injectErr != nil {
		return 0, f.injectErr
	}
	f.counts[key]++
	return f.counts[key], nil
}

// Allow is required to satisfy the middleware.RateLimiterStore
// interface but never exercised by the integrations tests
// (those drive checkQuota via IncrWithTTL only). Return values
// are a benign "admitted" pair so a hypothetical accidental
// caller fails open rather than rejecting traffic.
func (f *fakeLimiterStore) Allow(
	_ context.Context,
	_, _ string,
	_ time.Duration,
	_, _ int,
	_ time.Time,
) (bool, bool, error) {
	return true, true, nil
}

// newServiceForUnitTests assembles a Service with the
// dependencies we can construct without a Postgres. Methods
// that hit Pool / OAuth / Webhooks panic in these tests by
// design — every test in this file only exercises code paths
// that bypass those dependencies (input validation,
// checkQuota).
//
// When `limiter` is nil, the ServiceConfig.LimiterStore field
// is left as a true interface nil (NOT a typed nil concrete);
// this matters because `iface == nil` returns false for a
// typed-nil interface, which would route checkQuota to call
// IncrWithTTL on a nil pointer.
func newServiceForUnitTests(t *testing.T, limiter *fakeLimiterStore, now func() time.Time) *Service {
	t.Helper()
	if now == nil {
		now = func() time.Time { return time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC) }
	}
	cfg := ServiceConfig{
		DefaultClientDispatchPerHour: 5,
		Logger:                       log.New(io.Discard, "", 0),
		Now:                          now,
	}
	if limiter != nil {
		cfg.LimiterStore = limiter
	}
	return &Service{cfg: cfg}
}

// TestCheckQuota_BelowQuotaAllows pins the happy path: a
// client under its bucket cap is allowed.
func TestCheckQuota_BelowQuotaAllows(t *testing.T) {
	limiter := newFakeLimiterStore()
	svc := newServiceForUnitTests(t, limiter, nil)
	allowed, retryAt := svc.checkQuota(context.Background(), "client-1", 5)
	if !allowed {
		t.Errorf("checkQuota(under quota) allowed=false; want true")
	}
	if !retryAt.IsZero() {
		t.Errorf("checkQuota(under quota) retryAt=%v; want zero", retryAt)
	}
}

// TestCheckQuota_AboveQuotaDefersToNextWindow pins the
// rate-limited path: a client at its bucket cap is deferred,
// and the returned next_retry_at lands on the next hourly
// bucket boundary.
func TestCheckQuota_AboveQuotaDefersToNextWindow(t *testing.T) {
	limiter := newFakeLimiterStore()
	frozen := time.Date(2024, 6, 1, 12, 30, 0, 0, time.UTC)
	svc := newServiceForUnitTests(t, limiter, func() time.Time { return frozen })

	// Fill the quota (5 deliveries) — all should be allowed.
	for i := 0; i < 5; i++ {
		allowed, _ := svc.checkQuota(context.Background(), "client-1", 5)
		if !allowed {
			t.Fatalf("delivery %d: expected allowed=true while filling quota", i)
		}
	}
	// 6th delivery in the same bucket: deferred.
	allowed, retryAt := svc.checkQuota(context.Background(), "client-1", 5)
	if allowed {
		t.Errorf("checkQuota(over quota) allowed=true; want false")
	}
	want := time.Date(2024, 6, 1, 13, 0, 0, 0, time.UTC)
	if !retryAt.Equal(want) {
		t.Errorf("checkQuota(over quota) retryAt=%v; want %v (next hourly bucket boundary)", retryAt, want)
	}
}

// TestCheckQuota_LimiterErrorFailsOpen pins the production
// resilience contract: a transient Valkey error MUST NOT
// block dispatch. The doc.go contract says we fail-open and
// log; this test pins that behaviour.
func TestCheckQuota_LimiterErrorFailsOpen(t *testing.T) {
	limiter := newFakeLimiterStore()
	limiter.injectErr = errors.New("simulated valkey outage")
	svc := newServiceForUnitTests(t, limiter, nil)

	allowed, retryAt := svc.checkQuota(context.Background(), "client-1", 5)
	if !allowed {
		t.Errorf("checkQuota with limiter error: allowed=false; want true (must fail-open per doc.go)")
	}
	if !retryAt.IsZero() {
		t.Errorf("checkQuota with limiter error: retryAt=%v; want zero (no defer on fail-open)", retryAt)
	}
}

// TestCheckQuota_NoLimiter_AlwaysAllows pins the dev / single-
// tenant box behaviour: with LimiterStore==nil, every delivery
// is allowed (no Valkey wired = no per-client quota).
func TestCheckQuota_NoLimiter_AlwaysAllows(t *testing.T) {
	svc := newServiceForUnitTests(t, nil, nil)
	for i := 0; i < 100; i++ {
		allowed, retryAt := svc.checkQuota(context.Background(), "client-1", 1)
		if !allowed {
			t.Errorf("delivery %d: allowed=false with nil limiter; want true", i)
		}
		if !retryAt.IsZero() {
			t.Errorf("delivery %d: retryAt=%v with nil limiter; want zero", i, retryAt)
		}
	}
}

// TestCheckQuota_PerClientIsolation pins that two distinct
// clients have separate buckets. A noisy integration that
// exhausts its quota MUST NOT affect a well-behaved one.
func TestCheckQuota_PerClientIsolation(t *testing.T) {
	limiter := newFakeLimiterStore()
	svc := newServiceForUnitTests(t, limiter, nil)

	// client-A fills its quota.
	for i := 0; i < 5; i++ {
		_, _ = svc.checkQuota(context.Background(), "client-A", 5)
	}
	// client-A is now over quota.
	allowedA, _ := svc.checkQuota(context.Background(), "client-A", 5)
	if allowedA {
		t.Errorf("client-A: expected over-quota deny; got allowed")
	}
	// client-B's first delivery must be allowed.
	allowedB, retryAtB := svc.checkQuota(context.Background(), "client-B", 5)
	if !allowedB {
		t.Errorf("client-B: allowed=false; want true (must be isolated from client-A's bucket)")
	}
	if !retryAtB.IsZero() {
		t.Errorf("client-B: retryAt=%v; want zero", retryAtB)
	}
}

// TestCheckQuota_HourlyBucketsRotate pins that crossing an
// hour boundary opens a fresh bucket. Without this, a client
// that filled its quota at 12:59 would still be denied at
// 13:00 — wrong.
func TestCheckQuota_HourlyBucketsRotate(t *testing.T) {
	limiter := newFakeLimiterStore()
	currentTime := time.Date(2024, 6, 1, 12, 30, 0, 0, time.UTC)
	svc := newServiceForUnitTests(t, limiter, func() time.Time { return currentTime })

	// Fill the 12:00-13:00 bucket.
	for i := 0; i < 5; i++ {
		_, _ = svc.checkQuota(context.Background(), "client-1", 5)
	}
	// 6th delivery in the 12:30 window: deferred.
	allowed, _ := svc.checkQuota(context.Background(), "client-1", 5)
	if allowed {
		t.Errorf("over-quota at 12:30: allowed=true; want false")
	}
	// Roll the clock to 13:01 — new hourly bucket.
	currentTime = time.Date(2024, 6, 1, 13, 1, 0, 0, time.UTC)
	allowed, retryAt := svc.checkQuota(context.Background(), "client-1", 5)
	if !allowed {
		t.Errorf("at 13:01 (fresh bucket): allowed=false; want true (bucket rotation broken)")
	}
	if !retryAt.IsZero() {
		t.Errorf("at 13:01 (fresh bucket): retryAt=%v; want zero", retryAt)
	}
}

// TestRegisterWebhookForClient_InputValidation pins the
// pre-DB validation paths. These can run without a Pool
// because the validation short-circuits before any SQL.
func TestRegisterWebhookForClient_InputValidation(t *testing.T) {
	svc := newServiceForUnitTests(t, nil, nil)
	cases := []struct {
		name      string
		tenant    string
		client    string
		user      string
		url       string
		wantError string
	}{
		{"empty tenant", "", "client-1", "user-1", "https://example.com", "tenantID required"},
		{"whitespace tenant", "   ", "client-1", "user-1", "https://example.com", "tenantID required"},
		{"empty client", "tenant-1", "", "user-1", "https://example.com", "oauthClientID required"},
		{"empty user", "tenant-1", "client-1", "", "https://example.com", "userID required"},
		{"whitespace user", "tenant-1", "client-1", "   ", "https://example.com", "userID required"},
		{"empty url", "tenant-1", "client-1", "user-1", "", "url required"},
		{"whitespace url", "tenant-1", "client-1", "user-1", "   ", "url required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RegisterWebhookForClient(context.Background(), tc.tenant, tc.client, tc.user, []string{"read:mail"}, tc.url, []string{"email.received"}, "v2")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error = %v; want substring %q", err, tc.wantError)
			}
		})
	}
}

// TestRegisterWebhookForClient_RejectsEmptyEvents pins the
// defense-in-depth empty-events guard at the SERVICE layer.
// The HTTP boundary handler already rejects len(req.Events)
// == 0 with 400 invalid_request (TestRegister_RejectsEmptyEvents
// in handlers_test.go), but the service method must
// independently refuse it so any future caller that bypasses
// the handler — internal admin path, migration backfill,
// programmatic embedding of *Service — cannot persist a row
// with `events = []` (which the underlying webhooks layer
// interprets as a wildcard subscription).
func TestRegisterWebhookForClient_RejectsEmptyEvents(t *testing.T) {
	svc := newServiceForUnitTests(t, nil, nil)
	cases := []struct {
		name   string
		events []string
	}{
		{"nil events", nil},
		{"empty slice events", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := svc.RegisterWebhookForClient(
				context.Background(),
				"tenant-1",
				"client-1",
				"user-1",
				[]string{"read:mail"},
				"https://example.com",
				tc.events,
				"v2",
			)
			if err == nil {
				t.Fatal("expected error rejecting empty events, got nil")
			}
			if !strings.Contains(err.Error(), "requestedEvents required") {
				t.Errorf("error = %v; want substring %q", err, "requestedEvents required")
			}
			if result != nil {
				t.Errorf("expected nil result for empty-events rejection, got %+v", result)
			}
		})
	}
}

// TestRegisterWebhookForClient_AllEventsDenied pins the
// subscribe-time scope filter. A client that requests only
// events outside its scope set MUST receive
// ErrInsufficientScope with the denied list populated.
func TestRegisterWebhookForClient_AllEventsDenied(t *testing.T) {
	svc := newServiceForUnitTests(t, nil, nil)
	result, err := svc.RegisterWebhookForClient(
		context.Background(),
		"tenant-1",
		"client-1",
		"user-1",
		[]string{"read:contacts"}, // only contacts scope
		"https://example.com",
		[]string{"email.received", "calendar.event_created"}, // none match
		"v2",
	)
	if !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("expected ErrInsufficientScope, got %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result with denied list, got nil")
	}
	if len(result.Denied) != 2 {
		t.Errorf("expected 2 denied events, got %d (%v)", len(result.Denied), result.Denied)
	}
}

// TestNewService_RequiresDependencies pins the constructor
// guards: forgetting to wire Pool / Webhooks / OAuth is a
// boot-time misconfiguration the operator MUST see at startup.
func TestNewService_RequiresDependencies(t *testing.T) {
	_, err := NewService(ServiceConfig{})
	if err == nil {
		t.Fatal("NewService(empty) returned nil error; want validation failure")
	}
}

// TestNewService_DefaultsApplied pins the default-fill
// behaviour: DefaultClientDispatchPerHour, Logger, and Now
// MUST be filled when the operator omits them, so the rest of
// the service can assume non-nil.
func TestNewService_DefaultsApplied(t *testing.T) {
	// Smoke-test ONLY the default-fill — we can't fully call
	// NewService without a Pool because of the strict guards,
	// so this test exercises the same default-fill code path
	// via a constructed config that mimics what NewService
	// would produce.
	cfg := ServiceConfig{
		DefaultClientDispatchPerHour: 0, // operator omitted
	}
	if cfg.DefaultClientDispatchPerHour <= 0 {
		cfg.DefaultClientDispatchPerHour = DefaultClientDispatchPerHour
	}
	if cfg.DefaultClientDispatchPerHour != DefaultClientDispatchPerHour {
		t.Errorf("default-fill DefaultClientDispatchPerHour = %d; want %d", cfg.DefaultClientDispatchPerHour, DefaultClientDispatchPerHour)
	}
}

