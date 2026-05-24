package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/valkeyurl"
)

// RateLimiterConfig wires the Valkey-backed sliding-window rate
// limiter. Both tenant and user ceilings are applied; a request is
// rejected if either ceiling is exceeded.
//
// The sliding window is implemented as a Valkey sorted set per
// `(scope, identity)` keyed by request timestamp; on every call we
// trim entries older than `now - Window`, count the survivors, and
// admit or reject the new request atomically via a Lua script. This
// replaces the previous fixed-window counter, which would let
// callers see 2x the configured RPM at bucket boundaries (`burst at
// 59s, burst again at 61s`).
//
// The tenant and user checks are issued in a single Lua call so
// that a request rejected at the user ceiling does NOT consume any
// tenant budget — the script atomically rolls back the tenant
// admission when the user check fails. The previous two-call
// approach inflated the tenant counter under sustained user-level
// rate limiting.
type RateLimiterConfig struct {
	// Client is the Valkey (Redis-compatible) client used for
	// counter storage. Required; leave nil to short-circuit the
	// limiter (Wrap returns the next handler unchanged).
	Client RateLimiterStore

	// TenantRPM is the per-tenant request ceiling within the
	// sliding window.
	TenantRPM int
	// UserRPM is the per-user (tenant+user) request ceiling within
	// the sliding window.
	UserRPM int
	// Window is the sliding window duration. Defaults to 60s.
	Window time.Duration

	// Now overrides time.Now for tests.
	Now func() time.Time

	// Logger is used for transient-error diagnostics. When a
	// Valkey call fails we fail-open (allow the request) and log
	// the error so the limiter never takes the BFF offline.
	Logger *log.Logger
}

// RateLimiterStore is the narrow surface RateLimiter depends on.
// Implemented by `*RedisStore`, tests substitute a fake.
//
// `Allow` is the single combined tenant+user sliding-window
// primitive. Both keys are checked atomically: if the tenant check
// passes but the user check fails, the tenant admission is rolled
// back so the rejected request does NOT consume tenant budget.
//
// For tenant-only scoring (e.g. anonymous/unattributed traffic that
// still has a tenant context), pass `userKey == tenantKey` and
// `userLimit == 0`. The tenant-key placeholder is what lets the
// call pass Redis Cluster's pre-execution slot-co-location check;
// implementations MUST ignore the placeholder when `userLimit <=
// 0` and not touch it. Implementations MUST run all three steps —
// trim, check, admit-or-reject — for each active scope in a single
// atomic context (Lua / MULTI-EXEC), and MUST roll back the tenant
// admission when the user check rejects.
//
// Returns `(tenantOK, userOK, err)`, where each bool reports
// whether THAT scope's check passed (not the post-rollback state).
// Callers should report "tenant" rejection only when `!tenantOK`,
// and "user" rejection when `tenantOK && !userOK`. When no user
// scope is active (signalled by `userLimit <= 0`), `userOK` is
// always true.
type RateLimiterStore interface {
	Allow(
		ctx context.Context,
		tenantKey, userKey string,
		window time.Duration,
		tenantLimit, userLimit int,
		now time.Time,
	) (tenantAdmitted, userAdmitted bool, err error)
}

// RateLimiter is the HTTP middleware. Construct once at boot and
// share across every handler group that should respect the limit.
type RateLimiter struct {
	cfg RateLimiterConfig
}

// NewRateLimiter builds a RateLimiter with sensible defaults.
// Returns (nil, nil) when cfg.Client is nil — callers can then
// skip wiring the middleware without a branch on their side.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.TenantRPM <= 0 {
		cfg.TenantRPM = 1000
	}
	if cfg.UserRPM <= 0 {
		cfg.UserRPM = 200
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &RateLimiter{cfg: cfg}
}

// Wrap returns middleware that consults Valkey before delegating to
// `next`. When the limiter is disabled (Client is nil) the returned
// handler is `next` unchanged.
func (r *RateLimiter) Wrap(next http.Handler) http.Handler {
	if r == nil || r.cfg.Client == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tenantID := TenantIDFrom(req.Context())
		userID := KChatUserIDFrom(req.Context())
		if tenantID == "" {
			// Unauthenticated or unattributed request. The auth
			// middleware is the gate keeper for identity — the
			// limiter only runs after it, so missing context
			// means a wiring bug. Fail-open.
			next.ServeHTTP(w, req)
			return
		}

		now := r.cfg.Now()
		window := r.cfg.Window

		// Hash-tag the key shape so the tenant and user keys land
		// in the same Valkey-cluster slot. Cluster Lua requires
		// every KEYS[i] to be co-located; `{<tid>}` is the standard
		// Redis hash-tag convention.
		tenantKey := fmt.Sprintf("kmail:rl:tenant:{%s}", tenantID)
		// When there is no user scope, pass `tenantKey` as the
		// KEYS[2] placeholder (NOT an empty string) so Redis
		// Cluster's pre-execution slot-co-location check still
		// passes. The Lua script ignores KEYS[2] entirely when
		// `userLimit <= 0`, so the placeholder is never written
		// to.
		userKey := tenantKey
		userLimit := 0
		if userID != "" {
			userKey = fmt.Sprintf("kmail:rl:user:{%s}:%s", tenantID, userID)
			userLimit = r.cfg.UserRPM
		}

		tenantOK, userOK, err := r.cfg.Client.Allow(
			req.Context(),
			tenantKey, userKey,
			window,
			r.cfg.TenantRPM, userLimit,
			now,
		)
		if err != nil {
			r.cfg.Logger.Printf("ratelimit: allow tenant=%s user=%s: %v", tenantKey, userKey, err)
			next.ServeHTTP(w, req)
			return
		}
		if !tenantOK {
			writeRateLimitExceeded(w, window, r.cfg.TenantRPM, "tenant")
			return
		}
		if !userOK {
			writeRateLimitExceeded(w, window, r.cfg.UserRPM, "user")
			return
		}
		next.ServeHTTP(w, req)
	})
}

