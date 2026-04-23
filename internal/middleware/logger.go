package middleware

import (
	"log"
	"net/http"
	"time"
)

// RequestLogger returns middleware that emits a single structured
// log line per request with method, path, status, and latency. It is
// deliberately stdlib-only; richer structured logging (OpenTelemetry
// + Loki) lands in Phase 3 per `docs/ARCHITECTURE.md` §12.
func RequestLogger(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sr, r)
			logger.Printf("%s %s %d %s", r.Method, r.URL.Path, sr.status, time.Since(start))
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
