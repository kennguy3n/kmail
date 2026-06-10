package confidentialsend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

var errMLSBoom = errors.New("mls upstream boom")

// csHTTPHarness wires the production NewHandlers + Register behind a
// dev-bypass OIDC over a real Service, and exposes a request helper.
type csHTTPHarness struct {
	mux    *http.ServeMux
	tenant string
}

func newCSHarness(t *testing.T, svc *Service) *csHTTPHarness {
	t.Helper()
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(svc, nil, nil).Register(mux, authMW)
	return &csHTTPHarness{mux: mux}
}

func (h *csHTTPHarness) authed(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("X-KMail-Dev-Tenant-Id", h.tenant)
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "alice")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *csHTTPHarness) public(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rdr)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// TestHandlersCreateListRevokePortalDB drives the authed CRUD surface
// and the public portal end-to-end against Postgres.
func TestHandlersCreateListRevokePortalDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	svc := NewService(pool)
	h := newCSHarness(t, svc)
	h.tenant = tenant

	// CREATE (link-only, password-protected).
	rec := h.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send", map[string]any{
		"sender_id":          "alice",
		"encrypted_blob_ref": "blob://abc",
		"password":           "hunter2",
		"max_views":          2,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created SecureMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.LinkToken == "" || !created.HasPassword {
		t.Fatalf("unexpected created row: %+v", created)
	}

	// LIST → contains our row.
	rec = h.authed(t, http.MethodGet, "/api/v1/tenants/"+tenant+"/confidential-send?sender_id=alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed []SecureMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list returned %d rows", len(listed))
	}

	// PORTAL GET with no password → 401 (password required).
	rec = h.public(t, http.MethodGet, "/api/v1/secure/"+created.LinkToken, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("portal no-password: code=%d want 401", rec.Code)
	}

	// PORTAL POST with correct password → 200; key never exposed.
	rec = h.public(t, http.MethodPost, "/api/v1/secure/"+created.LinkToken, map[string]string{"password": "hunter2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("portal correct password: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var portal SecureMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &portal)
	if portal.MLSWrappingKey != "" {
		t.Error("portal must never expose mls_wrapping_key")
	}

	// REVOKE → 204.
	rec = h.authed(t, http.MethodDelete, "/api/v1/tenants/"+tenant+"/confidential-send/"+created.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// PORTAL after revoke → 410 Gone.
	rec = h.public(t, http.MethodPost, "/api/v1/secure/"+created.LinkToken, map[string]string{"password": "hunter2"})
	if rec.Code != http.StatusGone {
		t.Errorf("portal after revoke: code=%d want 410", rec.Code)
	}

	// PORTAL unknown token → 404.
	rec = h.public(t, http.MethodGet, "/api/v1/secure/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("portal unknown: code=%d want 404", rec.Code)
	}

	// PORTAL empty token → 400 (mux won't match empty path var, so
	// exercise the guard directly via the handler).
	rec = httptest.NewRecorder()
	NewHandlers(svc, nil, nil).portal(rec, httptest.NewRequest(http.MethodGet, "/api/v1/secure/x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("portal empty token: code=%d want 400", rec.Code)
	}
}

// TestHandlersMLSEndpointsDB covers the MLS status/wrap/rekey routes
// against a service with a mock deriver wired in.
func TestHandlersMLSEndpointsDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	svc := NewService(pool).WithMLS(&mockDeriver{wrapKey: "wrap-abc", rekeyKey: "rekey-def"})
	h := newCSHarness(t, svc)
	h.tenant = tenant

	// STATUS → enabled true.
	rec := h.authed(t, http.MethodGet, "/api/v1/tenants/"+tenant+"/confidential-send/mls/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mls status: code=%d", rec.Code)
	}
	var status map[string]bool
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if !status["enabled"] {
		t.Error("mls status should report enabled")
	}

	// WRAP → 200 with wrapping_key.
	rec = h.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send/mls/wrap", map[string]string{
		"sender_leaf_key":      "leaf",
		"recipient_credential": "bob@x.test",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mls wrap: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Create an MLS-wrapped message so rekey has a target.
	ctx := context.Background()
	wrapped, err := svc.CreateSecureMessage(ctx, CreateRequest{
		TenantID:         tenant,
		SenderID:         "alice",
		EncryptedBlobRef: "blob://mls",
		SenderLeafKey:    "leaf",
		Recipients:       []string{"bob@x.test"},
	})
	if err != nil {
		t.Fatalf("create wrapped: %v", err)
	}

	// REKEY → 200 with new wrapping_key.
	rec = h.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send/"+wrapped.ID+"/mls/rekey", map[string]any{
		"participants": []string{"bob@x.test", "carol@x.test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mls rekey: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// REKEY with empty participants → 400.
	rec = h.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send/"+wrapped.ID+"/mls/rekey", map[string]any{
		"participants": []string{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("rekey empty participants: code=%d want 400", rec.Code)
	}

	// REKEY unknown link → 404.
	rec = h.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send/00000000-0000-0000-0000-000000000000/mls/rekey", map[string]any{
		"participants": []string{"bob@x.test"},
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("rekey unknown link: code=%d want 404", rec.Code)
	}
}

