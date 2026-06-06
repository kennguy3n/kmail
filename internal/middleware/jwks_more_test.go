package middleware

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func rsaJWKSJSON(t *testing.T, kid string) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	eBytes := big.NewInt(int64(key.PublicKey.E)).Bytes()
	doc := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(eBytes),
	}}}
	b, _ := json.Marshal(doc)
	return string(b), &key.PublicKey
}

func TestJWKSFetcherKeyFuncRSA(t *testing.T) {
	body, pub := rsaJWKSJSON(t, "key-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f, err := NewJWKSFetcher(JWKSConfig{JWKSURL: srv.URL})
	if err != nil {
		t.Fatalf("NewJWKSFetcher: %v", err)
	}
	got, err := f.KeyFunc(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("KeyFunc: %v", err)
	}
	rk, ok := got.(*rsa.PublicKey)
	if !ok || rk.N.Cmp(pub.N) != 0 || rk.E != pub.E {
		t.Errorf("KeyFunc returned wrong key: %#v", got)
	}

	// Unknown kid → error.
	if _, err := f.KeyFunc(context.Background(), "nope"); err == nil {
		t.Error("unknown kid: expected error")
	}
}

func TestJWKSFetcherDiscovery(t *testing.T) {
	body, _ := rsaJWKSJSON(t, "disc-key")
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"jwks_uri":%q}`, base+"/jwks")
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	f, err := NewJWKSFetcher(JWKSConfig{Issuer: srv.URL})
	if err != nil {
		t.Fatalf("NewJWKSFetcher: %v", err)
	}
	if _, err := f.KeyFunc(context.Background(), "disc-key"); err != nil {
		t.Fatalf("KeyFunc via discovery: %v", err)
	}
}

func TestJWKSFetcherStaleRefresh(t *testing.T) {
	body, _ := rsaJWKSJSON(t, "k")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	now := time.Now()
	f, err := NewJWKSFetcher(JWKSConfig{
		JWKSURL: srv.URL, Refresh: time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewJWKSFetcher: %v", err)
	}
	if err := f.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if f.isStale() {
		t.Error("freshly refreshed keyset should not be stale")
	}
	// Advance the clock beyond the refresh window.
	now = now.Add(2 * time.Minute)
	if !f.isStale() {
		t.Error("keyset should be stale after refresh window")
	}
}

func TestNewJWKSFetcherValidation(t *testing.T) {
	if _, err := NewJWKSFetcher(JWKSConfig{}); err == nil {
		t.Error("missing issuer+url: expected error")
	}
}

func TestParseJWKErrors(t *testing.T) {
	if _, err := parseJWK(jwk{Kty: "oct"}); err == nil {
		t.Error("unsupported kty: expected error")
	}
	if _, err := parseRSA(jwk{Kty: "RSA"}); err == nil {
		t.Error("RSA missing n/e: expected error")
	}
	if _, err := parseEC(jwk{Kty: "EC", Crv: "bogus"}); err == nil {
		t.Error("EC bad curve: expected error")
	}

	// Round-trip a real EC key.
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	out, err := parseEC(jwk{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(ek.X.Bytes()),
		Y: base64.RawURLEncoding.EncodeToString(ek.Y.Bytes()),
	})
	if err != nil || out.X.Cmp(ek.X) != 0 {
		t.Errorf("parseEC round-trip: out=%v err=%v", out, err)
	}
}
