package middleware

import (
	"sync"
	"time"
)

// MemoryLimiter is an in-process token-bucket limiter used as the
// degraded fallback when the Valkey-backed sliding-window store is
// unreachable.
//
// It is intentionally per-replica (NOT shared across BFF
// instances): under a Valkey outage we cannot coordinate a global
// counter, so each replica enforces its own conservative ceiling.
// That bounds the blast radius of an outage without introducing a
// second coordination dependency that could itself fail. With N
// replicas the effective global ceiling is up to N× the per-replica
// limit, which is acceptable for a short-lived degraded window — the
// alternative (a hard 503 on every request the moment Valkey
// blinks) is strictly worse for availability.
//
// The bucket refills continuously at `limit/window` tokens per
// second and holds at most `limit` tokens, so a freshly-seen key may
// burst up to `limit` requests and then settles to the steady-state
// rate. This mirrors the authoritative sliding-window semantics
// closely enough to be a safe stand-in.
type MemoryLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket

	// ratePerSec is the steady-state refill rate (tokens/second),
	// derived as limit/window at construction.
	ratePerSec float64
	// burst is the bucket capacity (== limit), the most tokens a
	// key can accumulate while idle.
	burst float64

	// gcEvery bounds map growth: on the first Allow after this
	// interval elapses we drop fully-refilled (idle) buckets,
	// which carry no rate-limiting state and are safe to forget.
	gcEvery time.Duration
	lastGC  time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewMemoryLimiter builds a token-bucket limiter sized from an RPM
// ceiling and the sliding-window duration. A non-positive limit or
// window yields a limiter that admits everything (the caller treats
// "no fallback configured" as fail-open rather than a deadlock).
func NewMemoryLimiter(limit int, window time.Duration) *MemoryLimiter {
	if window <= 0 {
		window = time.Minute
	}
	rate := 0.0
	burst := 0.0
	if limit > 0 {
		burst = float64(limit)
		rate = float64(limit) / window.Seconds()
	}
	return &MemoryLimiter{
		buckets:    make(map[string]*tokenBucket),
		ratePerSec: rate,
		burst:      burst,
		gcEvery:    10 * window,
	}
}

// Allow consumes one token for `key` and reports whether the request
// is admitted. `now` is injected for deterministic testing.
//
// A limiter with a non-positive limit (`burst == 0`) admits
// unconditionally — it is the explicit "unlimited" sentinel, not a
// silently-broken bucket.
func (m *MemoryLimiter) Allow(key string, now time.Time) bool {
	if m == nil || m.burst <= 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.maybeGCLocked(now)

	b := m.buckets[key]
	if b == nil {
		// A never-seen key starts full so the fallback doesn't
		// reject legitimate first-time traffic during the outage.
		b = &tokenBucket{tokens: m.burst, last: now}
		m.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * m.ratePerSec
			if b.tokens > m.burst {
				b.tokens = m.burst
			}
			b.last = now
		}
	}

	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// maybeGCLocked drops idle (fully-refilled) buckets once per
// gcEvery. A bucket at capacity holds no debt, so forgetting it is
// equivalent to never having seen the key. Must be called with the
// mutex held.
func (m *MemoryLimiter) maybeGCLocked(now time.Time) {
	if m.lastGC.IsZero() {
		m.lastGC = now
		return
	}
	if now.Sub(m.lastGC) < m.gcEvery {
		return
	}
	m.lastGC = now
	for k, b := range m.buckets {
		refilled := b.tokens + now.Sub(b.last).Seconds()*m.ratePerSec
		if refilled >= m.burst {
			delete(m.buckets, k)
		}
	}
}