func writeRateLimitExceeded(w http.ResponseWriter, window time.Duration, rpm int, scope string) {
	// Retry-After is the most pessimistic estimate: a full
	// window. A sliding-window log doesn't have a single "reset"
	// instant the way a fixed window does (older entries roll off
	// continuously), so we surface the worst-case wait and rely
	// on clients backing off exponentially from there.
	retry := int(window.Seconds())
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rpm))
	w.Header().Set("X-RateLimit-Scope", scope)
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// slidingWindowScript is the atomic ZSET-based sliding-window log
// covering BOTH the tenant and (optionally) the user scope in a
// single call.
//
//	KEYS[1]   tenant ZSET key (kmail:rl:tenant:{<tid>})
//	KEYS[2]   user ZSET key (kmail:rl:user:{<tid>}:<uid>), or a
//	          copy of KEYS[1] when there is no user scope — the
//	          script ignores it in that case. KEYS[2] must hash
//	          to the same Valkey-cluster slot as KEYS[1] (which
//	          is why the caller passes KEYS[1] itself as the
//	          placeholder, not an empty string or a fixed
//	          sentinel).
//	ARGV[1]   window duration in milliseconds (integer)
//	ARGV[2]   tenant limit
//	ARGV[3]   user limit (<=0 signals "no user scope, skip
//	          KEYS[2]")
//	ARGV[4]   `now` epoch milliseconds
//	ARGV[5]   unique member to insert (`now-ms:<random hex>`)
//
// The script processes the tenant scope first; if the tenant
// admission succeeds AND a user scope is active, the user scope is
// processed next. When the user check rejects, the script ZREMs
// the just-inserted tenant member so a request rejected at the
// user ceiling does not consume tenant budget.
//
// PEXPIRE is refreshed on every call so idle scopes age out
// naturally and we never have to GC the sorted sets out-of-band.
// A 5s grace on top of the window handles small clock skew on the
// Valkey side.
//
// Returns `{tenant_admitted, user_admitted}` (both integers 0/1).
// When no user scope is active, `user_admitted` is 1 (no-op).
const slidingWindowScript = `
local tenant_key   = KEYS[1]
local user_key     = KEYS[2]
local window       = tonumber(ARGV[1])
local tenant_limit = tonumber(ARGV[2])
local user_limit   = tonumber(ARGV[3])
local now          = tonumber(ARGV[4])
local member       = ARGV[5]
local cutoff       = now - window

-- Tenant scope: trim + count + conditional add.
redis.call("ZREMRANGEBYSCORE", tenant_key, "-inf", "(" .. tostring(cutoff))
local tenant_count = tonumber(redis.call("ZCARD", tenant_key))
local tenant_admitted = 0
if tenant_count < tenant_limit then
    redis.call("ZADD", tenant_key, now, member)
    tenant_admitted = 1
end
redis.call("PEXPIRE", tenant_key, window + 5000)

-- Short-circuit: tenant rejected, nothing to do for user.
if tenant_admitted == 0 then
    return {0, 0}
end

-- No user scope active — the caller passed tenant_key as KEYS[2]
-- placeholder (for cluster slot co-location) and signalled "skip"
-- via user_limit<=0. Don't touch KEYS[2] in this branch.
if user_limit <= 0 then
    return {1, 1}
end

-- User scope: same trim + count + conditional add.
redis.call("ZREMRANGEBYSCORE", user_key, "-inf", "(" .. tostring(cutoff))
local user_count = tonumber(redis.call("ZCARD", user_key))
local user_admitted = 0
if user_count < user_limit then
    redis.call("ZADD", user_key, now, member)
    user_admitted = 1
end
redis.call("PEXPIRE", user_key, window + 5000)

-- Roll back the tenant admission if the user check rejected, so
-- rejected requests don't inflate the tenant counter. The first
-- return value is "did the tenant CHECK pass" (so the middleware
-- can attribute the rejection to the user scope) even though the
-- tenant ZSET was just restored to its pre-call state.
if user_admitted == 0 then
    redis.call("ZREM", tenant_key, member)
    return {1, 0}
end

return {1, 1}
`

