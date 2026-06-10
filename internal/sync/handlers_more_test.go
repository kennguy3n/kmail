package sync

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

func TestStatusFor(t *testing.T) {
	if got := statusFor(ErrInvalidInput); got != http.StatusBadRequest {
		t.Errorf("statusFor(ErrInvalidInput) = %d want 400", got)
	}
	if got := statusFor(errors.New("upstream")); got != http.StatusBadGateway {
		t.Errorf("statusFor(other) = %d want 502", got)
	}
}

func TestNewHandlersNilLoggerDefaults(t *testing.T) {
	h := NewHandlers(nil, nil)
	if h.logger == nil {
		t.Fatal("nil logger should default to log.Default()")
	}
}

// TestIdentifyMissingUser covers the 403 branch when the tenant is
// present but the KChat user is not.
func TestIdentifyMissingUser(t *testing.T) {
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap", nil)
	req = req.WithContext(middleware.WithTenantID(req.Context(), "tenant-1"))
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing user = %d want 403", rec.Code)
	}
}

// TestBootstrapMalformedBody covers the JSON parse-error branch.
func TestBootstrapMalformedBody(t *testing.T) {
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap", bytes.NewReader([]byte("{not json")))
	ctx := middleware.WithTenantID(req.Context(), "tenant-1")
	ctx = middleware.WithKChatUserID(ctx, "kchat-user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

// TestRegisterRoutes drives the bootstrap route through a real mux
// behind dev-bypass OIDC so Register is exercised.
func TestRegisterRoutes(t *testing.T) {
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(svc, log.New(io.Discard, "", 0)).Register(mux, authMW)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap?limit=10", nil)
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("X-KMail-Dev-Tenant-Id", "tenant-1")
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "kchat-user-1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap via mux = %d body=%s", rec.Code, rec.Body.String())
	}
}
