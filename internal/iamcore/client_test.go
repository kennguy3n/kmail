package iamcore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer stands up a mock iam-core exposing the M2M token
// endpoint and the management resources the Client calls. tokenHits
// counts how many times /oauth2/token was hit so token-cache
// behaviour can be asserted.
func newTestServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var tokenHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenHits, 1)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") != "kmail" || r.Form.Get("client_secret") != "s3cret" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "tok-123",
			TokenType:   "Bearer",
			ExpiresIn:   3600,
		})
	})
	mux.HandleFunc("/api/v1/management/users/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			http.Error(w, "unauthorized: "+got, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Tenant-ID") != "tenant-a" {
			http.Error(w, "wrong tenant", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/api/v1/management/users/missing" {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(User{
			UserID:   "user-1",
			TenantID: "tenant-a",
			Email:    "ada@acme.com",
			Name:     "Ada Lovelace",
		})
	})
	mux.HandleFunc("/api/v1/management/tenants/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Tenant{ID: "tenant-a", Name: "Acme", Slug: "acme"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &tokenHits
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Config{
		MgmtURL:      baseURL,
		ClientID:     "kmail",
		ClientSecret: "s3cret",
		Audience:     "https://tenant-a/api/v1/management/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNew_ValidatesRequiredFields(t *testing.T) {
	cases := map[string]Config{
		"no mgmt url": {ClientID: "a", ClientSecret: "b", Audience: "c"},
		"no client":   {MgmtURL: "https://x", Audience: "c"},
		"no audience": {MgmtURL: "https://x", ClientID: "a", ClientSecret: "b"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestNew_DerivesMgmtTenantFromAudience(t *testing.T) {
	c, err := New(Config{
		MgmtURL:      "https://auth.kmail.io/",
		ClientID:     "a",
		ClientSecret: "b",
		Audience:     "https://mgmt-tenant/api/v1/management/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.mgmtTenantID != "mgmt-tenant" {
		t.Errorf("mgmtTenantID = %q, want mgmt-tenant", c.mgmtTenantID)
	}
	// MgmtURL trailing slash must be trimmed so path joins are clean.
	if c.mgmtURL != "https://auth.kmail.io" {
		t.Errorf("mgmtURL = %q, want trimmed", c.mgmtURL)
	}
}

func TestGetUser(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newTestClient(t, srv.URL)

	u, err := c.GetUser(context.Background(), "tenant-a", "user-1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Email != "ada@acme.com" || u.Name != "Ada Lovelace" {
		t.Errorf("unexpected user: %+v", u)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newTestClient(t, srv.URL)

	_, err := c.GetUser(context.Background(), "tenant-a", "missing")
	if err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetTenant(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newTestClient(t, srv.URL)

	tn, err := c.GetTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if tn.Slug != "acme" {
		t.Errorf("slug = %q, want acme", tn.Slug)
	}
}

// TestTokenCaching asserts the token endpoint is hit exactly once
// across several management calls — the cached token is reused.
func TestTokenCaching(t *testing.T) {
	srv, tokenHits := newTestServer(t)
	c := newTestClient(t, srv.URL)

	for i := 0; i < 3; i++ {
		if _, err := c.GetUser(context.Background(), "tenant-a", "user-1"); err != nil {
			t.Fatalf("GetUser #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(tokenHits); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1 (cache reuse)", got)
	}
}

// TestTokenRefreshOnExpiry asserts that a cached token within the
// refresh skew of expiry triggers a fresh token request.
func TestTokenRefreshOnExpiry(t *testing.T) {
	srv, tokenHits := newTestServer(t)
	c := newTestClient(t, srv.URL)

	if _, err := c.GetUser(context.Background(), "tenant-a", "user-1"); err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	// Force the cached token to be inside the refresh skew window.
	c.mu.Lock()
	c.tokenExpiry = time.Now().Add(tokenRefreshSkew / 2)
	c.mu.Unlock()

	if _, err := c.GetUser(context.Background(), "tenant-a", "user-1"); err != nil {
		t.Fatalf("GetUser after expiry: %v", err)
	}
	if got := atomic.LoadInt32(tokenHits); got != 2 {
		t.Errorf("token endpoint hit %d times, want 2 (refresh near expiry)", got)
	}
}

func TestToken_DefaultsTTLWhenMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	if _, err := c.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	c.mu.Lock()
	exp := c.tokenExpiry
	c.mu.Unlock()
	// Default lifetime is 5 minutes; assert it is in a sane window.
	if d := time.Until(exp); d <= 0 || d > 6*time.Minute {
		t.Errorf("default expiry window = %v, want ~5m", d)
	}
}

func TestRequestToken_PropagatesEndpointError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	if _, err := c.token(context.Background()); err == nil {
		t.Fatal("expected error from non-200 token endpoint")
	}
}

func TestArgValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	c := newTestClient(t, srv.URL)
	ctx := context.Background()
	if _, err := c.GetUser(ctx, "", "u"); err == nil {
		t.Error("GetUser empty tenant: expected error")
	}
	if _, err := c.GetUser(ctx, "t", ""); err == nil {
		t.Error("GetUser empty user: expected error")
	}
	if _, err := c.ListUsers(ctx, ""); err == nil {
		t.Error("ListUsers empty tenant: expected error")
	}
	if _, err := c.GetTenant(ctx, ""); err == nil {
		t.Error("GetTenant empty tenant: expected error")
	}
}
