package middleware

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// resolveIdentity unit tests — the iam-core claim-fallback rules.

func TestResolveIdentity(t *testing.T) {
	o := MustNewOIDC(OIDCConfig{Env: EnvDevelopment})

	cases := []struct {
		name                           string
		tenantID, kchatUserID          string
		namespacedTenantID, sub        string
		wantTenant, wantUser           string
	}{
		{
			name: "kchat token uses direct claims",
			tenantID: "t1", kchatUserID: "u1",
			namespacedTenantID: "", sub: "sub-x",
			wantTenant: "t1", wantUser: "u1",
		},
		{
			name: "iamcore token falls back to namespaced tenant and sub",
			tenantID: "", kchatUserID: "",
			namespacedTenantID: "tenant-ns", sub: "sub-iam",
			wantTenant: "tenant-ns", wantUser: "sub-iam",
		},
		{
			name: "direct tenant wins over namespaced",
			tenantID: "t-direct", kchatUserID: "",
			namespacedTenantID: "t-ns", sub: "sub-iam",
			wantTenant: "t-direct", wantUser: "sub-iam",
		},
		{
			name: "direct kchat user wins over sub",
			tenantID: "t1", kchatUserID: "u-direct",
			namespacedTenantID: "", sub: "sub-iam",
			wantTenant: "t1", wantUser: "u-direct",
		},
		{
			name: "all empty resolves empty",
			wantTenant: "", wantUser: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTenant, gotUser := o.resolveIdentity(tc.tenantID, tc.kchatUserID, tc.namespacedTenantID, tc.sub)
			if gotTenant != tc.wantTenant || gotUser != tc.wantUser {
				t.Errorf("resolveIdentity = (%q, %q), want (%q, %q)", gotTenant, gotUser, tc.wantTenant, tc.wantUser)
			}
		})
	}
}

// TestResolveIdentity_FallbackWarningsLoggedOnce verifies each
// claim-fallback warning is emitted at most once per OIDC instance,
// even when every request uses the fallback path. Without the
// sync.Once guard a deployment whose iam-core tokens always carry
// the namespaced/sub claims would log on every authenticated
// request and flood log aggregation.
func TestResolveIdentity_FallbackWarningsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	o := MustNewOIDC(OIDCConfig{Env: EnvDevelopment, Logger: log.New(&buf, "", 0)})

	for i := 0; i < 5; i++ {
		gotTenant, gotUser := o.resolveIdentity("", "", "tenant-ns", "sub-iam")
		if gotTenant != "tenant-ns" || gotUser != "sub-iam" {
			t.Fatalf("resolveIdentity = (%q, %q), want (tenant-ns, sub-iam)", gotTenant, gotUser)
		}
	}

	out := buf.String()
	if got := strings.Count(out, "tenant_id claim missing"); got != 1 {
		t.Errorf("tenant fallback warning logged %d times, want 1\n%s", got, out)
	}
	if got := strings.Count(out, "kchat_user_id claim missing"); got != 1 {
		t.Errorf("user fallback warning logged %d times, want 1\n%s", got, out)
	}
}

// Integration: a JWT carrying the iam-core claim structure
// (namespaced tenant + bare sub, no kchat_user_id/tenant_id) must be
// accepted and mapped onto KMail's identity.

func TestAuthenticate_AcceptsIAMCoreToken(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	issuer, stop := newTestJWKSServer(t, priv, "iam-kid")
	defer stop()

	o, err := NewOIDC(OIDCConfig{Issuer: issuer, Audience: "https://api.kmail.io"})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}

	token := issueToken(t, priv, "iam-kid", jwt.MapClaims{
		"iss":                     issuer,
		"aud":                     []string{"https://api.kmail.io"},
		"exp":                     time.Now().Add(time.Hour).Unix(),
		"iat":                     time.Now().Unix(),
		"sub":                     "iamcore-user-42",
		"https://kmail.io/tenant_id": "iamcore-tenant-7",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	claims, err := o.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if claims.TenantID != "iamcore-tenant-7" {
		t.Errorf("tenant = %q, want iamcore-tenant-7", claims.TenantID)
	}
	if claims.KChatUserID != "iamcore-user-42" {
		t.Errorf("user = %q, want iamcore-user-42 (sub fallback)", claims.KChatUserID)
	}
}

func TestAuthenticate_RejectsTokenMissingTenant(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "iam-kid")
	defer stop()

	o, _ := NewOIDC(OIDCConfig{Issuer: issuer, Audience: "https://api.kmail.io"})

	// sub present (so user resolves) but no tenant claim of any kind.
	token := issueToken(t, priv, "iam-kid", jwt.MapClaims{
		"iss": issuer,
		"aud": []string{"https://api.kmail.io"},
		"exp": time.Now().Add(time.Hour).Unix(),
		"sub": "iamcore-user-42",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected rejection when no tenant claim is present")
	}
}

// PostAuthMiddleware (the chokepoint lazy provisioning hangs off)
// must run AFTER authentication has populated the tenant id in the
// request context, and BEFORE the wrapped handler. This is the
// ordering main.go relies on to provision the authenticated tenant.
func TestWrap_PostAuthMiddlewareSeesAuthenticatedTenant(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	issuer, stop := newTestJWKSServer(t, priv, "iam-kid")
	defer stop()

	var seenTenant string
	var handlerRan bool
	postAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenTenant = TenantIDFrom(r.Context())
			next.ServeHTTP(w, r)
		})
	}
	o, err := NewOIDC(OIDCConfig{
		Issuer:             issuer,
		Audience:           "https://api.kmail.io",
		PostAuthMiddleware: postAuth,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}

	handler := o.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerRan = true
		w.WriteHeader(http.StatusOK)
	}))

	token := issueToken(t, priv, "iam-kid", jwt.MapClaims{
		"iss":                        issuer,
		"aud":                        []string{"https://api.kmail.io"},
		"exp":                        time.Now().Add(time.Hour).Unix(),
		"sub":                        "u1",
		"https://kmail.io/tenant_id": "tenant-postauth",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !handlerRan {
		t.Fatalf("status = %d, handlerRan = %v; want handler to run", w.Code, handlerRan)
	}
	if seenTenant != "tenant-postauth" {
		t.Errorf("PostAuthMiddleware saw tenant %q, want tenant-postauth (must run after auth)", seenTenant)
	}
}

// An unauthenticated request must be rejected before
// PostAuthMiddleware runs — provisioning must never fire for a
// request that failed authentication.
func TestWrap_PostAuthMiddlewareSkippedOnAuthFailure(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "iam-kid")
	defer stop()

	postAuthRan := false
	postAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			postAuthRan = true
			next.ServeHTTP(w, r)
		})
	}
	o, _ := NewOIDC(OIDCConfig{Issuer: issuer, Audience: "https://api.kmail.io", PostAuthMiddleware: postAuth})
	handler := o.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) // no Authorization
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if postAuthRan {
		t.Error("PostAuthMiddleware ran for an unauthenticated request")
	}
}
