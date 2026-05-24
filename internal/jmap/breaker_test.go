package jmap

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newFakeRedis spins up a single-process miniredis server and
// returns a connected client + cleanup. miniredis implements the
// commands the breaker uses (GET / SET / DEL / ZADD / ZCARD /
// ZREMRANGEBYSCORE / PEXPIRE) and Lua via gopher-lua, which is
// enough to exercise the trip/cooldown/half-open state machine
// without a real Valkey.
func newFakeRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

func newSharedBreaker(t *testing.T, threshold int, window, cooldown time.Duration, now func() time.Time) (*RedisCircuitBreaker, *miniredis.Miniredis) {
	t.Helper()
	client, mr := newFakeRedis(t)
	b, err := NewRedisCircuitBreaker(RedisCircuitBreakerConfig{
		Client:    client,
		Threshold: threshold,
		Cooldown:  cooldown,
		Window:    window,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("NewRedisCircuitBreaker: %v", err)
	}
	return b, mr
}

// TestInProcessCircuitBreaker_TripsAtThreshold pins the default
// per-pod breaker against the same invariant the existing
// shard-failover test relied on: N failures opens the breaker, a
// success closes it. With zero cooldown/window this exercises the
// legacy count-only path so older tests keep working unchanged.
func TestInProcessCircuitBreaker_TripsAtThreshold(t *testing.T) {
	b := newInProcessCircuitBreaker(inProcessBreakerConfig{Threshold: 3})
	ctx := context.Background()
	host := "shard-a:8080"
	for i := 0; i < 2; i++ {
		b.RecordFailure(ctx, host)
		if b.Open(ctx, host) {
			t.Fatalf("breaker opened at %d failures, want >=3", i+1)
		}
	}
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("breaker did not open after 3 failures")
	}
	b.RecordSuccess(ctx, host)
	if b.Open(ctx, host) {
		t.Fatal("breaker still open after RecordSuccess")
	}
}

// TestInProcessCircuitBreaker_CooldownAllowsHalfOpenProbe mirrors
// the Redis breaker's cooldown test against the in-process impl
// so the two implementations have provably-equivalent state
// machines. Trips at threshold, stays open through the cooldown,
// half-opens (Open=false) after the cooldown, and closes fully on
// a successful probe.
func TestInProcessCircuitBreaker_CooldownAllowsHalfOpenProbe(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	b := newInProcessCircuitBreaker(inProcessBreakerConfig{
		Threshold: 2,
		Cooldown:  10 * time.Second,
		Window:    time.Minute,
		Now:       clock.Now,
	})
	ctx := context.Background()
	host := "shard-a:8080"

	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker open after 2 failures")
	}

	// Still inside the cooldown window.
	clock.Advance(5 * time.Second)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker still open mid-cooldown")
	}

	// Cooldown elapsed — half-open: Open() returns false so the
	// next call probes the host.
	clock.Advance(6 * time.Second)
	if b.Open(ctx, host) {
		t.Fatal("expected breaker half-open (Open=false) after cooldown")
	}

	// A successful probe closes the breaker fully.
	b.RecordSuccess(ctx, host)
	if b.Open(ctx, host) {
		t.Fatal("expected breaker closed after successful probe")
	}
}

// TestInProcessCircuitBreaker_SlidingWindowDoesNotTripOnStaleFailures
// pins the sliding-window semantics: failures older than `window`
// must not count toward the trip threshold, so a long-running pod
// doesn't accumulate stale 5xx into a false trip.
func TestInProcessCircuitBreaker_SlidingWindowDoesNotTripOnStaleFailures(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	b := newInProcessCircuitBreaker(inProcessBreakerConfig{
		Threshold: 3,
		Cooldown:  30 * time.Second,
		Window:    30 * time.Second,
		Now:       clock.Now,
	})
	ctx := context.Background()
	host := "shard-a:8080"

	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)

	clock.Advance(31 * time.Second)

	b.RecordFailure(ctx, host)
	if b.Open(ctx, host) {
		t.Fatal("breaker opened on stale-failure accumulation; window did not slide")
	}
}

