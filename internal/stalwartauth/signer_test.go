package stalwartauth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testSigner(t *testing.T, cfg Config) (*Signer, *rsa.PrivateKey) {
	t.Helper()
	key, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatalf("GenerateEphemeralKey: %v", err)
	}
	s, err := NewSigner(key, cfg)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s, key
}

// publicKeyFromServedJWKS reconstructs the RSA public key from the
// JWKS document the signer serves — mirroring exactly what Stalwart
// does to validate a minted token. Validating against this (rather
// than the in-memory key) proves the served JWKS is self-consistent.
func publicKeyFromServedJWKS(t *testing.T, s *Signer) *rsa.PublicKey {
	t.Helper()
	var set jwkSet
	if err := json.Unmarshal(s.JWKS(), &set); err != nil {
		t.Fatalf("unmarshal jwks: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("jwks: want 1 key, got %d", len(set.Keys))
	}
	k := set.Keys[0]
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nb),
		E: int(new(big.Int).SetBytes(eb).Int64()),
	}
}

func TestMintValidatesAgainstServedJWKS(t *testing.T) {
	s, _ := testSigner(t, Config{Issuer: "https://bff.example.com/oidc/stalwart"})
	tok, err := s.Mint("kmail-dev@kmail.dev")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	pub := publicKeyFromServedJWKS(t, s)

	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tok, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			t.Fatalf("unexpected method %v", token.Header["alg"])
		}
		return pub, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer("https://bff.example.com/oidc/stalwart"),
		jwt.WithAudience(DefaultAudience),
	)
	if err != nil {
		t.Fatalf("parse minted token against served JWKS: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token reported invalid")
	}
	if parsed.Header["kid"] != s.KeyID() {
		t.Errorf("kid: got %v want %s", parsed.Header["kid"], s.KeyID())
	}
	for _, k := range []string{"sub", "email", "preferred_username"} {
		if got := claims[k]; got != "kmail-dev@kmail.dev" {
			t.Errorf("claim %q: got %v want kmail-dev@kmail.dev", k, got)
		}
	}
	if claims["scope"] != "openid email" {
		t.Errorf("scope: got %v", claims["scope"])
	}
	if claims["aud"] != DefaultAudience {
		t.Errorf("aud: got %v", claims["aud"])
	}
}

func TestMintCachesPerPrincipal(t *testing.T) {
	s, _ := testSigner(t, Config{Issuer: "https://bff.example.com"})
	a1, _ := s.Mint("a@kmail.dev")
	a2, _ := s.Mint("a@kmail.dev")
	b1, _ := s.Mint("b@kmail.dev")
	if a1 != a2 {
		t.Error("expected cached token to be identical for same principal")
	}
	if a1 == b1 {
		t.Error("expected distinct tokens for distinct principals")
	}
}

func TestMintEmptyPrincipalRejected(t *testing.T) {
	s, _ := testSigner(t, Config{Issuer: "https://bff.example.com"})
	if _, err := s.Mint("   "); err == nil {
		t.Fatal("expected error for empty principal")
	}
}

