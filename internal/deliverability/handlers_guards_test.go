package deliverability

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

func TestStatusFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("w: %w", ErrInvalidInput), http.StatusBadRequest},
		{fmt.Errorf("w: %w", ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("w: %w", ErrSuppressed), http.StatusForbidden},
		{fmt.Errorf("w: %w", ErrSendLimitExceeded), http.StatusTooManyRequests},
		{errors.New("other"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := statusFor(c.err); got != c.want {
			t.Errorf("statusFor(%v)=%d want %d", c.err, got, c.want)
		}
	}
}

func TestAtoiDefault(t *testing.T) {
	if got := atoiDefault("", 7); got != 7 {
		t.Errorf("empty=%d want 7", got)
	}
	if got := atoiDefault("notanumber", 7); got != 7 {
		t.Errorf("invalid=%d want 7", got)
	}
	if got := atoiDefault("42", 7); got != 42 {
		t.Errorf("valid=%d want 42", got)
	}
}

func TestRegisterMountsRoutes(t *testing.T) {
	h, _ := dbHandlers(t)
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-secret",
		Env:            middleware.EnvDevelopment,
	})
	mux := http.NewServeMux()
	h.Register(mux, authMW)

	// Each registered pattern must resolve to a handler (not 404).
	for _, p := range []string{
		"/api/v1/tenants/t1/suppression",
		"/api/v1/tenants/t1/bounces",
		"/api/v1/admin/ip-pools",
		"/api/v1/tenants/t1/ip-pool",
		"/api/v1/tenants/t1/send-limit",
		"/api/v1/tenants/t1/warmup",
		"/api/v1/tenants/t1/dmarc-reports",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s not mounted (404)", p)
		}
	}
}

// TestHandlerGuards_CrossTenant asserts every tenant-scoped handler
// rejects a request whose authenticated tenant differs from the path
// tenant — the core multi-tenant isolation guard.
func TestHandlerGuards_CrossTenant(t *testing.T) {
	h, tenant := dbHandlers(t)
	other := "00000000-0000-0000-0000-000000000000"
	idv := map[string]string{"id": tenant, "email": "x@example.com"}

	scopedHandlers := []struct {
		name   string
		fn     http.HandlerFunc
		method string
	}{
		{"listSuppression", h.listSuppression, http.MethodGet},
		{"addSuppression", h.addSuppression, http.MethodPost},
		{"removeSuppression", h.removeSuppression, http.MethodDelete},
		{"listBounces", h.listBounces, http.MethodGet},
		{"recordBounce", h.recordBounce, http.MethodPost},
		{"getTenantPool", h.getTenantPool, http.MethodGet},
		{"assignTenantPool", h.assignTenantPool, http.MethodPost},
		{"getSendLimit", h.getSendLimit, http.MethodGet},
		{"setSendLimit", h.setSendLimit, http.MethodPatch},
		{"getWarmup", h.getWarmup, http.MethodGet},
		{"uploadDMARC", h.uploadDMARC, http.MethodPost},
		{"listDMARC", h.listDMARC, http.MethodGet},
		{"dmarcSummary", h.dmarcSummary, http.MethodGet},
	}
	for _, sh := range scopedHandlers {
		rec := httptest.NewRecorder()
		// Authenticated as `other` but path says `tenant` ⇒ 403.
		sh.fn(rec, scoped(other, sh.method, "", idv))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-tenant=%d want 403", sh.name, rec.Code)
		}

		// Missing tenant context entirely ⇒ 403.
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(sh.method, "/x", nil)
		req.SetPathValue("id", tenant)
		sh.fn(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s no-context=%d want 403", sh.name, rec.Code)
		}
	}
}

// TestHandlerGuards_BadJSON asserts body-decoding handlers reject
// malformed JSON with 400 (after passing the scope check).
func TestHandlerGuards_BadJSON(t *testing.T) {
	h, tenant := dbHandlers(t)
	idv := map[string]string{"id": tenant}

	bodyHandlers := []struct {
		name   string
		fn     http.HandlerFunc
		method string
	}{
		{"addSuppression", h.addSuppression, http.MethodPost},
		{"recordBounce", h.recordBounce, http.MethodPost},
		{"assignTenantPool", h.assignTenantPool, http.MethodPost},
		{"setSendLimit", h.setSendLimit, http.MethodPatch},
	}
	for _, bh := range bodyHandlers {
		rec := httptest.NewRecorder()
		bh.fn(rec, scoped(tenant, bh.method, `{bad json`, idv))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad-json=%d want 400", bh.name, rec.Code)
		}
	}

	// Admin (non-scoped) pool handlers reject malformed JSON too.
	adminBody := []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"createPool", h.createPool},
		{"addIP", h.addIP},
	}
	for _, ab := range adminBody {
		rec := httptest.NewRecorder()
		ab.fn(rec, scoped(tenant, http.MethodPost, `{bad json`, idv))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad-json=%d want 400", ab.name, rec.Code)
		}
	}
}