// TestHandlersErrorBranchesDB covers the create + mlsWrap error
// mappings (bad JSON 400, MLS-disabled 503, derive-failure 502).
func TestHandlersErrorBranchesDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)

	// --- MLS-disabled service ---
	plain := newCSHarness(t, NewService(pool))
	plain.tenant = tenant

	// Bad JSON → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "alice")
	rec := httptest.NewRecorder()
	plain.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("create bad json: code=%d want 400", rec.Code)
	}

	// Requesting MLS while disabled → 503.
	rec = plain.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send", map[string]any{
		"sender_id":          "alice",
		"encrypted_blob_ref": "blob://x",
		"sender_leaf_key":    "leaf",
		"recipients":         []string{"bob@x.test"},
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create MLS-disabled: code=%d want 503", rec.Code)
	}

	// mlsWrap while disabled → 503.
	rec = plain.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send/mls/wrap", map[string]string{
		"sender_leaf_key":      "leaf",
		"recipient_credential": "bob@x.test",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("wrap MLS-disabled: code=%d want 503", rec.Code)
	}

	// --- MLS enabled but deriver errors ---
	failing := newCSHarness(t, NewService(pool).WithMLS(&mockDeriver{err: errMLSBoom}))
	failing.tenant = tenant

	// create with MLS inputs → derive fails → 502.
	rec = failing.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send", map[string]any{
		"sender_id":          "alice",
		"encrypted_blob_ref": "blob://y",
		"sender_leaf_key":    "leaf",
		"recipients":         []string{"bob@x.test"},
	})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("create derive-fail: code=%d want 502", rec.Code)
	}

	// mlsWrap derive fails → 502.
	rec = failing.authed(t, http.MethodPost, "/api/v1/tenants/"+tenant+"/confidential-send/mls/wrap", map[string]string{
		"sender_leaf_key":      "leaf",
		"recipient_credential": "bob@x.test",
	})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("wrap derive-fail: code=%d want 502", rec.Code)
	}
}

// TestDerivePlaceholderWrappingKey is a pure-unit test of the
// deterministic local fallback used by tests.
func TestDerivePlaceholderWrappingKey(t *testing.T) {
	a := DerivePlaceholderWrappingKey("leaf", "bob@x.test")
	b := DerivePlaceholderWrappingKey("leaf", "bob@x.test")
	c := DerivePlaceholderWrappingKey("leaf", "carol@x.test")
	if a == "" || a != b {
		t.Errorf("placeholder not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Error("placeholder must differ per recipient")
	}
}

// TestGetSecureMessageLifecycleDB exercises the GetSecureMessage view
// counting, expiry, and views-exceeded branches via the Service.
func TestGetSecureMessageLifecycleDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	ctx := context.Background()

	// Views-exceeded: max_views=1, second view fails.
	svc := NewService(pool)
	m, err := svc.CreateSecureMessage(ctx, CreateRequest{
		TenantID:         tenant,
		SenderID:         "alice",
		EncryptedBlobRef: "blob://views",
		MaxViews:         1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.GetSecureMessage(ctx, m.LinkToken, ""); err != nil {
		t.Fatalf("first view: %v", err)
	}
	if _, err := svc.GetSecureMessage(ctx, m.LinkToken, ""); err != ErrViewsExceeded {
		t.Errorf("second view: want ErrViewsExceeded got %v", err)
	}

	// Expiry: a service whose clock is well past expires_at.
	future := NewService(pool)
	future.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	m2, err := svc.CreateSecureMessage(ctx, CreateRequest{
		TenantID:         tenant,
		SenderID:         "alice",
		EncryptedBlobRef: "blob://exp",
	})
	if err != nil {
		t.Fatalf("create m2: %v", err)
	}
	if _, err := future.GetSecureMessage(ctx, m2.LinkToken, ""); err != ErrLinkExpired {
		t.Errorf("expired view: want ErrLinkExpired got %v", err)
	}

	// Empty token → ErrLinkNotFound.
	if _, err := svc.GetSecureMessage(ctx, "", ""); err != ErrLinkNotFound {
		t.Errorf("empty token: want ErrLinkNotFound got %v", err)
	}
}