// RedisStore wraps a *redis.Client so it satisfies the
// RateLimiterStore interface. The sliding-window log is implemented
// as an `EVAL`'d Lua script — the script handle is loaded once
// (`redis.NewScript` caches the SHA) and re-used on every call.
type RedisStore struct {
	Client *redis.Client

	// scriptOnce guards lazy compilation when the struct is built
	// literally (`&RedisStore{Client: c}`) rather than via the
	// `NewRedisStoreFromClient` constructor. Eager-compiling
	// callers see `scriptOnce.Do` short-circuit immediately.
	scriptOnce sync.Once
	script     *redis.Script
}

// NewRedisStore is a convenience constructor that dials Valkey at
// `url` and returns a RedisStore wrapping the client. Callers that
// already own a *redis.Client should use `NewRedisStoreFromClient`
// directly. URL form (`redis://` / `rediss://`) and bare `host:port`
// are both accepted via `valkeyurl.Parse`.
func NewRedisStore(url string) (*RedisStore, error) {
	opts, err := valkeyurl.Parse(url)
	if err != nil {
		return nil, err
	}
	return NewRedisStoreFromClient(redis.NewClient(opts)), nil
}

// NewRedisStoreFromClient wraps an existing *redis.Client. The
// sliding-window Lua script is compiled here and the resulting
// `*redis.Script` is shared across calls so each Allow only pays
// for an EVALSHA round-trip.
func NewRedisStoreFromClient(c *redis.Client) *RedisStore {
	s := &RedisStore{Client: c}
	s.ensureScript()
	return s
}

// ensureScript compiles the sliding-window Lua script exactly once,
// safely across concurrent Allow callers when the store was built
// via the struct literal rather than the constructor. `sync.Once`
// gives us happens-before for the `script` write so subsequent
// goroutines observe the compiled handle without a data race.
func (s *RedisStore) ensureScript() {
	s.scriptOnce.Do(func() {
		s.script = redis.NewScript(slidingWindowScript)
	})
}

// Allow runs the combined tenant+user sliding-window Lua script
// against Valkey. The ZSET member inserted on admission is
// `<now-ms>:<8-byte-hex>` so two requests landing in the same
// millisecond still both get counted (ZADD member uniqueness is
// what dedupes, not the score). Returns `(tenantOK, userOK, err)`
// per the RateLimiterStore contract: each flag reports whether
// that scope's CHECK passed, not the post-rollback ZSET state.
//
// URL parsing for the underlying *redis.Client lives in the
// shared `internal/valkeyurl` package (Phase A) so the rate
// limiter, the shared circuit breaker, and the misc valkey
// consumers all accept the same `redis://` / `rediss://` /
// bare `host:port` forms with a single regression suite.
func (s *RedisStore) Allow(
	ctx context.Context,
	tenantKey, userKey string,
	window time.Duration,
	tenantLimit, userLimit int,
	now time.Time,
) (bool, bool, error) {
	if s.Client == nil {
		return false, false, errors.New("RedisStore: Client is nil")
	}
	s.ensureScript()

	windowMs := window.Milliseconds()
	if windowMs <= 0 {
		windowMs = int64(time.Minute / time.Millisecond)
	}
	nowMs := now.UnixMilli()
	member, err := newUniqueMember(nowMs)
	if err != nil {
		return false, false, fmt.Errorf("ratelimit: generate member: %w", err)
	}
	keys := []string{tenantKey, userKey}
	res, err := s.script.Run(ctx, s.Client, keys, windowMs, tenantLimit, userLimit, nowMs, member).Result()
	if err != nil {
		return false, false, fmt.Errorf("ratelimit: EVAL: %w", err)
	}
	pair, ok := res.([]interface{})
	if !ok || len(pair) != 2 {
		return false, false, fmt.Errorf("ratelimit: unexpected script result shape: %T", res)
	}
	tenantI, _ := pair[0].(int64)
	userI, _ := pair[1].(int64)
	return tenantI == 1, userI == 1, nil
}

// newUniqueMember produces a per-request ZSET member that won't
// collide with concurrent calls landing at the same millisecond.
// Format: `<now-ms>:<16 hex chars>`. The leading timestamp keeps
// the member sortable, which is occasionally useful for debugging
// dumps; the random suffix is what guarantees uniqueness under
// `ZADD` (which would otherwise no-op on a duplicate) and matches
// the value used by the rollback ZREM in the Lua script.
func newUniqueMember(nowMs int64) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strconv.FormatInt(nowMs, 10) + ":" + hex.EncodeToString(b[:]), nil
}
