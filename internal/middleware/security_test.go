package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurity_HeadersAlwaysSet(t *testing.T) {
	s := NewSecurity(SecurityConfig{WebOrigins: []string{"https://kmail.kchat.dev"}})
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rec, r)

	for _, name := range []string{
		"Content-Security-Policy",
		"Strict-Transport-Security",
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Referrer-Policy",
		"Permissions-Policy",
	} {
		if rec.Header().Get(name) == "" {
			t.Errorf("expected header %s to be set", name)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "https://kmail.kchat.dev") {
		t.Errorf("CSP missing web origin: %s", rec.Header().Get("Content-Security-Policy"))
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options = %s, want DENY", rec.Header().Get("X-Frame-Options"))
	}
}

func TestSecurity_CORSAllowList(t *testing.T) {
	s := NewSecurity(SecurityConfig{WebOrigins: []string{"https://allowed.example"}})
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Origin", "https://allowed.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://allowed.example" {
		t.Errorf("ACAO = %q, want allowed origin", rec.Header().Get("Access-Control-Allow-Origin"))
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Origin", "https://evil.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r2)
	if rec2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("ACAO = %q, want empty for non-allowed origin", rec2.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestSecurity_OptionsPreflight(t *testing.T) {
	s := NewSecurity(SecurityConfig{WebOrigins: []string{"https://allowed.example"}})
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("inner handler should not run on preflight")
	}))
	r := httptest.NewRequest("OPTIONS", "/", nil)
	r.Header.Set("Origin", "https://allowed.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
}

func TestSplitOrigins(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"https://a.example", []string{"https://a.example"}},
		{"https://a.example, https://b.example , https://a.example", []string{"https://a.example", "https://b.example"}},
	}
	for _, tc := range cases {
		got := SplitOrigins(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("SplitOrigins(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitOrigins(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