func TestVerifyRejectsWrongAudienceAndIssuer(t *testing.T) {
	good, _ := testSigner(t, Config{Issuer: "https://bff.example.com/oidc/stalwart", Audience: "stalwart"})
	tok, err := good.Mint("kmail-dev@kmail.dev")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := good.Verify(tok); err != nil {
		t.Fatalf("Verify own token: %v", err)
	}

	// Same key, different expected audience -> reject.
	wrongAud, err := NewSigner(good.key, Config{Issuer: "https://bff.example.com/oidc/stalwart", Audience: "other"})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := wrongAud.Verify(tok); err == nil {
		t.Error("expected audience mismatch to fail")
	}

	// Same key, different expected issuer -> reject.
	wrongIss, err := NewSigner(good.key, Config{Issuer: "https://evil.example.com", Audience: "stalwart"})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := wrongIss.Verify(tok); err == nil {
		t.Error("expected issuer mismatch to fail")
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	s, _ := testSigner(t, Config{
		Issuer:   "https://bff.example.com",
		TokenTTL: time.Minute,
		now:      func() time.Time { return past },
	})
	tok, err := s.Mint("kmail-dev@kmail.dev")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Verify with the real clock: the token minted an hour ago with a
	// 1m TTL is long expired.
	live, err := NewSigner(s.key, Config{Issuer: "https://bff.example.com"})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := live.Verify(tok); err == nil {
		t.Error("expected expired token to fail validation")
	}
}

func TestDiscoveryHasRequiredFields(t *testing.T) {
	const issuer = "https://bff.example.com/oidc/stalwart"
	s, _ := testSigner(t, Config{Issuer: issuer})
	var doc map[string]any
	if err := json.Unmarshal(s.Discovery(), &doc); err != nil {
		t.Fatalf("unmarshal discovery: %v", err)
	}
	// The five fields Stalwart's deserialiser requires.
	required := map[string]string{
		"issuer":                 issuer,
		"jwks_uri":               issuer + "/jwks.json",
		"userinfo_endpoint":      issuer + "/userinfo",
		"token_endpoint":         issuer + "/token",
		"authorization_endpoint": issuer + "/authorize",
	}
	for k, want := range required {
		got, ok := doc[k].(string)
		if !ok || got == "" {
			t.Errorf("discovery missing required field %q", k)
			continue
		}
		if got != want {
			t.Errorf("discovery[%q]: got %q want %q", k, got, want)
		}
	}
	algs, _ := doc["id_token_signing_alg_values_supported"].([]any)
	if len(algs) == 0 || algs[0] != "RS256" {
		t.Errorf("expected RS256 signing alg, got %v", doc["id_token_signing_alg_values_supported"])
	}
}

func TestIssuerTrailingSlashNormalised(t *testing.T) {
	s, _ := testSigner(t, Config{Issuer: "https://bff.example.com/oidc/stalwart/"})
	if s.Issuer() != "https://bff.example.com/oidc/stalwart" {
		t.Errorf("issuer not trimmed: %q", s.Issuer())
	}
	if s.issuerPath != "/oidc/stalwart" {
		t.Errorf("issuerPath: got %q", s.issuerPath)
	}
}

func TestRootIssuerHasEmptyPath(t *testing.T) {
	s, _ := testSigner(t, Config{Issuer: "https://bff.example.com"})
	if s.issuerPath != "" {
		t.Errorf("issuerPath: got %q want empty", s.issuerPath)
	}
}

func TestNewSignerRejectsBadConfig(t *testing.T) {
	key, _ := GenerateEphemeralKey()
	cases := map[string]Config{
		"empty issuer":    {Issuer: ""},
		"relative issuer": {Issuer: "/oidc/stalwart"},
		"no host":         {Issuer: "https://"},
	}
	for name, cfg := range cases {
		if _, err := NewSigner(key, cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := NewSigner(nil, Config{Issuer: "https://bff.example.com"}); err == nil {
		t.Error("nil key: expected error")
	}
}

func TestDefaultKidIsRFC7638Thumbprint(t *testing.T) {
	s, key := testSigner(t, Config{Issuer: "https://bff.example.com"})
	if s.KeyID() != thumbprint(&key.PublicKey) {
		t.Errorf("kid: got %q want thumbprint", s.KeyID())
	}
	// Explicit kid wins.
	s2, err := NewSigner(key, Config{Issuer: "https://bff.example.com", KeyID: "kmail-bff-1"})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if s2.KeyID() != "kmail-bff-1" {
		t.Errorf("explicit kid not honoured: %q", s2.KeyID())
	}
}

func TestParsePrivateKeyPEM(t *testing.T) {
	key, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}

	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if got, err := ParsePrivateKeyPEM(pkcs1); err != nil || got.N.Cmp(key.N) != 0 {
		t.Errorf("PKCS#1 round-trip failed: err=%v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if got, err := ParsePrivateKeyPEM(pkcs8); err != nil || got.N.Cmp(key.N) != 0 {
		t.Errorf("PKCS#8 round-trip failed: err=%v", err)
	}

	if _, err := ParsePrivateKeyPEM([]byte("not a pem")); err == nil {
		t.Error("expected error for non-PEM input")
	}
}
