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

// inProcessCircuitBreaker keeps consecutive-failure counters in a
// process-local map. This is the default when the proxy is built
// without a Valkey wire, and it's the behavior the existing tests
// pin. Tripping is a simple count-threshold check; the threshold
// is configured per-proxy and passed in via `threshold`.
type inProcessCircuitBreaker struct {
	threshold int

	mu       sync.Mutex
	failures map[string]int
}

func newInProcessCircuitBreaker(threshold int) *inProcessCircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	return &inProcessCircuitBreaker{
		threshold: threshold,
		failures:  map[string]int{},
	}
}

func (b *inProcessCircuitBreaker) Open(_ context.Context, host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures[host] >= b.threshold
}

func (b *inProcessCircuitBreaker) RecordSuccess(_ context.Context, host string) {
	b.mu.Lock()
	delete(b.failures, host)
	b.mu.Unlock()
}

func (b *inProcessCircuitBreaker) RecordFailure(_ context.Context, host string) {
	b.mu.Lock()
	b.failures[host]++
	b.mu.Unlock()
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
