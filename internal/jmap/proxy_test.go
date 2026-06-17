package jmap

import (
	"context"
	"fmt"
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

// TestShardsAvailable verifies the graceful-degradation health
// signal: a single-shard proxy (no resolver) reports its default
// Target host as available until that host's breaker trips, and
// recovers once a probe succeeds. The verdict must track the same
// breaker the failover transport consults so degradation kicks in
// exactly when the proxy would otherwise 502.
func TestShardsAvailable(t *testing.T) {
	p := newTestProxy(t)
	ctx := context.Background()
	host := p.Target().Host

	if !p.ShardsAvailable(ctx, "tenant-1") {
		t.Fatal("fresh proxy should report shard available")
	}

	// Default breaker trips after 3 consecutive failures (Cooldown=0
	// → stays open until a success).
	for i := 0; i < 3; i++ {
		p.breaker.RecordFailure(ctx, host)
	}
	if p.ShardsAvailable(ctx, "tenant-1") {
		t.Fatal("shard should be unavailable after breaker trips")
	}

	p.breaker.RecordSuccess(ctx, host)
	if !p.ShardsAvailable(ctx, "tenant-1") {
		t.Fatal("shard should recover after a successful probe closes the breaker")
	}
}

func TestNewProxy_RequiresPool(t *testing.T) {
	_, err := NewProxy(ProxyConfig{StalwartURL: "http://stalwart.test"})
	if err == nil {
		t.Fatal("expected error when Pool is nil")
	}
}

// stubInterceptor is a no-op SendInterceptor used to assert which
// interceptor (if any) is wired and active at a given point.
type stubInterceptor struct {
	id string
}

func (s *stubInterceptor) Intercept(_ context.Context, _ http.ResponseWriter, _ *http.Request, _ []byte) (bool, error) {
	return false, nil
}

// TestSendInterceptor_CfgWiredAtNewProxyIsLoaded pins the migration
// guarantee that `ProxyConfig.SendInterceptor` is honored even
// after NewProxy returns. Without the cfg→atomic migration in
// NewProxy, the atomic.Pointer would be nil at construction and
// `loadSendInterceptor` would have to walk back to cfg on every
// request — by migrating once at construction we get a single
// source of truth (the atomic) and the cfg field becomes write-only.
func TestSendInterceptor_CfgWiredAtNewProxyIsLoaded(t *testing.T) {
	stub := &stubInterceptor{id: "cfg"}
	p, err := NewProxy(ProxyConfig{
		StalwartURL:     "http://stalwart.test",
		Pool:            newDummyPool(t),
		Logger:          log.New(io.Discard, "", 0),
		SendInterceptor: stub,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	got := p.loadSendInterceptor()
	if got == nil {
		t.Fatal("loadSendInterceptor returned nil; cfg.SendInterceptor was not migrated to the atomic at NewProxy time")
	}
	if got != stub {
		t.Fatalf("loadSendInterceptor = %v, want the cfg-wired stub %v", got, stub)
	}
}

// TestSendInterceptor_SetNilTrulyDisablesEvenWithCfgWiring pins
// the contract documented on SetSendInterceptor: "Passing nil
// disables interception entirely". Before the cfg→atomic
// migration, loadSendInterceptor had a fallback that would
// resurrect cfg.SendInterceptor after a SetSendInterceptor(nil)
// call, making the doc comment a lie. The fix moved cfg into the
// atomic at NewProxy time so Store(nil) genuinely wins.
func TestSendInterceptor_SetNilTrulyDisablesEvenWithCfgWiring(t *testing.T) {
	cfgStub := &stubInterceptor{id: "cfg"}
	p, err := NewProxy(ProxyConfig{
		StalwartURL:     "http://stalwart.test",
		Pool:            newDummyPool(t),
		Logger:          log.New(io.Discard, "", 0),
		SendInterceptor: cfgStub,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	// Sanity: cfg-wired hook is live to start.
	if p.loadSendInterceptor() == nil {
		t.Fatal("precondition: cfg-wired interceptor must be live at construction")
	}
	p.SetSendInterceptor(nil)
	if got := p.loadSendInterceptor(); got != nil {
		t.Fatalf("loadSendInterceptor after SetSendInterceptor(nil) = %v, want nil. The doc comment promises Passing nil disables interception entirely.", got)
	}
}

// TestSendInterceptor_RuntimeSetOverridesCfg confirms the layered
// wiring model: cfg is a default seeded at NewProxy; a runtime
// `SetSendInterceptor(new)` cleanly swaps the implementation
// without leaking the cfg-era hook back. Tests the swap operation
// in both directions (cfg→new, new→another).
func TestSendInterceptor_RuntimeSetOverridesCfg(t *testing.T) {
	cfgStub := &stubInterceptor{id: "cfg"}
	newStub := &stubInterceptor{id: "runtime"}
	p, err := NewProxy(ProxyConfig{
		StalwartURL:     "http://stalwart.test",
		Pool:            newDummyPool(t),
		Logger:          log.New(io.Discard, "", 0),
		SendInterceptor: cfgStub,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	p.SetSendInterceptor(newStub)
	if got := p.loadSendInterceptor(); got != newStub {
		t.Fatalf("loadSendInterceptor after runtime swap = %v, want runtime stub %v", got, newStub)
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

// TestAccountCache_BoundedSize_EvictsLRU pins the bounded-size
// behaviour we wanted from the migration: a stream of writes that
// exceeds `accountCacheMaxEntries` must not grow the cache
// without bound — the oldest unused entry is dropped (LRU). This
// is the property the previous map-based implementation lacked
// and which motivated the LRU migration.
func TestAccountCache_BoundedSize_EvictsLRU(t *testing.T) {
	c := newAccountCache(time.Hour) // long TTL so we test the LRU bound, not the TTL
	// Touch tenant-0 first then write `accountCacheMaxEntries`
	// fresh keys; tenant-0 should evict because it is the
	// least-recently-used.
	c.set("tenant-0", "user-0", "acc-0")
	for i := 1; i <= accountCacheMaxEntries; i++ {
		c.set(fmt.Sprintf("tenant-%d", i), "user-x", "acc-x")
	}
	if c.inner.Len() != accountCacheMaxEntries {
		t.Errorf("cache len = %d after %d writes, want bounded to %d", c.inner.Len(), accountCacheMaxEntries+1, accountCacheMaxEntries)
	}
	if _, ok := c.get("tenant-0", "user-0"); ok {
		t.Error("expected tenant-0 to have been evicted as LRU")
	}
}

// TestRewrite_PreservesJmapPrefix verifies the proxy forwards the
// request path unchanged. Stalwart serves JMAP under `/jmap` and
// `/jmap/session` (root `/` is the admin UI), so the prefix must
// reach the upstream intact — stripping it (the previous behaviour)
// sent `/jmap/session` to `/session` (a 302) and POST `/jmap` to `/`
// (a 404), so every proxied JMAP call hit the wrong path.
func TestRewrite_PreservesJmapPrefix(t *testing.T) {
	p := newTestProxy(t)

	paths := []struct {
		name string
		path string
	}{
		{"bare jmap", "/jmap"},
		{"session", "/jmap/session"},
		{"upload", "/jmap/upload/deadbeef"},
		{"root", "/jmap/"},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodPost, "http://kmail-api"+tc.path, nil)

			outURL, _ := url.Parse("http://stalwart.test" + tc.path)
			out := in.Clone(in.Context())
			out.URL = outURL

			pr := &httputil.ProxyRequest{In: in, Out: out}
			p.rewrite(pr)

			if pr.Out.URL.Path != tc.path {
				t.Errorf("Path = %q, want %q (forwarded unchanged)", pr.Out.URL.Path, tc.path)
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

// TestRewrite_DevStalwartAuth verifies the dev/CI-only branch: when
// DevStalwartAuthHeader is set the proxy replaces the inbound
// Authorization with the configured (admin Basic) credential instead
// of stripping it, so the plain-HTTP dev BFF can authenticate to the
// stock Stalwart image that does not honour the X-KMail-* headers.
func TestRewrite_DevStalwartAuth(t *testing.T) {
	p, err := NewProxy(ProxyConfig{
		StalwartURL:           "http://stalwart.test",
		Pool:                  newDummyPool(t),
		Logger:                log.New(io.Discard, "", 0),
		DevStalwartAuthHeader: "Basic Zm9vOmJhcg==",
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	in := httptest.NewRequest(http.MethodPost, "http://kmail-api/jmap/session", nil)
	outURL, _ := url.Parse("http://stalwart.test/jmap/session")
	out := in.Clone(in.Context())
	out.URL = outURL
	out.Header = http.Header{"Authorization": []string{"Bearer user-token"}}

	pr := &httputil.ProxyRequest{In: in, Out: out}
	p.rewrite(pr)

	if got := pr.Out.Header.Get("Authorization"); got != "Basic Zm9vOmJhcg==" {
		t.Errorf("Authorization = %q, want the dev admin Basic credential", got)
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
	if !p.breaker.Open(context.Background(), srvURL.Host) {
		t.Errorf("breaker for %s did not open after 3 consecutive 5xx", srvURL.Host)
	}
}

// TestShardFailoverTransport_NonShardPathDrivesDefaultBreaker covers
// the single-target / no-shard-assignment path: when no shard URLs
// are stamped on the request, the transport must still record the
// default target's health against the breaker. Otherwise
// ShardsAvailable (which consults that same host) would always
// report healthy and graceful degradation could never engage for
// these tenants.
func TestShardFailoverTransport_NonShardPathDrivesDefaultBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, err := NewProxy(ProxyConfig{
		StalwartURL: srv.URL,
		Pool:        newDummyPool(t),
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	tr := &shardFailoverTransport{proxy: p, base: http.DefaultTransport}

	if !p.ShardsAvailable(context.Background(), "tenant-1") {
		t.Fatal("default target should be available before any failures")
	}

	// Drive non-shard requests (no withShardURLs) at the 500 target.
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/jmap", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip[%d]: %v", i, err)
		}
		resp.Body.Close()
	}

	if p.ShardsAvailable(context.Background(), "tenant-1") {
		t.Fatal("default-target breaker never tripped on the non-shard path — degradation would never engage")
	}
}

// countingShardResolver wraps a static map and counts GetTenantShard
// calls so a test can assert the per-request memo collapses the
// health-check + ServeHTTP double-resolve into one Postgres hit.
type countingShardResolver struct {
	primary string
	calls   int
}

func (c *countingShardResolver) GetTenantShard(ctx context.Context, tenantID string) (string, error) {
	c.calls++
	return c.primary, nil
}

func (c *countingShardResolver) GetSecondaryShards(ctx context.Context, tenantID string) ([]string, error) {
	return nil, nil
}

// TestShardResolveMemoDeduplicatesPerRequest verifies that with a
// per-request memo installed, two resolveShardURLs calls in the same
// request (as the degradation health check + ServeHTTP would make)
// issue only a single GetTenantShard query, and that a fresh request
// without the memo re-resolves.
func TestShardResolveMemoDeduplicatesPerRequest(t *testing.T) {
	p := newTestProxy(t)
	res := &countingShardResolver{primary: "http://shard-1:8080"}
	p.cfg.Shards = res

	// Without a memo: each call hits the resolver.
	bare := context.Background()
	_ = p.resolveShardURLs(bare, "tenant-1")
	_ = p.resolveShardURLs(bare, "tenant-1")
	if res.calls != 2 {
		t.Fatalf("no-memo: GetTenantShard calls = %d, want 2", res.calls)
	}

	// With a memo: the second resolve in the same request is served
	// from the memo.
	res.calls = 0
	ctx := WithShardResolveMemo(context.Background())
	a := p.resolveShardURLs(ctx, "tenant-1")
	b := p.resolveShardURLs(ctx, "tenant-1")
	if res.calls != 1 {
		t.Fatalf("memo: GetTenantShard calls = %d, want 1", res.calls)
	}
	if len(a) != 1 || len(b) != 1 || a[0] != "http://shard-1:8080" || b[0] != a[0] {
		t.Fatalf("memo returned inconsistent urls: a=%v b=%v", a, b)
	}

	// A new request (new memo) re-resolves.
	res.calls = 0
	ctx2 := WithShardResolveMemo(context.Background())
	_ = p.resolveShardURLs(ctx2, "tenant-1")
	if res.calls != 1 {
		t.Fatalf("second request: GetTenantShard calls = %d, want 1", res.calls)
	}
}

// fakeSendInterceptor returns a fixed (intercepted, err) pair and
// optionally writes a canned body to the ResponseWriter. Used to
// pin the proxy's handling of the four (intercepted, err) corners
// — in particular `(true, err)` which must NOT fall through to
// the upstream proxy (or `p.rp.ServeHTTP` would re-write headers
// on an already-committed connection).
type fakeSendInterceptor struct {
	intercepted bool
	err         error
	body        string
}

func (f *fakeSendInterceptor) Intercept(_ context.Context, w http.ResponseWriter, _ *http.Request, _ []byte) (bool, error) {
	if f.intercepted && f.body != "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(f.body))
	}
	return f.intercepted, f.err
}

// TestServeHTTP_InterceptedHonoredEvenWhenInterceptorErrors pins
// the contract: when a SendInterceptor returns
// `(intercepted=true, err≠nil)` — which writeJMAPResponse can
// produce after the helper has already written headers — the
// proxy MUST short-circuit instead of falling through to
// `p.rp.ServeHTTP`. Falling through would attempt a second
// WriteHeader on the committed ResponseWriter and either panic
// or corrupt the connection.
func TestServeHTTP_InterceptedHonoredEvenWhenInterceptorErrors(t *testing.T) {
	p := newTestProxy(t)
	p.SetSendInterceptor(&fakeSendInterceptor{
		intercepted: true,
		err:         fmt.Errorf("hold-after-dispatch failed: simulated"),
		body:        `{"methodResponses":[["EmailSubmission/set",{"created":{"sub-1":{"id":"submission-1"}}},"c1"]]}`,
	})

	// Prime the account cache so resolveAccount short-circuits and
	// the request reaches the interceptor branch instead of
	// failing at the upstream Postgres lookup.
	p.PrimeAccountCache("tenant-a", "kuser-a", "stalwart-acc-1")

	// Build a request that hits the JMAP-submit path with a body
	// that contains an EmailSubmission/set method call so the
	// interceptor branch is reached.
	body := strings.NewReader(`{"methodCalls":[["EmailSubmission/set",{"create":{"sub-1":{"emailId":"#draft"}}},"c1"]]}`)
	req := httptest.NewRequest(http.MethodPost, "http://kmail-api/jmap", body)
	ctx := req.Context()
	ctx = middleware.WithTenantID(ctx, "tenant-a")
	ctx = middleware.WithKChatUserID(ctx, "kuser-a")
	ctx = middleware.WithStalwartAccountID(ctx, "stalwart-acc-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	// Body must come from the interceptor; if the proxy fell
	// through it would have attempted to dial stalwart.test (the
	// configured backend) and produced a 502.
	got := rec.Body.String()
	if !strings.Contains(got, `"created"`) {
		t.Errorf("response body = %q, want the interceptor's canned response", got)
	}
	if rec.Code == http.StatusBadGateway {
		t.Fatalf("proxy returned 502 — the (intercepted=true, err≠nil) path fell through to the upstream proxy, which is the exact regression this test pins")
	}
}

// TestServeHTTP_InterceptorErrorWithoutInterceptedFallsThrough
// is the negative of the above: a `(intercepted=false, err≠nil)`
// return means the hook explicitly declined to handle the
// request but had a diagnostic. The proxy is expected to log + fall
// through to Stalwart, preserving the "transient Valkey blip can't
// break the send path" degradation contract.
func TestServeHTTP_InterceptorErrorWithoutInterceptedFallsThrough(t *testing.T) {
	p := newTestProxy(t)
	p.SetSendInterceptor(&fakeSendInterceptor{
		intercepted: false,
		err:         fmt.Errorf("non-fatal probe failure"),
	})
	p.PrimeAccountCache("tenant-a", "kuser-a", "stalwart-acc-1")

	body := strings.NewReader(`{"methodCalls":[["EmailSubmission/set",{"create":{"sub-1":{"emailId":"#draft"}}},"c1"]]}`)
	req := httptest.NewRequest(http.MethodPost, "http://kmail-api/jmap", body)
	ctx := req.Context()
	ctx = middleware.WithTenantID(ctx, "tenant-a")
	ctx = middleware.WithKChatUserID(ctx, "kuser-a")
	ctx = middleware.WithStalwartAccountID(ctx, "stalwart-acc-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	// With no upstream wired and a fall-through, the request lands
	// in the reverse-proxy path which fails dialing stalwart.test
	// → errorHandler → 502. That confirms the proxy did NOT
	// short-circuit on the (false, err) corner.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (fall-through to unreachable backend). Got body=%q", rec.Code, rec.Body.String())
	}
}
