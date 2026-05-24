package jmap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// CircuitBreaker abstracts the consecutive-5xx → trip-and-cool-down
// state machine the shard failover transport consults before each
// upstream attempt. Two implementations:
//
//   - inProcessCircuitBreaker — per-pod counter map, the
//     pre-Phase-5 default. Faithfully matches the original behavior
//     so existing tests continue to pass when no Valkey is wired.
//
//   - redisCircuitBreaker — Valkey-backed shared state so every BFF
//     pod sees the same view. A 5xx storm against shard X opens the
//     breaker once across the fleet instead of once per pod.
//
// All three methods MUST be safe for concurrent use. `Open` reports
// whether new traffic should be diverted away from `host`; the
// transport calls it on every retry decision so it MUST be fast —
// the Valkey implementation is a single Lua roundtrip per call.
type CircuitBreaker interface {
	// Open reports whether the breaker for `host` is currently
	// tripped. A "half-open probe" state — exactly one trial
	// request allowed after the cooldown window expires — is
	// communicated by returning false (allow) and then relying on
	// the caller to record success/failure to either re-close or
	// re-open the breaker.
	Open(ctx context.Context, host string) bool

	// RecordSuccess clears the failure state for `host`. A
	// successful response IS the half-open probe's positive
	// outcome.
	RecordSuccess(ctx context.Context, host string)

	// RecordFailure increments the failure count for `host` and
	// trips the breaker open if the threshold is reached. The
	// concrete cooldown / window timings are baked into the
	// implementation, not parameters on the call.
	RecordFailure(ctx context.Context, host string)
}

// inProcessCircuitBreaker keeps failure state in a process-local
// map. This is the default when the proxy is built without a
// Valkey wire — single-pod deployments and tests use it.
//
// The state machine is intentionally identical to the Redis-backed
// implementation so behavior doesn't drift when an operator turns
// `KMAIL_VALKEY_URL` on or off:
//
//   - Sliding `window`: failure timestamps older than `now - window`
//     are dropped on every call before counting against the trip
//     threshold. A burst that straddles the window doesn't
//     accumulate stale failures across long gaps.
//   - Trip-and-cooldown: when `len(failures) >= threshold`, the
//     breaker opens until `tripTime + cooldown`.
//   - Half-open probe: once the cooldown elapses, `Open` returns
//     false so exactly one caller probes the host. A successful
//     probe (`RecordSuccess`) clears the state; a failed probe
//     (`RecordFailure`) re-trips the cooldown.
//
// Zero `cooldown` / `window` retain the original count-only
// behavior (no time-based eviction, breaker stays open until the
// next `RecordSuccess`) — this preserves bug-for-bug compatibility
// for tests that never advance a clock.
type inProcessCircuitBreaker struct {
	threshold int
	cooldown  time.Duration
	window    time.Duration
	now       func() time.Time

	mu    sync.Mutex
	state map[string]*hostBreakerState
}

// hostBreakerState is the per-host counter. `failures` is a
// sliding window of failure timestamps (kept ordered, oldest
// first). `openUntil` is the wall-clock instant after which the
// breaker leaves the "open" plateau; the zero value means the
// breaker has never tripped.
type hostBreakerState struct {
	failures  []time.Time
	openUntil time.Time
}

// inProcessBreakerConfig parameterises the local breaker so the
// proxy can thread the same Threshold/Cooldown/Window values into
// it as the Redis-backed impl. Zero fields take safe defaults.
//
// Invariant: the effective sliding `Window` is clamped to >=
// `Cooldown`. Otherwise an operator misconfiguration like
// (Cooldown=90s, Window=60s) would let the failure history age out
// of the window during the cooldown plateau, so after the breaker
// half-opens a single failed probe wouldn't see enough recent
// failures to re-trip — the breaker would prematurely fully close
// despite the host still being unhealthy. The clamp + logger
// warning (see `clampBreakerWindow`) keeps the re-trip semantics
// intuitive regardless of how the env vars are set.
type inProcessBreakerConfig struct {
	Threshold int
	Cooldown  time.Duration
	Window    time.Duration
	Now       func() time.Time
	Logger    *log.Logger
}

func newInProcessCircuitBreaker(cfg inProcessBreakerConfig) *inProcessCircuitBreaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 3
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Window = clampBreakerWindow(cfg.Cooldown, cfg.Window, "jmap.inProcessCircuitBreaker", cfg.Logger)
	return &inProcessCircuitBreaker{
		threshold: cfg.Threshold,
		cooldown:  cfg.Cooldown,
		window:    cfg.Window,
		now:       cfg.Now,
		state:     map[string]*hostBreakerState{},
	}
}