// TestInProcessCircuitBreaker_HalfOpenProbeFailureReTrips verifies
// the half-open re-trip path: after the cooldown a failed probe
// must re-open the breaker for another cooldown cycle.
func TestInProcessCircuitBreaker_HalfOpenProbeFailureReTrips(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	b := newInProcessCircuitBreaker(inProcessBreakerConfig{
		Threshold: 2,
		Cooldown:  10 * time.Second,
		Window:    time.Minute,
		Now:       clock.Now,
	})
	ctx := context.Background()
	host := "shard-a:8080"

	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)
	clock.Advance(11 * time.Second)
	if b.Open(ctx, host) {
		t.Fatal("expected half-open after cooldown")
	}

	// Failed probe — the breaker must re-trip into the open
	// plateau for another cooldown cycle.
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected re-trip after failed half-open probe")
	}
}

// TestRedisCircuitBreaker_TripsAtThreshold mirrors the in-process
// test against the shared backend: the threshold count opens the
// breaker, and a recorded success closes it.
func TestRedisCircuitBreaker_TripsAtThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b, _ := newSharedBreaker(t, 3, time.Minute, 30*time.Second, func() time.Time { return now })
	ctx := context.Background()
	host := "shard-a:8080"
	for i := 0; i < 2; i++ {
		b.RecordFailure(ctx, host)
		if b.Open(ctx, host) {
			t.Fatalf("breaker opened at %d failures, want >=3", i+1)
		}
	}
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("breaker did not open after 3 failures")
	}
	b.RecordSuccess(ctx, host)
	if b.Open(ctx, host) {
		t.Fatal("breaker still open after RecordSuccess")
	}
}

// TestRedisCircuitBreaker_CooldownAllowsHalfOpenProbe verifies the
// state machine: after the cooldown elapses, `Open` returns false
// (half-open — the next caller probes the host). If the probe
// fails, the breaker re-opens at the next threshold; if it
// succeeds, the breaker stays closed.
func TestRedisCircuitBreaker_CooldownAllowsHalfOpenProbe(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{now: now}
	b, _ := newSharedBreaker(t, 2, time.Minute, 10*time.Second, clock.Now)
	ctx := context.Background()
	host := "shard-a:8080"

	// Trip the breaker.
	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker open after 2 failures")
	}

	// Still inside cooldown.
	clock.Advance(5 * time.Second)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker still open mid-cooldown")
	}

	// Cooldown elapsed — half-open: Open() returns false so the
	// next call probes the host.
	clock.Advance(6 * time.Second)
	if b.Open(ctx, host) {
		t.Fatal("expected breaker half-open (Open=false) after cooldown")
	}

	// A successful probe closes the breaker fully.
	b.RecordSuccess(ctx, host)
	if b.Open(ctx, host) {
		t.Fatal("expected breaker closed after successful probe")
	}
}

// TestRedisCircuitBreaker_SlidingWindowDoesNotTripOnStaleFailures
// pins the failure window: old failures roll off the ZSET, so a
// burst that crossed the window boundary shouldn't accumulate
// toward a trip.
func TestRedisCircuitBreaker_SlidingWindowDoesNotTripOnStaleFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{now: now}
	b, _ := newSharedBreaker(t, 3, 30*time.Second, 30*time.Second, clock.Now)
	ctx := context.Background()
	host := "shard-a:8080"

	// Two failures within the window.
	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)

	// Advance past the window — the two earlier failures are now
	// stale and must NOT count toward the trip.
	clock.Advance(31 * time.Second)

	// One fresh failure puts the count at 1 (sliding-window
	// trimmed the older entries) — should NOT trip the breaker.
	b.RecordFailure(ctx, host)
	if b.Open(ctx, host) {
		t.Fatal("breaker opened on stale-failure accumulation; window did not slide")
	}
}

