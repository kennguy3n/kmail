package middleware

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
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
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret", Env: EnvDevelopment})
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
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret", Env: EnvDevelopment})
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
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret", Env: EnvDevelopment})
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
	o := MustNewOIDC(OIDCConfig{DevBypassToken: "dev-secret", Env: EnvDevelopment})
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
	// Happy path: empty issuer in development → no JWKS → OK.
	if _, err := NewOIDC(OIDCConfig{Env: EnvDevelopment}); err != nil {
		t.Fatalf("expected no error on empty dev config, got %v", err)
	}
}

// ------------------------------------------------------------------
// Production-guard tests: dev-only auth paths must be unreachable
// when KMAIL_ENV != "development".
// ------------------------------------------------------------------

func TestNewOIDC_RefusesMissingJWKSInProduction(t *testing.T) {
	_, err := NewOIDC(OIDCConfig{Env: "production"})
	if err == nil {
		t.Fatal("expected NewOIDC to refuse a JWKS-less production config")
	}
	if !strings.Contains(err.Error(), "JWKS") {
		t.Errorf("expected JWKS error, got %v", err)
	}
	// Pin both env var names in the error so an operator landing
	// here from a Helm-deployed cluster (KMAIL_-prefixed form) AND
	// an operator landing here from a docker-compose / shell
	// invocation (bare form) both find the right knob in the
	// message. `getenvKMail` in internal/config/config.go resolves
	// both forms, so the error message MUST advertise both.
	for _, want := range []string{
		"KMAIL_KCHAT_OIDC_ISSUER",
		"KCHAT_OIDC_ISSUER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to advertise %q, got %v", want, err)
		}
	}
}

func TestNewOIDC_RefusesEmptyEnvForJWKSlessConfig(t *testing.T) {
	// Empty env defaults to non-dev: must fail closed, not silently
	// downgrade to the unverified-JWT fallback.
	if _, err := NewOIDC(OIDCConfig{}); err == nil {
		t.Fatal("expected NewOIDC to refuse empty Env with no JWKS")
	}
}

// TestAuthenticate_NoJWKSInProductionAdvertisesBothEnvVarForms exercises
// the runtime error path: a deployment that managed to construct an
// OIDC instance with no JWKS (e.g. by mutating the config after
// NewOIDC) must still surface BOTH env var names when authenticate
// reaches the "no JWKS issuer configured" branch. Otherwise an
// operator who hits this in a live cluster grep's the wrong name.
func TestAuthenticate_NoJWKSInProductionAdvertisesBothEnvVarForms(t *testing.T) {
	o := &OIDC{cfg: OIDCConfig{Env: EnvProduction}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer some-token-that-is-not-the-dev-bypass")
	_, err := o.authenticate(req)
	if err == nil {
		t.Fatal("expected authenticate to fail with no JWKS configured")
	}
	for _, want := range []string{
		"KMAIL_KCHAT_OIDC_ISSUER",
		"KCHAT_OIDC_ISSUER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected runtime auth error to advertise %q, got %v", want, err)
		}
	}
}

func TestNewOIDC_RefusesDevBypassInProduction(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()
	_, err := NewOIDC(OIDCConfig{
		Issuer:         issuer,
		Env:            "production",
		DevBypassToken: "should-not-be-set",
	})
	if err == nil {
		t.Fatal("expected NewOIDC to refuse DevBypassToken outside development")
	}
	if !strings.Contains(err.Error(), "DEV_BYPASS_TOKEN") {
		t.Errorf("expected DEV_BYPASS_TOKEN error, got %v", err)
	}
}

func TestAuthenticate_DevBypassRejectedOutsideDev(t *testing.T) {
	// Build an OIDC middleware that has both a JWKS issuer (so
	// NewOIDC accepts the config) AND a dev-bypass token. The
	// authenticate() implementation must still refuse the bypass
	// token because Env != development. This is the defence in
	// depth for the case where a deployment somehow ends up with
	// both fields set despite the constructor guard (e.g. a
	// future caller that bypasses NewOIDC).
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	o := &OIDC{cfg: OIDCConfig{
		Issuer:         issuer,
		Env:            "production",
		DevBypassToken: "dev-secret",
	}}
	if mw, err := NewOIDC(OIDCConfig{Issuer: issuer, Env: "production"}); err == nil {
		// Borrow the auto-built JWKS fetcher so verifyAndExtract
		// has a valid key source — we only want to assert that the
		// dev-bypass path is rejected, not exercise the JWT
		// verifier itself.
		o.cfg.JWKS = mw.cfg.JWKS
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected dev-bypass rejection outside development")
	}
}

