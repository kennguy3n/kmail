package jmap

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// newDummyPool creates a pgxpool.Pool that parses successfully but
// is never connected to. NewProxy only stores it; tests that avoid
// Proxy.resolveAccount never acquire a connection.
func newDummyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgresql://test:test@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p, err := NewProxy(ProxyConfig{
		StalwartURL: "http://stalwart.test",
		Pool:        newDummyPool(t),
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	return p
}

func TestNewProxy_RequiresStalwartURL(t *testing.T) {
	_, err := NewProxy(ProxyConfig{Pool: newDummyPool(t)})
	if err == nil {
		t.Fatal("expected error when StalwartURL is empty")
	}
}

func TestNewProxy_RequiresPool(t *testing.T) {
	_, err := NewProxy(ProxyConfig{StalwartURL: "http://stalwart.test"})
	if err == nil {
		t.Fatal("expected error when Pool is nil")
	}
}

func TestAccountCache_SetGet(t *testing.T) {
	c := newAccountCache(time.Minute)
	if _, ok := c.get("t", "u"); ok {
		t.Fatal("expected miss on empty cache")
	}
	c.set("t", "u", "acc-1")
	got, ok := c.get("t", "u")
	if !ok || got != "acc-1" {
		t.Fatalf("get = (%q,%v), want (acc-1,true)", got, ok)
	}
}

func TestAccountCache_KeysAreNamespacedByTenant(t *testing.T) {
	c := newAccountCache(time.Minute)
	c.set("t1", "u", "acc-1")
	c.set("t2", "u", "acc-2")

	got1, _ := c.get("t1", "u")
	got2, _ := c.get("t2", "u")
	if got1 != "acc-1" || got2 != "acc-2" {
		t.Errorf("cross-tenant collision: t1=%q t2=%q", got1, got2)
	}
}

func TestAccountCache_Expiry(t *testing.T) {
	c := newAccountCache(10 * time.Millisecond)
	c.set("t", "u", "acc-1")
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.get("t", "u"); ok {
		t.Error("expected expired entry to be reported as miss")
	}
}

func TestAccountCache_EvictsExpiredEntryOnGet(t *testing.T) {
	c := newAccountCache(10 * time.Millisecond)
	c.set("t", "u", "acc-1")
	time.Sleep(20 * time.Millisecond)
	_, _ = c.get("t", "u") // triggers eviction
	c.mu.RLock()
	_, stillPresent := c.m[c.key("t", "u")]
	c.mu.RUnlock()
	if stillPresent {
		t.Error("expected expired entry to be evicted from the map")
	}
}

// TestRewrite_StripsJmapPrefix verifies the proxy rewrite strips the
// `/jmap` prefix so Stalwart sees the path it actually implements,
// and clears RawPath so net/url regenerates it from Path.
func TestRewrite_StripsJmapPrefix(t *testing.T) {
	p := newTestProxy(t)

	tests := []struct {
		name    string
		inPath  string
		outPath string
	}{
		{"bare jmap", "/jmap", "/"},
		{"session", "/jmap/session", "/session"},
		{"upload", "/jmap/upload/deadbeef", "/upload/deadbeef"},
		{"root", "/jmap/", "/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodPost, "http://kmail-api"+tc.inPath, nil)
			// Force RawPath to a non-empty value to ensure the rewrite
			// clears it — otherwise RequestURI would still emit the
			// original prefixed path.
			in.URL.RawPath = tc.inPath

			outURL, _ := url.Parse("http://stalwart.test" + tc.inPath)
			out := in.Clone(in.Context())
			out.URL = outURL

			pr := &httputil.ProxyRequest{In: in, Out: out}
			p.rewrite(pr)

			if pr.Out.URL.Path != tc.outPath {
				t.Errorf("Path = %q, want %q", pr.Out.URL.Path, tc.outPath)
			}
			if pr.Out.URL.RawPath != "" {
				t.Errorf("RawPath = %q, want empty", pr.Out.URL.RawPath)
			}
		})
	}
}

