package priority

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

var errSourceDown = errors.New("source down")

func TestClampLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 20},      // empty → default
		{"abc", 20},   // non-digit → default
		{"5x", 20},    // trailing non-digit → default
		{"0", 20},     // zero → default
		{"50", 50},    // in-range
		{"100", 100},  // exactly max
		{"9999", 100}, // over max → clamped
		{"  ", 20},    // whitespace only → default
	}
	for _, c := range cases {
		if got := clampLimit(c.raw, 20, 100); got != c.want {
			t.Errorf("clampLimit(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestPriorityRegisterRoutes exercises Register end-to-end via a mux
// behind dev-bypass OIDC (which sets both tenant + kchat user).
func TestPriorityRegisterRoutes(t *testing.T) {
	src := &fakeSource{}
	svc, _ := NewService(Config{Source: src})
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(svc, nil, nil).Register(mux, authMW)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/priority-inbox?limit=10", nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", "tenant-a")
	r.Header.Set("X-KMail-Dev-Kchat-User-Id", "kchat-a")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET priority-inbox via mux = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPriorityInboxComputeError covers the 502 branch when the source
// fails.
func TestPriorityInboxComputeError(t *testing.T) {
	src := &fakeSource{err: errSourceDown}
	svc, _ := NewService(Config{Source: src})
	h := NewHandlers(svc, nil, nil)
	w := httptest.NewRecorder()
	h.priorityInbox(w, authed(http.MethodGet, "/api/v1/priority-inbox"))
	if w.Code != http.StatusBadGateway {
		t.Errorf("compute error = %d want 502", w.Code)
	}
}