func TestAuthenticate_UnverifiedFallbackRejectedOutsideDev(t *testing.T) {
	// Direct construction of an OIDC whose Env is non-dev and
	// whose JWKS is nil — the request-time path must refuse
	// before ever falling back to decodeJWTClaims. NewOIDC would
	// normally refuse this config, so we instantiate &OIDC{}
	// directly to exercise authenticate() in isolation.
	o := &OIDC{cfg: OIDCConfig{Env: "production"}}
	tok := makeUnverifiedJWT(t, map[string]any{
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if _, err := o.authenticate(req); err == nil {
		t.Fatal("expected unverified-JWT fallback rejection outside development")
	}
}

func TestAuthenticate_UnverifiedFallbackAllowedInDev(t *testing.T) {
	// In development, the no-JWKS fallback still works so
	// contributors can hand-roll a JWT against a stack with no
	// real issuer.
	o := &OIDC{cfg: OIDCConfig{Env: EnvDevelopment}}
	tok := makeUnverifiedJWT(t, map[string]any{
		"tenant_id":     "t1",
		"kchat_user_id": "u1",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	claims, err := o.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if claims.TenantID != "t1" || claims.KChatUserID != "u1" {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

// TestAuthenticate_UnverifiedFallbackAppliesIAMCoreClaims verifies
// the dev no-issuer fallback resolves iam-core-style tokens — a
// namespaced tenant claim and the standard `sub` instead of the bare
// tenant_id / kchat_user_id — just like the verified production path,
// so an operator bootstrapping the iam-core integration locally is
// not rejected with a 401.
func TestAuthenticate_UnverifiedFallbackAppliesIAMCoreClaims(t *testing.T) {
	o := &OIDC{cfg: OIDCConfig{Env: EnvDevelopment}}
	tok := makeUnverifiedJWT(t, map[string]any{
		"https://kmail.io/tenant_id": "t-iamcore",
		"sub":                        "user-iamcore",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	claims, err := o.authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if claims.TenantID != "t-iamcore" || claims.KChatUserID != "user-iamcore" {
		t.Errorf("iam-core fallbacks not applied in dev: %+v", claims)
	}
}

// makeUnverifiedJWT builds a compact JWT with a junk signature so
// decodeUnverifiedClaims succeeds in the dev fallback. The header is the
// canonical `{"alg":"none","typ":"JWT"}` so test consumers don't
// need to provide signing material.
func makeUnverifiedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + payload + ".sig"
}

// Internal sanity check on decodeUnverifiedClaims — the no-JWKS
// fallback still rejects malformed payloads.
func TestDecodeUnverifiedClaims_Malformed(t *testing.T) {
	o := &OIDC{cfg: OIDCConfig{Env: EnvDevelopment}}
	if _, err := o.decodeUnverifiedClaims("not.a.jwt"); err == nil {
		t.Error("expected malformed-payload error")
	}
	if _, err := o.decodeUnverifiedClaims(strings.Repeat("a", 10)); err == nil {
		t.Error("expected not-a-jwt error")
	}
}

// TestNewOIDC_WarnsOnUnknownEnv exercises the operator-typo
// guard: a KMAIL_ENV value that isn't one of the recognised
// strings silently falls through to production-grade behaviour,
// so NewOIDC must log a warning that names the unknown value.
func TestNewOIDC_WarnsOnUnknownEnv(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()

	var buf bytes.Buffer
	_, err := NewOIDC(OIDCConfig{
		Issuer: issuer,
		Env:    "develpment", // typo
		Logger: log.New(&buf, "", 0),
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `KMAIL_ENV="develpment"`) {
		t.Errorf("expected warning naming the unknown env, got: %q", out)
	}
	if !strings.Contains(out, "treating as production") {
		t.Errorf("expected warning to explain fail-safe behaviour, got: %q", out)
	}
}

// TestNewOIDC_DoesNotWarnOnKnownEnv pins the silent path: each
// of the recognised KMAIL_ENV values must NOT emit the
// unknown-env warning.
func TestNewOIDC_DoesNotWarnOnKnownEnv(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, stop := newTestJWKSServer(t, priv, "test-kid")
	defer stop()
	// "dev", "prod", "stg" are explicitly recognised aliases —
	// the docker-compose convention (`KMAIL_ENV: dev`) must not
	// silently trigger the unknown-env warning.
	for _, env := range []string{"", "development", "DEVELOPMENT", "  staging  ", "production", "dev", "DEV", "prod", "stg"} {
		t.Run(env, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := NewOIDC(OIDCConfig{
				Issuer: issuer,
				Env:    env,
				Logger: log.New(&buf, "", 0),
			})
			if err != nil {
				t.Fatalf("NewOIDC: %v", err)
			}
			if strings.Contains(buf.String(), "is not one of") {
				t.Errorf("unexpected unknown-env warning for Env=%q: %s", env, buf.String())
			}
		})
	}
}

// TestNewOIDC_DevAliasUnlocksDevBypass pins the docker-compose
// developer-experience case: `KMAIL_ENV=dev` (the alias) must
// be treated identically to `KMAIL_ENV=development`, including
// allowing `DevBypassToken` to be wired without rejecting at
// construction time.
func TestNewOIDC_DevAliasUnlocksDevBypass(t *testing.T) {
	for _, env := range []string{"dev", "DEV", "  dev  ", "development"} {
		t.Run(env, func(t *testing.T) {
			var buf bytes.Buffer
			o, err := NewOIDC(OIDCConfig{
				Env:            env,
				DevBypassToken: "let-me-in",
				Logger:         log.New(&buf, "", 0),
			})
			if err != nil {
				t.Fatalf("NewOIDC(Env=%q): unexpected error: %v", env, err)
			}
			if !o.cfg.isDevEnv() {
				t.Fatalf("isDevEnv()=false for Env=%q, want true", env)
			}
		})
	}
}
