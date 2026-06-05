package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader is the canonical header used to carry the
// per-request correlation ID into and out of the service. It matches
// the de-facto convention most load balancers / proxies (nginx, Envoy,
// ALB) already emit, so an upstream-assigned ID is reused rather than
// replaced — giving a single ID that spans the edge, the API, the
// workers, and any logs they emit.
const RequestIDHeader = "X-Request-Id"

// RequestID returns middleware that ensures every request carries a
// correlation ID. If the inbound request already has one (set by an
// upstream proxy) it is preserved; otherwise a fresh random ID is
// minted. The ID is stored on the context (see RequestIDFrom) and
// echoed back on the response so a client/operator can quote it when
// reporting an issue.
//
// Unlike the W3C trace id (which only exists when OTLP tracing is
// configured), the request id is ALWAYS present, so it is the reliable
// join key for tenant-scoped log aggregation.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithRequestID stashes a request id on a context. It exists so code
// paths that originate work outside the HTTP server (e.g. a background
// worker processing a job row that recorded the originating request
// id) can propagate the same correlation id into their logs.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFrom returns the request id on the context, or "" if none
// was set. Exported so the structured logger and downstream callers
// can include it.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// newRequestID returns a 128-bit random hex id. crypto/rand failure is
// effectively impossible; if it ever happens we fall back to a fixed
// sentinel rather than panicking a request over a log-correlation id.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

// sanitizeRequestID bounds an upstream-supplied id so a malicious or
// buggy client can't inject log-forging characters (newlines) or
// unbounded strings into our logs via the correlation header.
func sanitizeRequestID(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		// Printable ASCII excluding space/control; keeps ids log-safe.
		if r > 0x20 && r < 0x7f {
			out = append(out, r)
		}
	}
	return string(out)
}