// TestRedisCircuitBreaker_SharedState verifies the headline
// invariant of the migration from in-process to Valkey: two
// instances backed by the SAME server observe the same trip/reset
// events. A 5xx storm against shard X reported by one pod opens
// the breaker for shard X from every other pod's perspective.
func TestRedisCircuitBreaker_SharedState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	client, _ := newFakeRedis(t)
	cfg := RedisCircuitBreakerConfig{
		Client: client, Threshold: 2,
		Cooldown: 30 * time.Second, Window: time.Minute,
		Now: func() time.Time { return now },
	}
	pod1, err := NewRedisCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("pod1: %v", err)
	}
	pod2, err := NewRedisCircuitBreaker(cfg)
	if err != nil {
		t.Fatalf("pod2: %v", err)
	}
	ctx := context.Background()
	host := "shard-a:8080"

	pod1.RecordFailure(ctx, host)
	pod2.RecordFailure(ctx, host)

	if !pod2.Open(ctx, host) {
		t.Error("pod2 did not see pod1's failure: shared state broken")
	}
	if !pod1.Open(ctx, host) {
		t.Error("pod1 did not see pod2's failure: shared state broken")
	}

	// A recorded success on either pod must close the breaker
	// from every pod's view.
	pod1.RecordSuccess(ctx, host)
	if pod2.Open(ctx, host) {
		t.Error("pod2 still sees the breaker open after pod1's RecordSuccess")
	}
}

// TestRedisCircuitBreaker_PerHostIsolation verifies a trip on
// shard A doesn't affect shard B.
func TestRedisCircuitBreaker_PerHostIsolation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b, _ := newSharedBreaker(t, 2, time.Minute, 30*time.Second, func() time.Time { return now })
	ctx := context.Background()
	b.RecordFailure(ctx, "shard-a:8080")
	b.RecordFailure(ctx, "shard-a:8080")
	if !b.Open(ctx, "shard-a:8080") {
		t.Fatal("shard-a breaker did not trip")
	}
	if b.Open(ctx, "shard-b:8080") {
		t.Fatal("shard-b breaker also tripped: per-host isolation broken")
	}
}

// TestRedisCircuitBreaker_ConcurrentTripsExactlyOnce sanity-checks
// the atomicity claim: a fleet of concurrent failures races, but
// the breaker still trips deterministically (no lost increments,
// no double-reset). The ZCARD-then-SET pattern under the Lua
// guarantees this.
func TestRedisCircuitBreaker_ConcurrentTripsExactlyOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b, _ := newSharedBreaker(t, 5, time.Minute, 30*time.Second, func() time.Time { return now })
	ctx := context.Background()
	host := "shard-a:8080"
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.RecordFailure(ctx, host)
		}()
	}
	wg.Wait()
	if !b.Open(ctx, host) {
		t.Fatal("breaker did not trip under concurrent failures")
	}
}

// TestRedisCircuitBreaker_NilClient guards the constructor.
func TestRedisCircuitBreaker_NilClient(t *testing.T) {
	if _, err := NewRedisCircuitBreaker(RedisCircuitBreakerConfig{}); err == nil {
		t.Fatal("expected error for nil Client")
	}
}

// TestClampBreakerWindow_ExtendsWindowWhenShorterThanCooldown pins
// the contract from clampBreakerWindow: an operator who sets
// Window < Cooldown should still see correct re-trip semantics
// because the effective Window is silently extended to Cooldown
// AND a WARN line is logged so they can fix the misconfiguration.
func TestClampBreakerWindow_ExtendsWindowWhenShorterThanCooldown(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	got := clampBreakerWindow(90*time.Second, 60*time.Second, "test", logger)
	if got != 90*time.Second {
		t.Fatalf("clamp window=60s cooldown=90s -> %s, want 90s", got)
	}
	if !strings.Contains(buf.String(), "WARN: test:") {
		t.Fatalf("expected WARN log line, got %q", buf.String())
	}
}

