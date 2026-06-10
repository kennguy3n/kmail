package cmk

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestCMKRegisterRoutesMountDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	h := NewHandlers(svc, pool, log.New(io.Discard, "", 0))
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-secret",
		Env:            middleware.EnvDevelopment,
	})
	mux := http.NewServeMux()
	h.Register(mux, authMW)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/tenants/" + tenant + "/cmk"},
		{http.MethodGet, "/api/v1/tenants/" + tenant + "/cmk/active"},
		{http.MethodGet, "/api/v1/tenants/" + tenant + "/cmk/hsm"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		// Without a dev-bypass header the auth wrapper rejects, but a
		// 404 specifically means the route was never registered.
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s %s not mounted (404)", tc.method, tc.path)
		}
	}
}

func TestCMKHandlerBadJSONDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	h := NewHandlers(svc, pool, log.New(io.Discard, "", 0))
	idv := map[string]string{"id": tenant}

	for name, fn := range map[string]http.HandlerFunc{
		"register":    h.register,
		"rotate":      h.rotate,
		"registerHSM": h.registerHSM,
	} {
		rec := httptest.NewRecorder()
		fn(rec, cmkReq(tenant, http.MethodPost, `{not-json`, idv))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad json=%d want 400 body=%s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestCMKLookupPlanNilPool(t *testing.T) {
	// With a nil pool lookupPlan returns ("", nil); register then
	// proceeds with an empty plan and the service rejects it as
	// not privacy-eligible (403).
	svc := NewCMKServiceWithEnvelope(nil, newTestEnvelope(t))
	h := NewHandlers(svc, nil, log.New(io.Discard, "", 0))
	rec := httptest.NewRecorder()
	h.register(rec, cmkReq("t", http.MethodPost, `{"public_key_pem":`+jsonStr(pubPEM(t))+`}`, map[string]string{"id": "t"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("register nil-pool plan=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
}
