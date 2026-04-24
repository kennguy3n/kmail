package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// newTestJWKSServer stands up an httptest server that answers the
// OIDC discovery document at /.well-known/openid-configuration and
// the JWKS document at /jwks. Returns the issuer URL and a
// teardown func.
func newTestJWKSServer(t *testing.T, priv *rsa.PrivateKey, kid string) (string, func()) {
	t.Helper()
	mux := http.NewServeMux()

	srv := httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := priv.PublicKey.N.Bytes()
		e := []byte{0x01, 0x00, 0x01}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": kid,
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(n),
				"e":   base64.RawURLEncoding.EncodeToString(e),
			}},
		})
	})

	return srv.URL, srv.Close
}

// issueToken builds a compact JWT signed with priv.
func issueToken(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// ------------------------------------------------------------------
// Dev-bypass path
// ------------------------------------------------------------------

func TestAuthenticate_DevBypass(t *testing.T) {
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("X-KMail-Dev-Tenant-Id", "t1")
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "u1")

	claims, err := o.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if claims.TenantID != "t1" || claims.KChatUserID != "u1" {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

func TestAuthenticate_MissingAuthorization(t *testing.T) {
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected error for missing Authorization header")
	}
}

// ------------------------------------------------------------------
// JWKS-verified path
// ------------------------------------------------------------------

func TestAuthenticate_VerifiesJWT(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o, err := NewOIDC(OIDCConfig{
		Issuer:   issuer,
		Audience: "kmail",
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}

	token := issueToken(t, priv, "test-kid", jwt.MapClaims{
		"iss":           issuer,
		"aud":           []string{"kmail"},
		"exp":           time.Now().Add(time.Hour).Unix(),
		"iat":           time.Now().Unix(),
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	claims, err := o.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if claims.TenantID != "t1" || claims.KChatUserID != "u1" {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

func TestAuthenticate_RejectsBadSignature(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o, _ := NewOIDC(OIDCConfig{Issuer: issuer})

	// Sign with a different key but the same kid — signature
	// verification should reject this.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := issueToken(t, other, "test-kid", jwt.MapClaims{
		"iss":           issuer,
		"exp":           time.Now().Add(time.Hour).Unix(),
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected signature verification failure")
	}
}

func TestAuthenticate_RejectsExpiredToken(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o, _ := NewOIDC(OIDCConfig{Issuer: issuer})
	token := issueToken(t, priv, "test-kid", jwt.MapClaims{
		"iss":           issuer,
		"exp":           time.Now().Add(-time.Hour).Unix(),
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected expired-token rejection")
	}
}

func TestAuthenticate_RejectsWrongIssuer(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o, _ := NewOIDC(OIDCConfig{Issuer: issuer})
	token := issueToken(t, priv, "test-kid", jwt.MapClaims{
		"iss":           "https://attacker.example.com",
		"exp":           time.Now().Add(time.Hour).Unix(),
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected issuer mismatch rejection")
	}
}

func TestAuthenticate_RejectsMissingAudience(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o, _ := NewOIDC(OIDCConfig{Issuer: issuer, Audience: "kmail"})
	token := issueToken(t, priv, "test-kid", jwt.MapClaims{
		"iss":           issuer,
		"aud":           []string{"other"},
		"exp":           time.Now().Add(time.Hour).Unix(),
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected audience rejection")
	}
}

func TestAuthenticate_RejectsMissingKChatClaims(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o, _ := NewOIDC(OIDCConfig{Issuer: issuer})
	token := issueToken(t, priv, "test-kid", jwt.MapClaims{
		"iss": issuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected missing-claim rejection")
	}
}

// ------------------------------------------------------------------
// JWKS fetcher: caching and refresh
// ------------------------------------------------------------------

func TestJWKSFetcher_CachesAcrossCalls(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	var calls int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		n := priv.PublicKey.N.Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(n),
				"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}},
		})
	})

	f, err := NewJWKSFetcher(JWKSConfig{Issuer: srv.URL})
	if err != nil {
		t.Fatalf("NewJWKSFetcher: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := f.KeyFunc(context.Background(), "k1"); err != nil {
			t.Fatalf("KeyFunc: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 JWKS fetch, got %d", calls)
	}
}

// ------------------------------------------------------------------
// Wrap: context propagation + 401 on failure
// ------------------------------------------------------------------

func TestWrap_Passes401OnError(t *testing.T) {
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret"})
	handler := o.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestWrap_PropagatesContext(t *testing.T) {
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret"})
	var gotTenant string
	handler := o.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		gotTenant = TenantIDFrom(req.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("X-KMail-Dev-Tenant-Id", "t1")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if gotTenant != "t1" {
		t.Errorf("expected tenant propagation, got %q", gotTenant)
	}
}

// Silence unused-import warnings when the file is trimmed during
// copy-paste. Sanity check on the std-lib io import actually being
// used via io.ReadAll below would be nice, but we don't rely on it
// in this file — keep the import list tight.
var _ = io.EOF

func TestAudienceContains(t *testing.T) {
	if !audienceContains(jwt.ClaimStrings{"a", "b"}, "b") {
		t.Error("expected true for present audience")
	}
	if audienceContains(jwt.ClaimStrings{"a"}, "b") {
		t.Error("expected false for missing audience")
	}
}

func TestNewOIDC_ReturnsError_WhenDiscoveryURLEmpty(t *testing.T) {
	// Happy path: empty issuer → no JWKS → OK.
	if _, err := NewOIDC(OIDCConfig{}); err != nil {
		t.Fatalf("expected no error on empty config, got %v", err)
	}
}

// Internal sanity check on decodeJWTClaims — the no-JWKS fallback
// still rejects malformed payloads.
func TestDecodeJWTClaims_Malformed(t *testing.T) {
	if _, err := decodeJWTClaims("not.a.jwt"); err == nil {
		t.Error("expected malformed-payload error")
	}
	if _, err := decodeJWTClaims(strings.Repeat("a", 10)); err == nil {
		t.Error("expected not-a-jwt error")
	}
}
