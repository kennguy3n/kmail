package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("expected a generated request id on the context")
	}
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Fatalf("response header %q = %q, want %q", RequestIDHeader, got, seen)
	}
	if len(seen) != 32 { // 16 random bytes -> 32 hex chars
		t.Fatalf("generated id %q has unexpected length %d", seen, len(seen))
	}
}

func TestRequestIDPreservesUpstreamID(t *testing.T) {
	const upstream = "edge-abc123"
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, upstream)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != upstream {
		t.Fatalf("request id = %q, want upstream %q", seen, upstream)
	}
	if got := rec.Header().Get(RequestIDHeader); got != upstream {
		t.Fatalf("response header = %q, want %q", got, upstream)
	}
}

func TestRequestIDSanitizesUpstreamID(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Inject a log-forging newline + control chars; they must be stripped.
	req.Header.Set(RequestIDHeader, "abc\r\n injected=1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "abcinjected=1" {
		t.Fatalf("sanitized id = %q, want %q", seen, "abcinjected=1")
	}
}

func TestRequestIDFromEmptyContext(t *testing.T) {
	if got := RequestIDFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "" {
		t.Fatalf("RequestIDFrom on bare context = %q, want empty", got)
	}
}
