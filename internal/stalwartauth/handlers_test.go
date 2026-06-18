package stalwartauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, issuer string) (*httptest.Server, *Signer) {
	t.Helper()
	key, err := GenerateEphemeralKey()
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	s, err := NewSigner(key, Config{Issuer: issuer})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(s, nil).Register(mux)
	return httptest.NewServer(mux), s
}

func TestDiscoveryAndJWKSEndpoints(t *testing.T) {
	srv, s := newTestServer(t, "https://bff.example.com/oidc/stalwart")
	defer srv.Close()

	// Endpoints mount under the issuer path.
	for _, path := range []string{"/oidc/stalwart/.well-known/openid-configuration", "/oidc/stalwart/jwks.json"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: content-type %q", path, ct)
		}
		resp.Body.Close()
	}

	// Discovery body matches what the signer serves.
	resp, _ := http.Get(srv.URL + "/oidc/stalwart/.well-known/openid-configuration")
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if doc["issuer"] != s.Issuer() {
		t.Errorf("served issuer %v != %s", doc["issuer"], s.Issuer())
	}
}

func TestUserinfoRequiresValidBearer(t *testing.T) {
	srv, s := newTestServer(t, "https://bff.example.com/oidc/stalwart")
	defer srv.Close()
	url := srv.URL + "/oidc/stalwart/userinfo"

	// No bearer -> 401.
	resp, _ := http.Get(url)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer: status %d want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Garbage bearer -> 401.
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad bearer: status %d want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Valid minted bearer -> 200 with principal claims echoed.
	tok, err := s.Mint("kmail-dev@kmail.dev")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET userinfo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid bearer: status %d want 200", resp.StatusCode)
	}
	var info map[string]any
	json.NewDecoder(resp.Body).Decode(&info)
	if info["email"] != "kmail-dev@kmail.dev" || info["sub"] != "kmail-dev@kmail.dev" {
		t.Errorf("userinfo claims: %v", info)
	}
}

func TestTokenAndAuthorizeReturn501(t *testing.T) {
	srv, _ := newTestServer(t, "https://bff.example.com/oidc/stalwart")
	defer srv.Close()
	for _, path := range []string{"/oidc/stalwart/token", "/oidc/stalwart/authorize"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("GET %s: status %d want 501", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestRootIssuerMountsAtRoot(t *testing.T) {
	srv, _ := newTestServer(t, "https://bff.example.com")
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("root-mounted discovery: status %d", resp.StatusCode)
	}
	resp.Body.Close()
}
