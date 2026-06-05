package featureflags

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// fakeSource is an in-memory source for resolver tests. It counts
// loadAll calls so caching behaviour can be asserted.
type fakeSource struct {
	mu        sync.Mutex
	flags     []Flag
	overrides []Override
	plans     map[string]string
	loadCalls int
	planCalls int
	loadErr   error
	loadDelay time.Duration // simulated store latency for race tests
	planDelay time.Duration // simulated tenantPlan latency for race tests
}

func (f *fakeSource) loadAll(context.Context) ([]Flag, []Override, error) {
	f.mu.Lock()
	f.loadCalls++
	delay := f.loadDelay
	err := f.loadErr
	flags, overrides := f.flags, f.overrides
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, nil, err
	}
	return flags, overrides, nil
}

func (f *fakeSource) tenantPlan(_ context.Context, tenantID string) (string, error) {
	f.mu.Lock()
	f.planCalls++
	delay := f.planDelay
	plan := f.plans[tenantID]
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return plan, nil
}

func (f *fakeSource) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadCalls
}

func (f *fakeSource) planCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planCalls
}

func TestEvaluatePrecedence(t *testing.T) {
	flag := resolved{
		def: false,
		overrides: map[string]bool{
			scopeKey(ScopeGlobal, ""):         true,
			scopeKey(ScopePlan, "pro"):        false,
			scopeKey(ScopeTenant, "tenant-A"): true,
			scopeKey(ScopeUser, "user-veto"):  false,
		},
	}
	tests := []struct {
		name string
		subj Subject
		want bool
	}{
		{"user override beats everything", Subject{TenantID: "tenant-A", UserID: "user-veto", Plan: "pro"}, false},
		{"tenant override beats plan+global", Subject{TenantID: "tenant-A", UserID: "user-x", Plan: "pro"}, true},
		{"plan override beats global", Subject{TenantID: "tenant-Z", Plan: "pro"}, false},
		{"global override beats default", Subject{TenantID: "tenant-Z", Plan: "core"}, true},
		{"empty subject falls to global", Subject{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evaluate(flag, tt.subj); got != tt.want {
				t.Fatalf("evaluate(%+v) = %v, want %v", tt.subj, got, tt.want)
			}
		})
	}
}

func TestEvaluateFallsToDefaultWithNoOverrides(t *testing.T) {
	on := resolved{def: true, overrides: map[string]bool{}}
	off := resolved{def: false, overrides: map[string]bool{}}
	if !evaluate(on, Subject{TenantID: "t"}) {
		t.Fatal("default-on flag should be enabled")
	}
	if evaluate(off, Subject{TenantID: "t"}) {
		t.Fatal("default-off flag should be disabled")
	}
}

func TestUnregisteredFlagIsOff(t *testing.T) {
	src := &fakeSource{flags: []Flag{{Key: "known", DefaultEnabled: true}}}
	svc := newService(src, nil)
	if svc.EvaluateSubject(context.Background(), "unknown", Subject{}) {
		t.Fatal("unregistered flag must resolve to false")
	}
	if !svc.EvaluateSubject(context.Background(), "known", Subject{}) {
		t.Fatal("registered default-on flag must resolve to true")
	}
}

func TestIsEnabledResolvesPlanFromContext(t *testing.T) {
	src := &fakeSource{
		flags:     []Flag{{Key: "beta", DefaultEnabled: false}},
		overrides: []Override{{FlagKey: "beta", Scope: ScopePlan, ScopeID: "privacy", Enabled: true}},
		plans:     map[string]string{"tenant-1": "privacy", "tenant-2": "core"},
	}
	svc := newService(src, nil)

	ctx1 := middleware.WithKChatUserID(middleware.WithTenantID(context.Background(), "tenant-1"), "u1")
	if !svc.IsEnabled(ctx1, "beta") {
		t.Fatal("privacy-plan tenant should have beta enabled via plan override")
	}
	ctx2 := middleware.WithTenantID(context.Background(), "tenant-2")
	if svc.IsEnabled(ctx2, "beta") {
		t.Fatal("core-plan tenant should not have beta enabled")
	}
}