// clampBreakerWindow enforces window >= cooldown so a recovered
// host that fails its half-open probe re-trips the breaker (i.e.,
// the prior failure history is still in the sliding window when
// the probe lands). Returns the clamped value; when clamping
// actually changes the value it logs a one-time WARN through
// `logger` (or `log.Default()` if nil) so the operator notices
// their `KMAIL_BREAKER_WINDOW` / `KMAIL_BREAKER_COOLDOWN`
// inversion. Zero or negative cooldown disables the clamp (the
// breaker falls back to the legacy count-only mode).
func clampBreakerWindow(cooldown, window time.Duration, source string, logger *log.Logger) time.Duration {
	if cooldown <= 0 || window <= 0 || window >= cooldown {
		return window
	}
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(
		"WARN: %s: Window (%s) < Cooldown (%s); clamping effective Window to Cooldown "+
			"so failed half-open probes can still re-trip the breaker. "+
			"Set Window >= Cooldown to silence this warning.",
		source, window, cooldown,
	)
	return cooldown
}

// trimLocked drops failure timestamps older than `now - window`
// from the front of the slice. With `window=0` the function is a
// no-op so the legacy count-only path (preserved by zero-config
// callers) still works.
func (b *inProcessCircuitBreaker) trimLocked(s *hostBreakerState, now time.Time) {
	if b.window <= 0 || len(s.failures) == 0 {
		return
	}
	cutoff := now.Add(-b.window)
	i := 0
	for i < len(s.failures) && s.failures[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		s.failures = s.failures[i:]
	}
}

func (b *inProcessCircuitBreaker) Open(_ context.Context, host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.state[host]
	if !ok {
		return false
	}
	now := b.now()
	b.trimLocked(s, now)
	if b.cooldown > 0 {
		// Open plateau: the breaker tripped recently and the
		// cooldown hasn't elapsed yet. Half-open: the cooldown
		// elapsed — return false so the next caller probes.
		return !s.openUntil.IsZero() && now.Before(s.openUntil)
	}
	// Cooldown disabled — match the original count-only
	// behavior used by tests that never advance a clock.
	return len(s.failures) >= b.threshold
}

func (b *inProcessCircuitBreaker) RecordSuccess(_ context.Context, host string) {
	b.mu.Lock()
	delete(b.state, host)
	b.mu.Unlock()
}

func (b *inProcessCircuitBreaker) RecordFailure(_ context.Context, host string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.state[host]
	if !ok {
		s = &hostBreakerState{}
		b.state[host] = s
	}
	now := b.now()
	b.trimLocked(s, now)
	s.failures = append(s.failures, now)
	// Half-open re-trip: if the breaker previously tripped
	// (`openUntil` is set) and the cooldown has elapsed (we're
	// in the half-open state), a single failure re-trips
	// immediately regardless of how many failures are in the
	// sliding window. This is the standard CB pattern and
	// guarantees the half-open probe correctly latches the
	// breaker back open even if the trim emptied the window.
	if b.cooldown > 0 && !s.openUntil.IsZero() && !now.Before(s.openUntil) {
		s.openUntil = now.Add(b.cooldown)
		return
	}
	if len(s.failures) >= b.threshold {
		if b.cooldown > 0 {
			s.openUntil = now.Add(b.cooldown)
		}
	}
}

// RedisCircuitBreaker is the multi-pod, Valkey-backed breaker.
// State is keyed by shard `host` (URL.Host) and lives entirely in
// Valkey, so every BFF pod sees the same trip/reset events.
//
// State model — three implicit states from two keys:
//
//	kmail:cb:{<host>}:fail        ZSET of failure timestamps within
//	                              `window` (sliding); ZCARD compared
//	                              against threshold to trip.
//	kmail:cb:{<host>}:open_until  String with epoch-ms wall-clock
//	                              after which the breaker leaves the
//	                              "open" state. Set on trip, deleted
//	                              on a successful probe.
//
// Closed:    no open_until, fail count < threshold within window.
// Open:      now < open_until.
// Half-open: open_until expired but not yet a successful probe.
//
// The hash-tag (`{<host>}`) co-locates both keys to the same Valkey
// Cluster slot so the Lua scripts can operate on them in one EVAL.
//
// Failures and probes are processed via Lua so the trip and reset
// transitions are atomic — two concurrent pods seeing the same
// threshold crossing race-free trip exactly once.
type RedisCircuitBreaker struct {
	client    *redis.Client
	logger    *log.Logger
	threshold int
	cooldown  time.Duration
	window    time.Duration
	now       func() time.Time

	allowOnce   sync.Once
	allowScript *redis.Script
	failOnce    sync.Once
	failScript  *redis.Script
}

