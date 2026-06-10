package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
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
