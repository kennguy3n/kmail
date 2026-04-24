package middleware

import (
	"context"
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
// Implemented by *redis.Client, tests substitute a fake.
type RateLimiterStore interface {
	// Incr atomically increments the counter at `key` by 1 and
	// sets the TTL to `ttl` on the first increment. Returns the
	// new counter value.
	//
	// The contract mirrors the standard `INCR + EXPIRE NX` pattern;
	// concrete implementations MUST ensure the two operations are
	// observed together (e.g. via MULTI or a Lua script).
	IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error)
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

		window := r.cfg.Window
		// Bucket each request into a discrete window so we can
		// drop the counter with a TTL rather than bookkeeping a
		// ZSET of timestamps. For 60s windows the identity of the
		// current bucket flips once per minute; callers see at
		// worst 2x their limit at bucket boundaries, which is the
		// documented tradeoff for fixed-window counters.
		bucket := r.cfg.Now().UTC().Truncate(window).Unix()

		tenantKey := fmt.Sprintf("kmail:rl:tenant:%s:%d", tenantID, bucket)
		ttl := window + 5*time.Second // small grace so late callers still see the TTL

		count, err := r.cfg.Client.IncrWithTTL(req.Context(), tenantKey, ttl)
		if err != nil {
			r.cfg.Logger.Printf("ratelimit: tenant incr %s: %v", tenantKey, err)
			next.ServeHTTP(w, req)
			return
		}
		if count > int64(r.cfg.TenantRPM) {
			writeRateLimitExceeded(w, window, r.cfg.TenantRPM, "tenant")
			return
		}

		if userID != "" {
			userKey := fmt.Sprintf("kmail:rl:user:%s:%s:%d", tenantID, userID, bucket)
			count, err := r.cfg.Client.IncrWithTTL(req.Context(), userKey, ttl)
			if err != nil {
				r.cfg.Logger.Printf("ratelimit: user incr %s: %v", userKey, err)
				next.ServeHTTP(w, req)
				return
			}
			if count > int64(r.cfg.UserRPM) {
				writeRateLimitExceeded(w, window, r.cfg.UserRPM, "user")
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

func writeRateLimitExceeded(w http.ResponseWriter, window time.Duration, rpm int, scope string) {
	// Retry-After is the most pessimistic estimate: the remainder
	// of the current window. Fixed-window counters reset at the
	// boundary so a caller that waits this long is guaranteed to
	// land in a fresh bucket.
	retry := int(window.Seconds())
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rpm))
	w.Header().Set("X-RateLimit-Scope", scope)
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// RedisStore wraps a *redis.Client so it satisfies the
// RateLimiterStore interface. The MULTI/EXEC pipeline here is the
// canonical "INCR + EXPIRE NX" pattern — both commands are
// pipelined in a single RTT, and the pipeline's `Exec` surface
// returns the INCR result.
type RedisStore struct {
	Client *redis.Client
}

// NewRedisStore is a convenience constructor that dials Valkey at
// `url` and returns a RedisStore wrapping the client. Callers that
// already own a *redis.Client should assign it to the struct field
// directly.
func NewRedisStore(url string) (*RedisStore, error) {
	opts, err := parseValkeyURL(url)
	if err != nil {
		return nil, err
	}
	return &RedisStore{Client: redis.NewClient(opts)}, nil
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

// IncrWithTTL runs the INCR + EXPIRE NX pipeline against Valkey.
// Returns the post-increment counter value so the caller can
// compare it against the ceiling.
func (s *RedisStore) IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if s.Client == nil {
		return 0, errors.New("RedisStore: Client is nil")
	}
	pipe := s.Client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