// RedisCircuitBreakerConfig parameterises the shared breaker.
// Defaults are applied for any zero-valued field:
//
//	Threshold: 3 consecutive failures within Window trips open.
//	Cooldown:  30s "open" plateau before the half-open probe.
//	Window:    60s sliding window — failures older than this don't
//	           count toward the trip threshold.
//
// The defaults are tuned to match the existing in-process behavior
// in the typical case (a brief 5xx storm during a Stalwart restart
// trips the breaker, and shard health probes naturally restore it).
type RedisCircuitBreakerConfig struct {
	Client    *redis.Client
	Logger    *log.Logger
	Threshold int
	Cooldown  time.Duration
	Window    time.Duration
	// Now is the wall-clock source; defaults to time.Now. Tests
	// inject a fixed clock so trip / cooldown transitions are
	// deterministic.
	Now func() time.Time
}

// NewRedisCircuitBreaker wires a Valkey-backed shared breaker.
// Returns (nil, error) if Client is nil.
func NewRedisCircuitBreaker(cfg RedisCircuitBreakerConfig) (*RedisCircuitBreaker, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("jmap.NewRedisCircuitBreaker: Client is required")
	}
	b := &RedisCircuitBreaker{
		client:    cfg.Client,
		logger:    cfg.Logger,
		threshold: cfg.Threshold,
		cooldown:  cfg.Cooldown,
		window:    cfg.Window,
		now:       cfg.Now,
	}
	if b.logger == nil {
		b.logger = log.Default()
	}
	if b.threshold <= 0 {
		b.threshold = 3
	}
	if b.cooldown <= 0 {
		b.cooldown = 30 * time.Second
	}
	if b.window <= 0 {
		b.window = 60 * time.Second
	}
	if b.now == nil {
		b.now = time.Now
	}
	// Mirror the in-process breaker invariant so both impls agree
	// on re-trip semantics when an operator misconfigures the env
	// vars (cooldown > window). See clampBreakerWindow for the
	// detailed rationale.
	b.window = clampBreakerWindow(b.cooldown, b.window, "jmap.RedisCircuitBreaker", b.logger)
	// Eagerly compile so the first Open call is a fast Lua
	// invocation, not a script compile + EVAL.
	b.ensureAllowScript()
	b.ensureFailScript()
	return b, nil
}

// breakerAllowScript decides whether the breaker is currently open.
// The script consumes `open_until` and, if the cooldown has
// elapsed, does NOT delete it — only a recorded success closes the
// breaker. This lets multiple pods all observe "half-open" until
// the first one that gets through reports success, after which
// they all see "closed".
//
//	KEYS[1]   kmail:cb:{<host>}:fail        (unused here but
//	                                         hash-tag co-located)
//	KEYS[2]   kmail:cb:{<host>}:open_until
//	ARGV[1]   `now` epoch milliseconds
//
// Returns 1 when the breaker IS open (caller should skip the
// host), 0 when closed or half-open (caller should attempt).
const breakerAllowScript = `
local open_until_key = KEYS[2]
local now            = tonumber(ARGV[1])
local raw            = redis.call("GET", open_until_key)
if raw == false or raw == nil then
    return 0
end
local until_ms = tonumber(raw)
if until_ms == nil or now >= until_ms then
    return 0
end
return 1
`

// breakerFailScript records a failure timestamp in the sliding
// ZSET, prunes anything older than `now - window`, and trips the
// breaker open when the surviving count crosses `threshold`. The
// trip transition is idempotent: a concurrent caller hitting the
// threshold a second time simply rewrites `open_until` with the
// fresher cooldown horizon. Both keys get a PEXPIRE refresh so
// idle hosts age out instead of leaking ZSET entries forever.
//
//	KEYS[1]   kmail:cb:{<host>}:fail
//	KEYS[2]   kmail:cb:{<host>}:open_until
//	ARGV[1]   window milliseconds
//	ARGV[2]   threshold (integer)
//	ARGV[3]   cooldown milliseconds
//	ARGV[4]   `now` epoch milliseconds
//	ARGV[5]   unique ZSET member (`<now-ms>:<random hex>`)
//
// Returns the post-prune failure count.
const breakerFailScript = `
local fail_key       = KEYS[1]
local open_until_key = KEYS[2]
local window         = tonumber(ARGV[1])
local threshold      = tonumber(ARGV[2])
local cooldown       = tonumber(ARGV[3])
local now            = tonumber(ARGV[4])
local member         = ARGV[5]
local cutoff         = now - window

redis.call("ZREMRANGEBYSCORE", fail_key, "-inf", "(" .. tostring(cutoff))
redis.call("ZADD", fail_key, now, member)
local count = tonumber(redis.call("ZCARD", fail_key))

-- Keep the failure set alive a little longer than the window so
-- a host that goes from "almost tripped" to "completely idle"
-- doesn't lose its history before the window naturally rolls
-- forward.
redis.call("PEXPIRE", fail_key, window + 5000)

-- Half-open re-trip: when open_until exists AND the cooldown
-- has elapsed (so Open() reports half-open), a single new
-- failure must immediately re-trip the breaker regardless of
-- whether the sliding-window count crossed the threshold. This
-- mirrors the in-process breaker and is the standard CB
-- pattern — without it, an operator misconfig (window < cooldown)
-- could let a failed half-open probe leave the breaker closed.
local raw = redis.call("GET", open_until_key)
if raw ~= false and raw ~= nil then
    local open_until = tonumber(raw)
    if open_until ~= nil and now >= open_until then
        redis.call("SET", open_until_key, tostring(now + cooldown), "PX", cooldown + 5000)
        return count
    end
end

if count >= threshold then
    redis.call("SET", open_until_key, tostring(now + cooldown), "PX", cooldown + 5000)
end
return count
`

