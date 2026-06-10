package undosend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

func TestNewHandlersNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewHandlers(nil) should panic")
		}
	}()
	NewHandlers(nil)
}

func TestCancelHandler_GuardBranches(t *testing.T) {
	h, _ := newTestHandlers(t)

	// Missing tenant context → 500.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/send/x/cancel", nil)
	r.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.cancel(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("cancel no-tenant = %d want 500", w.Code)
	}

	// Missing id → 400.
	r = newAuthedRequest(http.MethodPost, "/api/v1/send//cancel", "tenant-a")
	r.SetPathValue("id", "")
	w = httptest.NewRecorder()
	h.cancel(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("cancel empty-id = %d want 400", w.Code)
	}
}

func TestStatusHandler_GuardBranches(t *testing.T) {
	h, _ := newTestHandlers(t)

	// Missing tenant context → 500.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/send/x", nil)
	r.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.status(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status no-tenant = %d want 500", w.Code)
	}

	// Missing id → 400.
	r = newAuthedRequest(http.MethodGet, "/api/v1/send/", "tenant-a")
	r.SetPathValue("id", "")
	w = httptest.NewRecorder()
	h.status(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status empty-id = %d want 400", w.Code)
	}
}

// TestStatusHandler_CrossTenantIs404 covers the ps.TenantID != tenant
// leak-prevention branch.
func TestStatusHandler_CrossTenantIs404(t *testing.T) {
	h, svc := newTestHandlers(t)
	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	r := newAuthedRequest(http.MethodGet, "/api/v1/send/"+ps.ID, "tenant-OTHER")
	r.SetPathValue("id", ps.ID)
	w := httptest.NewRecorder()
	h.status(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status cross-tenant = %d want 404", w.Code)
	}
}

// TestRegisterRoutes drives both routes through a real mux behind
// dev-bypass OIDC so Register is exercised.
func TestRegisterRoutes(t *testing.T) {
	h, svc := newTestHandlers(t)
	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux, authMW)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/send/"+ps.ID, nil)
	r.Header.Set("Authorization", "Bearer dev-token")
	r.Header.Set("X-KMail-Dev-Tenant-Id", "tenant-a")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status via mux = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"email_id":"email-1"`) {
		t.Errorf("missing email_id in body: %s", w.Body.String())
	}
}
