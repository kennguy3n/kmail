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
	hsts := rec.Header().Get("Strict-Transport-Security")
	for _, want := range []string{"max-age=", "includeSubDomains", "preload"} {
		if !strings.Contains(hsts, want) {
			t.Errorf("HSTS missing %q: %s", want, hsts)
		}
	}
	// CSP must not weaken script-src with unsafe-inline / unsafe-eval.
	csp := rec.Header().Get("Content-Security-Policy")
	for _, banned := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(scriptSrc(csp), banned) {
			t.Errorf("CSP script-src contains %q: %s", banned, csp)
		}
	}
	if pp := rec.Header().Get("Permissions-Policy"); !strings.Contains(pp, "camera=()") ||
		!strings.Contains(pp, "microphone=()") || !strings.Contains(pp, "geolocation=()") {
		t.Errorf("Permissions-Policy must disable camera/microphone/geolocation: %s", pp)
	}
}

// scriptSrc extracts the `script-src ...` directive from a CSP
// string so the unsafe-inline assertion targets the script context
// specifically (style-src legitimately keeps 'unsafe-inline' for
// the inline React.CSSProperties the app emits).
func scriptSrc(csp string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, "script-src") {
			return d
		}
	}
	return ""
}

func TestSecurity_HSTSPreloadDisabled(t *testing.T) {
	s := NewSecurity(SecurityConfig{
		WebOrigins:         []string{"https://kmail.kchat.dev"},
		DisableHSTSPreload: true,
	})
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if hsts := rec.Header().Get("Strict-Transport-Security"); strings.Contains(hsts, "preload") {
		t.Errorf("expected no preload token when disabled: %s", hsts)
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