func (b *RedisCircuitBreaker) ensureAllowScript() {
	b.allowOnce.Do(func() {
		b.allowScript = redis.NewScript(breakerAllowScript)
	})
}

func (b *RedisCircuitBreaker) ensureFailScript() {
	b.failOnce.Do(func() {
		b.failScript = redis.NewScript(breakerFailScript)
	})
}

func (b *RedisCircuitBreaker) keys(host string) (string, string) {
	// Hash-tag on host so both keys land in the same Cluster slot.
	return fmt.Sprintf("kmail:cb:{%s}:fail", host),
		fmt.Sprintf("kmail:cb:{%s}:open_until", host)
}

// Open reports whether the breaker for `host` is currently tripped.
// Fail-open semantics: if the Valkey call errors, we LOG and return
// false so we don't take down the proxy when the breaker store is
// unavailable. The local in-process breaker is still attached to the
// proxy as a belt-and-braces guard when this mode is selected.
func (b *RedisCircuitBreaker) Open(ctx context.Context, host string) bool {
	b.ensureAllowScript()
	failKey, openKey := b.keys(host)
	nowMs := b.now().UnixMilli()
	res, err := b.allowScript.Run(ctx, b.client, []string{failKey, openKey}, nowMs).Result()
	if err != nil {
		b.logger.Printf("jmap: breaker Open host=%s: %v", host, err)
		return false
	}
	v, _ := res.(int64)
	return v == 1
}

// RecordSuccess clears both the failure ZSET and the open_until
// marker. Done in a small MULTI so a concurrent failure write can't
// interleave between the two DELs and leave half-state behind.
func (b *RedisCircuitBreaker) RecordSuccess(ctx context.Context, host string) {
	failKey, openKey := b.keys(host)
	pipe := b.client.TxPipeline()
	pipe.Del(ctx, failKey)
	pipe.Del(ctx, openKey)
	if _, err := pipe.Exec(ctx); err != nil {
		b.logger.Printf("jmap: breaker RecordSuccess host=%s: %v", host, err)
	}
}

// RecordFailure pushes a timestamped failure into the sliding-window
// ZSET and lets the Lua script decide whether to trip. Logs and
// swallows transient Valkey errors — losing a single failure
// timestamp on a Valkey blip is preferable to escalating it to a
// client-visible 5xx.
func (b *RedisCircuitBreaker) RecordFailure(ctx context.Context, host string) {
	b.ensureFailScript()
	failKey, openKey := b.keys(host)
	nowMs := b.now().UnixMilli()
	member, err := newBreakerMember(nowMs)
	if err != nil {
		b.logger.Printf("jmap: breaker RecordFailure host=%s: member: %v", host, err)
		return
	}
	_, err = b.failScript.Run(
		ctx,
		b.client,
		[]string{failKey, openKey},
		b.window.Milliseconds(),
		b.threshold,
		b.cooldown.Milliseconds(),
		nowMs,
		member,
	).Result()
	if err != nil {
		b.logger.Printf("jmap: breaker RecordFailure host=%s: %v", host, err)
	}
}

// newBreakerMember mirrors the rate-limiter's unique-member pattern:
// timestamp prefix for sortable dumps + 8 random hex bytes so two
// failures landing in the same millisecond don't collide on ZADD.
func newBreakerMember(nowMs int64) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strconv.FormatInt(nowMs, 10) + ":" + hex.EncodeToString(b[:]), nil
}