// TestClampBreakerWindow_DoesNotClampWhenWindowSufficient pins the
// no-op path: with a correctly-configured (window >= cooldown)
// breaker, clampBreakerWindow must return the original window AND
// emit no log lines.
func TestClampBreakerWindow_DoesNotClampWhenWindowSufficient(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	got := clampBreakerWindow(30*time.Second, 60*time.Second, "test", logger)
	if got != 60*time.Second {
		t.Fatalf("clamp window=60s cooldown=30s -> %s, want 60s", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log output, got %q", buf.String())
	}
}

// TestClampBreakerWindow_DisabledWhenCooldownZero pins the legacy
// count-only path: when Cooldown==0 the breaker has no cooldown
// plateau, so window clamping doesn't apply. clampBreakerWindow
// returns the window untouched and emits no warning.
func TestClampBreakerWindow_DisabledWhenCooldownZero(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	got := clampBreakerWindow(0, 0, "test", logger)
	if got != 0 {
		t.Fatalf("clamp window=0 cooldown=0 -> %s, want 0", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log output, got %q", buf.String())
	}
}

// TestRedisCircuitBreaker_HalfOpenReTripsAfterTrimmedWindow pins
// the Lua-side half-open re-trip semantic: when prior failures
// have already trimmed out of the sliding window but `open_until`
// is still set, a single failure during the half-open state must
// re-trip the breaker. Without this guarantee a tight window
// (e.g., 5s) plus a longer cooldown (10s) would leave the breaker
// permanently closed after a failed probe — the bug the in-process
// counterpart was catching.
func TestRedisCircuitBreaker_HalfOpenReTripsAfterTrimmedWindow(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	b, _ := newSharedBreaker(t, 2, 5*time.Second, 10*time.Second, clock.Now)
	ctx := context.Background()
	host := "shard-a:8080"

	// Trip.
	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker open after 2 failures")
	}

	// Advance past BOTH the cooldown AND the window so the prior
	// failures get trimmed out of the ZSET when the next failure
	// is recorded. (cutoff = 12s - 5s = 7s; the original
	// failures at t=0 are < 7s.)
	clock.Advance(12 * time.Second)
	if b.Open(ctx, host) {
		t.Fatal("expected half-open (Open=false) after cooldown")
	}

	// A single failed probe must re-trip even though the window
	// trim left count=1 < threshold=2.
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker re-tripped after failed half-open probe")
	}
}

// TestInProcessCircuitBreaker_ClampsWindowAndReTrips proves the
// end-to-end behavior: with a misconfigured (cooldown=10s,
// window=5s) breaker, the in-process impl now correctly re-trips
// on a failed half-open probe — via the half-open re-trip path,
// not the count threshold, since the window trim removed the
// pre-trip failures by the time the probe lands.
func TestInProcessCircuitBreaker_ClampsWindowAndReTrips(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	b := newInProcessCircuitBreaker(inProcessBreakerConfig{
		Threshold: 2,
		Cooldown:  10 * time.Second,
		Window:    5 * time.Second,
		Now:       clock.Now,
		Logger:    logger,
	})
	ctx := context.Background()
	host := "shard-a:8080"

	if !strings.Contains(buf.String(), "WARN:") {
		t.Fatalf("expected clamp warning, got %q", buf.String())
	}

	b.RecordFailure(ctx, host)
	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected breaker open after 2 failures")
	}

	clock.Advance(11 * time.Second)
	if b.Open(ctx, host) {
		t.Fatal("expected half-open after cooldown")
	}

	b.RecordFailure(ctx, host)
	if !b.Open(ctx, host) {
		t.Fatal("expected re-trip after failed half-open probe; clamp likely failed")
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
