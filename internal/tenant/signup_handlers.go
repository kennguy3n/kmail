package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// SignupRateLimiter is the narrow slice of the Valkey-backed rate
// limiter store the public signup endpoint needs. `*middleware.RedisStore`
// satisfies it via IncrWithTTL; tests substitute an in-memory fake.
type SignupRateLimiter interface {
	IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// SignupHandlers exposes the public self-service signup endpoints:
//
//	POST /api/v1/signup            — initiate (public, rate-limited per IP)
//	GET  /api/v1/signup/{id}/status — poll status (public)
//
// Both routes are intentionally unauthenticated — the funnel runs
// before any tenant or user exists. The POST is protected by a
// per-IP, fixed-window Valkey counter (default 10/min) so the public
// surface can't be used to spray Stripe Checkout Sessions.
type SignupHandlers struct {
	svc     *SignupService
	limiter SignupRateLimiter
	metrics *SignupMetrics
	logger  *log.Logger

	// limit / window govern the per-IP rate limit on POST /signup.
	limit  int64
	window time.Duration
	// trustedProxyDepth is the number of trusted reverse proxies
	// (ingress / load balancers) in front of this server. It bounds how
	// far from the right of the forwarded chain we read the client IP so
	// a client-supplied X-Forwarded-For prefix can't spoof the rate-limit
	// key. See clientIP.
	trustedProxyDepth int
}

// SignupHandlersConfig wires SignupHandlers.
type SignupHandlersConfig struct {
	Service *SignupService
	// Limiter is optional; when nil the per-IP rate limit is disabled
	// (the endpoint still functions). Production always wires the
	// shared RedisStore.
	Limiter SignupRateLimiter
	Metrics *SignupMetrics
	Logger  *log.Logger
	// Limit is the max POST /signup requests per IP per Window.
	// Defaults to 10. Window defaults to one minute.
	Limit  int64
	Window time.Duration
	// TrustedProxyDepth is the number of trusted reverse proxies in
	// front of this server (e.g. 1 for a single Kubernetes ingress).
	// Only the rightmost TrustedProxyDepth hops of the forwarded chain
	// are trusted, so a spoofed X-Forwarded-For prefix can't forge the
	// rate-limit identity. Defaults to 1. Set to 0 when the server is
	// directly internet-facing (trust only the transport peer and
	// ignore X-Forwarded-For entirely).
	TrustedProxyDepth int
}

// NewSignupHandlers constructs SignupHandlers with sensible defaults.
func NewSignupHandlers(cfg SignupHandlersConfig) *SignupHandlers {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 10
	}
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	// Default an unset (zero) depth to a single ingress hop — the common
	// Kubernetes deployment. A caller that is genuinely internet-facing
	// passes a negative value, which we normalize to depth 0 (ignore
	// X-Forwarded-For, trust only the transport peer).
	switch {
	case cfg.TrustedProxyDepth == 0:
		cfg.TrustedProxyDepth = 1
	case cfg.TrustedProxyDepth < 0:
		cfg.TrustedProxyDepth = 0
	}
	return &SignupHandlers{
		svc:               cfg.Service,
		limiter:           cfg.Limiter,
		metrics:           cfg.Metrics,
		logger:            cfg.Logger,
		limit:             cfg.Limit,
		window:            cfg.Window,
		trustedProxyDepth: cfg.TrustedProxyDepth,
	}
}

// Register installs the public signup routes onto mux. Unlike the
// tenant CRUD routes these are NOT wrapped in the OIDC middleware —
// the signup funnel is pre-auth.
func (h *SignupHandlers) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/signup", http.HandlerFunc(h.initiate))
	mux.Handle("GET /api/v1/signup/{id}/status", http.HandlerFunc(h.status))
}

type initiateSignupRequest struct {
	Email   string `json:"email"`
	OrgName string `json:"org_name"`
	Plan    string `json:"plan"`
}

func (h *SignupHandlers) initiate(w http.ResponseWriter, r *http.Request) {
	if !h.allow(r) {
		h.metrics.rateLimited()
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(h.window.Seconds())))
		writeError(w, http.StatusTooManyRequests, errors.New("rate limit exceeded"))
		return
	}

	var in initiateSignupRequest
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req, err := h.svc.InitiateSignup(r.Context(), in.Email, in.OrgName, in.Plan)
	if err != nil {
		h.logger.Printf("initiateSignup: %v", err)
		writeError(w, statusForSignupError(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

func (h *SignupHandlers) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, err := h.svc.GetStatus(r.Context(), id)
	if errors.Is(err, ErrSignupNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		h.logger.Printf("signupStatus: %v", err)
		writeError(w, statusForSignupError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// allow consults the per-IP fixed-window counter. Fails open on a
// limiter error (logs and admits) so a Valkey blip never takes the
// public signup page offline. Returns true when the request is
// admitted.
func (h *SignupHandlers) allow(r *http.Request) bool {
	if h.limiter == nil {
		return true
	}
	ip := clientIP(r, h.trustedProxyDepth)
	bucket := time.Now().UTC().Truncate(h.window).Unix()
	key := fmt.Sprintf("kmail:signup:rl:%s:%d", ip, bucket)
	count, err := h.limiter.IncrWithTTL(r.Context(), key, h.window)
	if err != nil {
		h.logger.Printf("signup ratelimit: %v (failing open)", err)
		return true
	}
	return count <= h.limit
}

func (m *SignupMetrics) rateLimited() {
	if m != nil && m.RateLimited != nil {
		m.RateLimited.Inc()
	}
}

// clientIP extracts the rate-limit identity for r, resolving the real
// client IP without trusting a spoofable X-Forwarded-For prefix.
//
// The forwarded chain as observed by this server is the X-Forwarded-For
// entries (client-supplied first, then each proxy appends the peer it
// saw) followed by the transport RemoteAddr (the immediate peer). The
// rightmost trustedProxyDepth hops of that chain are our own trusted
// infrastructure; the entry immediately to their left is the furthest
// IP we can still attribute to the real client. Reading from the right
// means a client that pre-seeds X-Forwarded-For with arbitrary IPs only
// pads the (ignored) left of the chain and cannot forge its identity.
//
// With trustedProxyDepth == 0 the server is treated as directly
// internet-facing: X-Forwarded-For is ignored entirely and only the
// transport peer is used.
func clientIP(r *http.Request, trustedProxyDepth int) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if trustedProxyDepth <= 0 {
		return remote
	}
	chain := splitForwardedFor(r.Header.Get("X-Forwarded-For"))
	chain = append(chain, remote)
	idx := len(chain) - 1 - trustedProxyDepth
	if idx < 0 {
		// Fewer hops than trusted proxies (chain shorter than expected,
		// e.g. a proxy didn't append): fall back to the leftmost known
		// entry rather than indexing out of range.
		idx = 0
	}
	return chain[idx]
}

// splitForwardedFor parses a comma-separated X-Forwarded-For header into
// its non-empty, trimmed hops in left-to-right order.
func splitForwardedFor(xff string) []string {
	if strings.TrimSpace(xff) == "" {
		return nil
	}
	parts := strings.Split(xff, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// statusForSignupError maps signup service errors to HTTP statuses.
// Reuses the tenant sentinels (ErrInvalidInput, ErrNotFound) handled
// by statusForServiceError and adds the signup-specific ones.
func statusForSignupError(err error) int {
	switch {
	case errors.Is(err, ErrSignupNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrCheckoutUnavailable):
		return http.StatusServiceUnavailable
	default:
		return statusForServiceError(err)
	}
}
