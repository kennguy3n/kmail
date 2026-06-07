package cmk

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func pubPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func cmkReq(tenant, method, body string, pv map[string]string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), tenant)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestCMKKeyHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	h := NewHandlers(svc, pool, log.New(io.Discard, "", 0))
	idv := map[string]string{"id": tenant}

	// active with no key → {"key": nil}
	rec := httptest.NewRecorder()
	h.active(rec, cmkReq(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Fatalf("active(empty)=%d body=%s", rec.Code, rec.Body.String())
	}

	// register a key
	rec = httptest.NewRecorder()
	h.register(rec, cmkReq(tenant, http.MethodPost, `{"public_key_pem":`+jsonStr(pubPEM(t))+`}`, idv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register=%d body=%s", rec.Code, rec.Body.String())
	}
	var key Key
	if err := json.Unmarshal(rec.Body.Bytes(), &key); err != nil {
		t.Fatalf("decode key: %v", err)
	}

	// list
	rec = httptest.NewRecorder()
	h.list(rec, cmkReq(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("list=%d", rec.Code)
	}

	// rotate
	rec = httptest.NewRecorder()
	h.rotate(rec, cmkReq(tenant, http.MethodPost, `{"public_key_pem":`+jsonStr(pubPEM(t))+`}`, idv))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate=%d body=%s", rec.Code, rec.Body.String())
	}
	var rotated Key
	_ = json.Unmarshal(rec.Body.Bytes(), &rotated)

	// active now returns the rotated key
	rec = httptest.NewRecorder()
	h.active(rec, cmkReq(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Errorf("active=%d", rec.Code)
	}

	// revoke the rotated key
	rec = httptest.NewRecorder()
	h.revoke(rec, cmkReq(tenant, http.MethodDelete, "", map[string]string{"id": tenant, "keyId": rotated.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("revoke=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCMKRegisterRejectsNonPrivacyPlanDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "core", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	h := NewHandlers(svc, pool, log.New(io.Discard, "", 0))

	rec := httptest.NewRecorder()
	h.register(rec, cmkReq(tenant, http.MethodPost, `{"public_key_pem":`+jsonStr(pubPEM(t))+`}`, map[string]string{"id": tenant}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("register core plan=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
}

func TestCMKHSMHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	h := NewHandlers(svc, pool, log.New(io.Discard, "", 0))
	idv := map[string]string{"id": tenant}

	// list HSM (empty)
	rec := httptest.NewRecorder()
	h.listHSM(rec, cmkReq(tenant, http.MethodGet, "", idv))
	if rec.Code != http.StatusOK {
		t.Fatalf("listHSM=%d", rec.Code)
	}

	// register a KMIP HSM config
	rec = httptest.NewRecorder()
	h.registerHSM(rec, cmkReq(tenant, http.MethodPost,
		`{"provider_type":"kmip","endpoint":"kmips://hsm.example.com:5696","credentials":"user:pass"}`, idv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("registerHSM=%d body=%s", rec.Code, rec.Body.String())
	}
	var cfg HSMConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)

	// test the connection (stub provider)
	rec = httptest.NewRecorder()
	h.testHSM(rec, cmkReq(tenant, http.MethodPost, "", map[string]string{"id": tenant, "configId": cfg.ID}))
	if rec.Code != http.StatusOK {
		t.Logf("testHSM=%d body=%s", rec.Code, rec.Body.String())
	}

	// invalid provider → 400
	rec = httptest.NewRecorder()
	h.registerHSM(rec, cmkReq(tenant, http.MethodPost, `{"provider_type":"bogus","endpoint":"x"}`, idv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("registerHSM bad provider=%d want 400", rec.Code)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
