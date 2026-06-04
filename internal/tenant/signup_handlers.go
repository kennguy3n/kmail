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
	return &SignupHandlers{
		svc:     cfg.Service,
		limiter: cfg.Limiter,
		metrics: cfg.Metrics,
		logger:  cfg.Logger,
		limit:   cfg.Limit,
		window:  cfg.Window,
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
	ip := clientIP(r)
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

// clientIP extracts the originating client IP, honoring the first hop
// in X-Forwarded-For (set by the ingress / load balancer) and falling
// back to the transport RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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
