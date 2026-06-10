package scheduledsend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// TestHandlersRegisterAndServeDB wires the production NewHandlers +
// Register behind a dev-bypass OIDC and drives the full REST surface
// (list → get → cancel) against a real Service, covering the
// constructor, route registration, and the DB-backed handler paths.
func TestHandlersRegisterAndServeDB(t *testing.T) {
	svc, tenant := newDBService(t, nil)
	ctx := context.Background()

	ss, err := svc.Schedule(ctx, dbScheduleInput(tenant, time.Now().Add(10*time.Minute), "dev-user"))
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(svc).Register(mux, authMW)

	do := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer dev-token")
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
		req.Header.Set("X-KMail-Dev-Kchat-User-Id", "dev-user")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// LIST → 200 with our row.
	rec := do(http.MethodGet, "/api/v1/scheduled-sends")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var listBody struct {
		ScheduledSends []map[string]any `json:"scheduled_sends"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listBody)
	if len(listBody.ScheduledSends) != 1 {
		t.Fatalf("list returned %d rows", len(listBody.ScheduledSends))
	}

	// GET one → 200.
	rec = do(http.MethodGet, "/api/v1/scheduled-sends/"+ss.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// GET unknown id → 404.
	rec = do(http.MethodGet, "/api/v1/scheduled-sends/00000000-0000-0000-0000-000000000000")
	if rec.Code != http.StatusNotFound {
		t.Errorf("get missing: code=%d want 404", rec.Code)
	}

	// DELETE (cancel) → 200; second cancel is idempotent → also 200.
	rec = do(http.MethodDelete, "/api/v1/scheduled-sends/"+ss.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodDelete, "/api/v1/scheduled-sends/"+ss.ID)
	if rec.Code != http.StatusOK {
		t.Errorf("double cancel (idempotent): code=%d want 200", rec.Code)
	}
}

// TestNewHandlersPanicsOnNil documents the loud-wiring contract:
// NewHandlers must panic when handed a nil Service.
func TestNewHandlersPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewHandlers(nil) should panic")
		}
	}()
	NewHandlers(nil)
}