func TestRewrite_InjectsHeaders(t *testing.T) {
	p := newTestProxy(t)

	in := httptest.NewRequest(http.MethodPost, "http://kmail-api/jmap/session", nil)
	ctx := middleware.WithStalwartAccountID(in.Context(), "stalwart-acc-1")
	// Populate tenant + kchat_user via the auth middleware path.
	// Rewrite reads them from the context; we simulate that by
	// calling Wrap-like logic: we cannot reach the unexported
	// keys, so we rely on WithStalwartAccountID only (the other
	// two headers will be empty strings, which is fine for the
	// header-presence assertion).
	in = in.WithContext(ctx)

	outURL, _ := url.Parse("http://stalwart.test/jmap/session")
	out := in.Clone(in.Context())
	out.URL = outURL
	out.Header = http.Header{"Authorization": []string{"Bearer leak-me"}}

	pr := &httputil.ProxyRequest{In: in, Out: out}
	p.rewrite(pr)

	if got := pr.Out.Header.Get("X-KMail-Stalwart-Account-Id"); got != "stalwart-acc-1" {
		t.Errorf("X-KMail-Stalwart-Account-Id = %q, want stalwart-acc-1", got)
	}
	if got := pr.Out.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization should be stripped, got %q", got)
	}
	if pr.Out.Host != "stalwart.test" {
		t.Errorf("Out.Host = %q, want stalwart.test", pr.Out.Host)
	}
}

// TestServeHTTP_MissingContext: when the OIDC middleware has not
// run, ServeHTTP must return 500 (caller wired the mux wrong).
func TestServeHTTP_MissingContext(t *testing.T) {
	p := newTestProxy(t)

	req := httptest.NewRequest(http.MethodPost, "http://kmail-api/jmap", nil)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "missing tenant or user context") {
		t.Errorf("body = %q, want mention of missing context", body)
	}
}

// TestErrorHandler verifies upstream failures surface as 502 with
// a JMAP-shaped error body.
func TestErrorHandler(t *testing.T) {
	p := newTestProxy(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://kmail-api/jmap", nil)
	p.errorHandler(rec, req, http.ErrHandlerTimeout)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(rec.Body.String(), "serverUnavailable") {
		t.Errorf("body = %q, want serverUnavailable", rec.Body.String())
	}
}

// TestShardFailoverTransport_BufersBodyAcrossRetries verifies a
// 5xx response from the primary shard does not consume the request
// body for the secondary attempt. Without buffering the second
// shard would receive an empty payload and reject the request.
func TestShardFailoverTransport_BuffersBodyAcrossRetries(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer primary.Close()

	var got string
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondary.Close()

	p := newTestProxy(t)
	tr := &shardFailoverTransport{proxy: p, base: http.DefaultTransport}
	body := strings.NewReader(`{"using":["urn:ietf:params:jmap:core"]}`)
	req, err := http.NewRequest(http.MethodPost, "http://placeholder/jmap", body)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withShardURLs(req.Context(), []string{primary.URL, secondary.URL})
	req = req.WithContext(ctx)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got != `{"using":["urn:ietf:params:jmap:core"]}` {
		t.Errorf("secondary body = %q, want full payload", got)
	}
}

// TestShardFailoverTransport_LastShardBreaker verifies that a 5xx
// from the last candidate URL still increments the circuit breaker
// for that host instead of falling through to breakerReset. The old
// code reset the counter on every last-shard 5xx, so the breaker
// could never trip for the only remaining shard.
func TestShardFailoverTransport_LastShardBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestProxy(t)
	tr := &shardFailoverTransport{proxy: p, base: http.DefaultTransport}

	// One candidate (the last == only shard). Each request should
	// increment the breaker counter and on the threshold open it.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://placeholder/jmap", nil)
		req = req.WithContext(withShardURLs(req.Context(), []string{srv.URL}))
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip[%d]: %v", i, err)
		}
		resp.Body.Close()
	}

	srvURL, _ := url.Parse(srv.URL)
	if !p.breakerOpen(srvURL.Host, 3) {
		t.Errorf("breaker for %s did not open after 3 consecutive 5xx", srvURL.Host)
	}
}