// TestGetSnapshotSingleFlight verifies the thundering-herd guard: when
// many goroutines hit a cold cache at once, only ONE reload runs
// against the store, not one per goroutine.
func TestGetSnapshotSingleFlight(t *testing.T) {
	src := &fakeSource{
		flags:     []Flag{{Key: "f", DefaultEnabled: true}},
		loadDelay: 30 * time.Millisecond, // hold the lock long enough to herd
	}
	svc := newService(src, nil)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if !svc.EvaluateSubject(context.Background(), "f", Subject{}) {
				t.Errorf("flag f should resolve true")
			}
		}()
	}
	wg.Wait()

	if got := src.calls(); got != 1 {
		t.Fatalf("loadAll called %d times under concurrent cold-cache access, want 1 (single-flight)", got)
	}
}

// TestPlanForSingleFlight verifies the per-tenant plan-cache guard:
// when many goroutines resolve the SAME tenant's plan against a cold
// cache at once, only ONE tenantPlan query runs (single-flight) rather
// than one per goroutine.
func TestPlanForSingleFlight(t *testing.T) {
	src := &fakeSource{
		plans:     map[string]string{"tenant-A": "privacy"},
		planDelay: 30 * time.Millisecond, // herd while the leader loads
	}
	svc := newService(src, nil)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if got := svc.planFor(context.Background(), "tenant-A"); got != "privacy" {
				t.Errorf("planFor = %q, want %q", got, "privacy")
			}
		}()
	}
	wg.Wait()

	if got := src.planCallCount(); got != 1 {
		t.Fatalf("tenantPlan called %d times under concurrent cold-cache access, want 1 (single-flight)", got)
	}
}

// TestPlanForCachesAcrossCalls verifies a warm plan cache serves
// subsequent lookups without re-querying the store.
func TestPlanForCachesAcrossCalls(t *testing.T) {
	src := &fakeSource{plans: map[string]string{"tenant-A": "pro"}}
	svc := newService(src, nil).WithTTL(time.Hour)

	for i := 0; i < 3; i++ {
		if got := svc.planFor(context.Background(), "tenant-A"); got != "pro" {
			t.Fatalf("planFor = %q, want %q", got, "pro")
		}
	}
	if got := src.planCallCount(); got != 1 {
		t.Fatalf("tenantPlan called %d times across warm-cache reads, want 1", got)
	}
}

func TestSnapshotCachingAndInvalidate(t *testing.T) {
	src := &fakeSource{flags: []Flag{{Key: "f", DefaultEnabled: true}}}
	svc := newService(src, nil).WithTTL(time.Hour)

	for i := 0; i < 5; i++ {
		svc.EvaluateSubject(context.Background(), "f", Subject{})
	}
	if got := src.calls(); got != 1 {
		t.Fatalf("expected 1 load within TTL, got %d", got)
	}

	svc.Invalidate()
	svc.EvaluateSubject(context.Background(), "f", Subject{})
	if got := src.calls(); got != 2 {
		t.Fatalf("expected reload after Invalidate, got %d loads", got)
	}
}

func TestFailSafeOnLoadError(t *testing.T) {
	src := &fakeSource{loadErr: context.DeadlineExceeded}
	svc := newService(src, nil)
	if svc.EvaluateSubject(context.Background(), "anything", Subject{}) {
		t.Fatal("a store load error must default the flag to OFF")
	}
}

func TestPackageLevelDefault(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })

	// No default installed → off.
	if IsEnabled(context.Background(), "x") {
		t.Fatal("package IsEnabled with no default must be false")
	}

	src := &fakeSource{flags: []Flag{{Key: "x", DefaultEnabled: true}}}
	SetDefault(newService(src, nil))
	if !IsEnabled(context.Background(), "x") {
		t.Fatal("package IsEnabled should delegate to the installed default")
	}
}

func TestListSorted(t *testing.T) {
	src := &fakeSource{
		flags: []Flag{{Key: "zeta"}, {Key: "alpha"}},
		overrides: []Override{
			{FlagKey: "alpha", Scope: ScopeUser, ScopeID: "u", Enabled: true},
			{FlagKey: "alpha", Scope: ScopeGlobal, ScopeID: "", Enabled: false},
		},
	}
	svc := newService(src, nil)
	views, err := svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Key != "alpha" || views[1].Key != "zeta" {
		t.Fatalf("flags not sorted by key: %+v", views)
	}
	// Overrides sorted least→most specific: global before user.
	if len(views[0].Overrides) != 2 || views[0].Overrides[0].Scope != ScopeGlobal || views[0].Overrides[1].Scope != ScopeUser {
		t.Fatalf("overrides not sorted by precedence: %+v", views[0].Overrides)
	}
}
