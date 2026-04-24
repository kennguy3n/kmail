package middleware

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// RequestLogger returns middleware that emits a single log line per
// request. Format is controlled by `format`: "json" emits one JSON
// object per line with `method`, `path`, `status`, `duration_ms`,
// `tenant_id`, `user_id`, and `trace_id` fields; anything else (or
// empty) falls back to the previous text format.
func RequestLogger(logger *log.Logger, format string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sr, r)
			elapsed := time.Since(start)
			if format == "json" {
				rec := map[string]any{
					"ts":          start.UTC().Format(time.RFC3339Nano),
					"method":      r.Method,
					"path":        r.URL.Path,
					"status":      sr.status,
					"duration_ms": elapsed.Milliseconds(),
				}
				if tid := TenantIDFrom(r.Context()); tid != "" {
					rec["tenant_id"] = tid
				}
				if uid := KChatUserIDFrom(r.Context()); uid != "" {
					rec["user_id"] = uid
				}
				if trid := TraceIDFrom(r.Context()); trid != "" {
					rec["trace_id"] = trid
				}
				if b, err := json.Marshal(rec); err == nil {
					logger.Println(string(b))
					return
				}
			}
			logger.Printf("%s %s %d %s", r.Method, r.URL.Path, sr.status, elapsed)
		})
	}
}

// statusRecorder wraps http.ResponseWriter so the middleware can
// record the response status code without consuming the body.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wroteHeader {
		return
	}
	s.status = status
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's http.Flusher when
// available. httputil.ReverseProxy (and server-sent events) relies
// on this to stream responses incrementally; without it the proxy
// buffers the entire upstream body before forwarding.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so Go 1.20+
// http.ResponseController can discover optional interfaces
// (Hijacker, Pusher, etc.) through this wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
