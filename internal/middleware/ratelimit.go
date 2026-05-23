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
	"time"

	"github.com/redis/go-redis/v9"
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
// `Allow` is the single sliding-window primitive: it atomically (a)
// drops every member of the sorted set older than `now - window`,
// (b) counts the survivors, and (c) inserts a new member at `now`
// iff the post-insert count would not exceed `limit`. The return
// values are the post-call count and whether the new request was
// admitted. Implementations MUST run all three steps in a single
// atomic context (Lua / MULTI-EXEC).
type RateLimiterStore interface {
	Allow(ctx context.Context, key string, window time.Duration, limit int, now time.Time) (count int64, allowed bool, err error)
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

		tenantKey := fmt.Sprintf("kmail:rl:tenant:%s", tenantID)
		_, allowed, err := r.cfg.Client.Allow(req.Context(), tenantKey, window, r.cfg.TenantRPM, now)
		if err != nil {
			r.cfg.Logger.Printf("ratelimit: tenant allow %s: %v", tenantKey, err)
			next.ServeHTTP(w, req)
			return
		}
		if !allowed {
			writeRateLimitExceeded(w, window, r.cfg.TenantRPM, "tenant")
			return
		}

		if userID != "" {
			userKey := fmt.Sprintf("kmail:rl:user:%s:%s", tenantID, userID)
			_, allowed, err := r.cfg.Client.Allow(req.Context(), userKey, window, r.cfg.UserRPM, now)
			if err != nil {
				r.cfg.Logger.Printf("ratelimit: user allow %s: %v", userKey, err)
				next.ServeHTTP(w, req)
				return
			}
			if !allowed {
				writeRateLimitExceeded(w, window, r.cfg.UserRPM, "user")
				return
			}
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

// slidingWindowScript is the atomic ZSET-based sliding-window log:
//
//	KEYS[1]   the per-scope ZSET key (kmail:rl:tenant:<tid>, ...)
//	ARGV[1]   window duration in milliseconds (integer)
//	ARGV[2]   limit (max admitted requests within the window)
//	ARGV[3]   `now` epoch milliseconds
//	ARGV[4]   unique member to insert (`now-ms:<random hex>`)
//
// The script drops every entry older than `now - window`, counts
// the survivors, and admits-or-rejects atomically. We refresh the
// PEXPIRE on every call so a quiet scope ages out naturally and we
// never have to GC the sorted set out-of-band.
//
// Returns `{post_count, admitted_int}` where `admitted_int` is 1 if
// the new request was added to the set, 0 if it was rejected.
const slidingWindowScript = `
local key       = KEYS[1]
local window    = tonumber(ARGV[1])
local limit     = tonumber(ARGV[2])
local now       = tonumber(ARGV[3])
local member    = ARGV[4]
local cutoff    = now - window

redis.call("ZREMRANGEBYSCORE", key, "-inf", "(" .. tostring(cutoff))
local count = tonumber(redis.call("ZCARD", key))

local admitted = 0
if count < limit then
    redis.call("ZADD", key, now, member)
    count = count + 1
    admitted = 1
end

-- Refresh TTL so an idle key eventually disappears even if the next
-- caller never returns. +5s grace handles small clock skew on the
-- Valkey side.
redis.call("PEXPIRE", key, window + 5000)
return {count, admitted}
`

// RedisStore wraps a *redis.Client so it satisfies the
// RateLimiterStore interface. The sliding-window log is implemented
// as an `EVAL`'d Lua script — the script handle is loaded once
// (`redis.NewScript` caches the SHA) and re-used on every call.
type RedisStore struct {
	Client *redis.Client
	script *redis.Script
}

// NewRedisStore is a convenience constructor that dials Valkey at
// `url` and returns a RedisStore wrapping the client. Callers that
// already own a *redis.Client should use NewRedisStoreFromClient.
func NewRedisStore(url string) (*RedisStore, error) {
	opts, err := parseValkeyURL(url)
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
	return &RedisStore{
		Client: c,
		script: redis.NewScript(slidingWindowScript),
	}
}

func parseValkeyURL(url string) (*redis.Options, error) {
	if url == "" {
		return nil, errors.New("valkey url is empty")
	}
	// Accept both full-DSN (redis://host:port) and bare host:port
	// for convenience — the compose stack exposes the latter.
	if len(url) > 8 && url[:8] == "redis://" || len(url) > 9 && url[:9] == "rediss://" {
		return redis.ParseURL(url)
	}
	return &redis.Options{Addr: url}, nil
}

// Allow runs the sliding-window Lua script against Valkey. The
// member inserted into the ZSET is `<now-ms>:<8-byte-hex>` so two
// requests landing in the same millisecond (which collides under
// ZADD's `score` is the timestamp but `member` is the dedup key)
// still both get counted.
func (s *RedisStore) Allow(ctx context.Context, key string, window time.Duration, limit int, now time.Time) (int64, bool, error) {
	if s.Client == nil {
		return 0, false, errors.New("RedisStore: Client is nil")
	}
	if s.script == nil {
		// Defensive: someone built the struct literally rather
		// than via NewRedisStoreFromClient. Compile lazily.
		s.script = redis.NewScript(slidingWindowScript)
	}
	windowMs := window.Milliseconds()
	if windowMs <= 0 {
		windowMs = int64(time.Minute / time.Millisecond)
	}
	nowMs := now.UnixMilli()
	member, err := newUniqueMember(nowMs)
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: generate member: %w", err)
	}
	res, err := s.script.Run(ctx, s.Client, []string{key}, windowMs, limit, nowMs, member).Result()
	if err != nil {
		return 0, false, fmt.Errorf("ratelimit: EVAL: %w", err)
	}
	pair, ok := res.([]interface{})
	if !ok || len(pair) != 2 {
		return 0, false, fmt.Errorf("ratelimit: unexpected script result shape: %T", res)
	}
	count, _ := pair[0].(int64)
	admittedI, _ := pair[1].(int64)
	return count, admittedI == 1, nil
}

// newUniqueMember produces a per-request ZSET member that won't
// collide with concurrent calls landing at the same millisecond.
// Format: `<now-ms>:<16 hex chars>`. The leading timestamp keeps
// the member sortable, which is occasionally useful for debugging
// dumps; the random suffix is what guarantees uniqueness under
// `ZADD` (which would otherwise no-op on a duplicate).
func newUniqueMember(nowMs int64) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strconv.FormatInt(nowMs, 10) + ":" + hex.EncodeToString(b[:]), nil
}
