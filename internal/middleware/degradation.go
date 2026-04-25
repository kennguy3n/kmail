package middleware

// Graceful-degradation middleware for the 99.95% Phase 5 target.
//
// When the upstream Stalwart shard is unhealthy, read paths
// (Mailbox/get, Email/get) fall back to a Valkey-cached response
// so the inbox stays usable. Write paths return 503 — silently
// dropping a Send is worse than failing it loudly.
//
// The middleware tags successful degraded responses with
// `X-KMail-Degraded: true` so the React app can render a banner
// without inspecting payloads.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DegradationConfig wires the middleware.
type DegradationConfig struct {
	// Cache is the Valkey client used to read cached responses.
	// nil disables the middleware (Wrap returns next unchanged).
	Cache *redis.Client

	// HealthCheck reports whether the upstream Stalwart shard is
	// healthy. When false, GET requests on read paths are served
	// from cache and POST/PUT/DELETE on the same paths return 503.
	// Required.
	HealthCheck func(ctx context.Context) bool

	// ReadPaths is the list of URL prefixes the middleware
	// considers eligible for cached fallback. Defaults to the
	// JMAP read endpoints when empty.
	ReadPaths []string

	// CacheTTL is the TTL applied to cached responses. Defaults
	// to 5 minutes — short enough that stale data is rare,
	// long enough that a 30s outage doesn't fall through.
	CacheTTL time.Duration

	// Logger is used for transient-error diagnostics.
	Logger *log.Logger
}

// Degradation is the middleware.
type Degradation struct {
	cfg DegradationConfig
}

// NewDegradation builds a Degradation. Returns nil when Cache is
// nil so callers can short-circuit wiring.
func NewDegradation(cfg DegradationConfig) *Degradation {
	if cfg.Cache == nil || cfg.HealthCheck == nil {
		return nil
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if len(cfg.ReadPaths) == 0 {
		cfg.ReadPaths = []string{"/jmap"}
	}
	return &Degradation{cfg: cfg}
}

// Wrap returns middleware that consults the health check before
// delegating to `next`. Healthy: pass-through with response
// caching for read paths. Unhealthy: serve from cache (read) or
// 503 (write).
func (d *Degradation) Wrap(next http.Handler) http.Handler {
	if d == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.matchesReadPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		healthy := d.cfg.HealthCheck(r.Context())
		isRead := r.Method == http.MethodGet || r.Method == http.MethodHead
		if healthy {
			d.serveAndCache(w, r, next)
			return
		}
		if !isRead {
			http.Error(w, "upstream temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := d.serveFromCache(w, r); err != nil {
			d.cfg.Logger.Printf("degradation: cache miss for %s: %v", r.URL.Path, err)
			http.Error(w, "upstream temporarily unavailable", http.StatusServiceUnavailable)
		}
	})
}

func (d *Degradation) matchesReadPath(r *http.Request) bool {
	for _, p := range d.cfg.ReadPaths {
		if strings.HasPrefix(r.URL.Path, p) {
			return true
		}
	}
	return false
}

func (d *Degradation) cacheKey(r *http.Request) string {
	return "kmail:degrade:" + r.Method + ":" + r.URL.Path + "?" + r.URL.RawQuery
}

func (d *Degradation) serveAndCache(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		next.ServeHTTP(w, r)
		return
	}
	rec := &recordingWriter{ResponseWriter: w, body: &bytes.Buffer{}, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	if rec.status >= 200 && rec.status < 300 && rec.body.Len() > 0 {
		_ = d.cfg.Cache.Set(r.Context(), d.cacheKey(r), rec.body.Bytes(), d.cfg.CacheTTL).Err()
	}
}

func (d *Degradation) serveFromCache(w http.ResponseWriter, r *http.Request) error {
	val, err := d.cfg.Cache.Get(r.Context(), d.cacheKey(r)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return errCacheMiss
		}
		return err
	}
	w.Header().Set("X-KMail-Degraded", "true")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(val))
	return nil
}

var errCacheMiss = errors.New("degradation: cache miss")

// recordingWriter captures the response body for caching while
// also forwarding it to the underlying ResponseWriter.
type recordingWriter struct {
	http.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (r *recordingWriter) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}
