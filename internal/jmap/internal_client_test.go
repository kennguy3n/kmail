package jmap

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInternalClient_Dispatch_HappyPath(t *testing.T) {
	t.Parallel()

	var capturedHeaders http.Header
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jmap/api" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		capturedHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		capturedBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"methodResponses": [
				["Mailbox/get", {"list": [{"id": "mbx-1"}], "state": "ms-1"}, "c0"]
			],
			"sessionState": "session-1"
		}`))
	}))
	defer srv.Close()

	p := newTestProxy(t)
	// Point the proxy at our httptest server. Tests construct
	// the URL directly via `joinPath` so we don't need the proxy
	// reverse-proxy machinery here.
	pTarget := p.target
	pTarget.Scheme = "http"
	pTarget.Host = srv.Listener.Addr().String()
	p.target = pTarget

	p.PrimeAccountCache("t1", "u1", "acc-1")

	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}

	resp, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		Using: []string{"urn:ietf:params:jmap:core"},
		MethodCalls: [][]any{
			{"Mailbox/get", map[string]any{"accountId": "acc-1"}, "c0"},
		},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if capturedHeaders.Get("X-KMail-Tenant-Id") != "t1" {
		t.Errorf("tenant header = %q", capturedHeaders.Get("X-KMail-Tenant-Id"))
	}
	if capturedHeaders.Get("X-KMail-Stalwart-Account-Id") != "acc-1" {
		t.Errorf("account header = %q", capturedHeaders.Get("X-KMail-Stalwart-Account-Id"))
	}

	var sent map[string]any
	if err := json.Unmarshal(capturedBody, &sent); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := sent["methodCalls"]; !ok {
		t.Errorf("body missing methodCalls: %s", capturedBody)
	}

	name, args, ok := resp.CallByID("c0")
	if !ok {
		t.Fatalf("response missing c0")
	}
	if name != "Mailbox/get" {
		t.Errorf("name = %q", name)
	}
	list, _ := args["list"].([]any)
	if len(list) != 1 {
		t.Errorf("list len = %d", len(list))
	}
	if state, _ := args["state"].(string); state != "ms-1" {
		t.Errorf("state = %q", state)
	}
}

func TestInternalClient_Dispatch_MethodLevelError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"methodResponses": [
				["error", {"type": "accountNotFound", "description": "no such acc"}, "c0"]
			]
		}`))
	}))
	defer srv.Close()

	p := newTestProxy(t)
	p.target.Host = srv.Listener.Addr().String()
	p.target.Scheme = "http"
	p.PrimeAccountCache("t1", "u1", "acc-1")

	c, _ := NewInternalClient(p)
	_, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	})
	if err == nil {
		t.Fatal("expected method-level error to surface")
	}
	if !strings.Contains(err.Error(), "accountNotFound") {
		t.Errorf("err = %v want accountNotFound", err)
	}
}

func TestInternalClient_Dispatch_5xxFailsOver(t *testing.T) {
	t.Parallel()

	var primaryHits, secondaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondaryHits, 1)
		_, _ = w.Write([]byte(`{"methodResponses":[]}`))
	}))
	defer secondary.Close()

	p := newTestProxy(t)
	p.target.Scheme = "http"
	p.target.Host = primary.Listener.Addr().String()
	// Inject a static shard resolver returning both URLs.
	p.cfg.Shards = staticShardResolver{
		"t1": {"http://" + primary.Listener.Addr().String(), "http://" + secondary.Listener.Addr().String()},
	}
	p.PrimeAccountCache("t1", "u1", "acc-1")

	c, _ := NewInternalClient(p)
	resp, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if atomic.LoadInt32(&primaryHits) != 1 {
		t.Errorf("primary hits = %d want 1", primaryHits)
	}
	if atomic.LoadInt32(&secondaryHits) != 1 {
		t.Errorf("secondary hits = %d want 1", secondaryHits)
	}
}

func TestInternalClient_Dispatch_4xxDoesNotFailOver(t *testing.T) {
	t.Parallel()

	var primaryHits, secondaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		http.Error(w, "bad req", http.StatusBadRequest)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondaryHits, 1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer secondary.Close()

	p := newTestProxy(t)
	p.cfg.Shards = staticShardResolver{
		"t1": {"http://" + primary.Listener.Addr().String(), "http://" + secondary.Listener.Addr().String()},
	}
	p.PrimeAccountCache("t1", "u1", "acc-1")

	c, _ := NewInternalClient(p)
	_, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	})
	if err == nil {
		t.Fatal("expected 4xx to surface immediately")
	}
	if atomic.LoadInt32(&secondaryHits) != 0 {
		t.Errorf("4xx must not fail over; secondary hits = %d", secondaryHits)
	}
	if atomic.LoadInt32(&primaryHits) != 1 {
		t.Errorf("primary hits = %d want 1", primaryHits)
	}
}

func TestInternalClient_DownloadBlob_HappyPath(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		_, _ = w.Write([]byte("RAW-RFC5322-BYTES"))
	}))
	defer srv.Close()

	p := newTestProxy(t)
	p.target.Scheme = "http"
	p.target.Host = srv.Listener.Addr().String()
	c, _ := NewInternalClient(p)

	body, err := c.DownloadBlob(context.Background(), "t1", "u1", "acc-1", "blob-9", "message.eml")
	if err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	if string(body) != "RAW-RFC5322-BYTES" {
		t.Errorf("body = %q", body)
	}
	if gotPath != "/jmap/download/acc-1/blob-9/message.eml" {
		t.Errorf("path = %q", gotPath)
	}
	if gotHeaders.Get("X-KMail-Stalwart-Account-Id") != "acc-1" {
		t.Errorf("account header = %q", gotHeaders.Get("X-KMail-Stalwart-Account-Id"))
	}
}

func TestInternalClient_DownloadBlob_OversizeErrors(t *testing.T) {
	t.Parallel()

	// Server returns more bytes than the (test-shrunk) cap. The
	// client must report an error rather than silently returning the
	// truncated prefix — a truncated export artifact is corrupt data.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789ABCDEF")) // 16 bytes
	}))
	defer srv.Close()

	p := newTestProxy(t)
	p.target.Scheme = "http"
	p.target.Host = srv.Listener.Addr().String()
	c, _ := NewInternalClient(p)
	c.maxBlobBytes = 8

	body, err := c.DownloadBlob(context.Background(), "t1", "u1", "acc-1", "blob-9", "big.bin")
	if err == nil {
		t.Fatalf("expected oversize error, got body len=%d", len(body))
	}
	if body != nil {
		t.Errorf("oversize must not return truncated bytes; got %q", body)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want it to mention the byte limit", err)
	}
}

func TestInternalClient_DownloadBlob_ExactLimitOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("01234567")) // exactly 8 bytes
	}))
	defer srv.Close()

	p := newTestProxy(t)
	p.target.Scheme = "http"
	p.target.Host = srv.Listener.Addr().String()
	c, _ := NewInternalClient(p)
	c.maxBlobBytes = 8

	body, err := c.DownloadBlob(context.Background(), "t1", "u1", "acc-1", "blob-9", "exact.bin")
	if err != nil {
		t.Fatalf("a blob exactly at the limit must succeed: %v", err)
	}
	if string(body) != "01234567" {
		t.Errorf("body = %q", body)
	}
}

func TestInternalClient_Dispatch_OversizeErrors(t *testing.T) {
	t.Parallel()

	// A valid-but-oversized JSON envelope must fail explicitly rather
	// than relying on json.Unmarshal to incidentally catch a truncated
	// body.
	big := `{"methodResponses":[["Mailbox/get",{"x":"` + strings.Repeat("a", 64) + `"},"c0"]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	p := newTestProxy(t)
	p.target.Scheme = "http"
	p.target.Host = srv.Listener.Addr().String()
	p.PrimeAccountCache("t1", "u1", "acc-1")
	c, _ := NewInternalClient(p)
	c.maxResponseBytes = 16

	_, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	})
	if err == nil {
		t.Fatal("expected oversize response error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want it to mention the byte limit", err)
	}
}

// TestInternalClient_DevAuthHeader verifies the dev/CI-only branch:
// when the backing proxy carries a DevStalwartAuthHeader,
// BFF-initiated calls (Dispatch + DownloadBlob) stamp that
// Authorization value so they authenticate to the stock Stalwart
// image instead of 401-ing; with no dev header (the production
// posture) no Authorization is sent and the request relies on the
// shared mTLS transport.
func TestInternalClient_DevAuthHeader(t *testing.T) {
	t.Parallel()

	const devHeader = "Basic Zm9vOmJhcg==" // foo:bar

	for _, tc := range []struct {
		name      string
		devHeader string
		wantAuth  string
	}{
		{name: "dev sets admin Basic", devHeader: devHeader, wantAuth: devHeader},
		{name: "prod sends none", devHeader: "", wantAuth: ""},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var dispatchAuth, downloadAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/jmap/api" {
					dispatchAuth = r.Header.Get("Authorization")
					_, _ = w.Write([]byte(`{"methodResponses":[]}`))
					return
				}
				downloadAuth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte("BLOB"))
			}))
			defer srv.Close()

			p, err := NewProxy(ProxyConfig{
				StalwartURL:           srv.URL,
				Pool:                  newDummyPool(t),
				Logger:                log.New(io.Discard, "", 0),
				DevStalwartAuthHeader: tc.devHeader,
			})
			if err != nil {
				t.Fatalf("NewProxy: %v", err)
			}
			p.PrimeAccountCache("t1", "u1", "acc-1")
			c, err := NewInternalClient(p)
			if err != nil {
				t.Fatalf("NewInternalClient: %v", err)
			}

			if _, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
				MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
			}); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if dispatchAuth != tc.wantAuth {
				t.Errorf("Dispatch Authorization = %q want %q", dispatchAuth, tc.wantAuth)
			}

			if _, err := c.DownloadBlob(context.Background(), "t1", "u1", "acc-1", "blob-1", "m.eml"); err != nil {
				t.Fatalf("DownloadBlob: %v", err)
			}
			if downloadAuth != tc.wantAuth {
				t.Errorf("DownloadBlob Authorization = %q want %q", downloadAuth, tc.wantAuth)
			}
		})
	}
}

// TestInternalClient_OIDCBearer_Mints verifies the production path:
// with a Minter configured and no dev header, BFF-initiated calls
// (Dispatch + DownloadBlob) mint a `stalwart`-audience bearer for
// the resolved account and forward it, exactly as the proxy does —
// so they authenticate to the official OIDC image instead of 401-ing
// on the X-KMail-* headers it does not honor.
func TestInternalClient_OIDCBearer_Mints(t *testing.T) {
	t.Parallel()

	var dispatchAuth, downloadAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jmap/api" {
			dispatchAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"methodResponses":[]}`))
			return
		}
		downloadAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("BLOB"))
	}))
	defer srv.Close()

	m := &fakeMinter{}
	p, err := NewProxy(ProxyConfig{
		StalwartURL: srv.URL,
		Pool:        newDummyPool(t),
		Logger:      log.New(io.Discard, "", 0),
		Minter:      m,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	p.PrimeAccountCache("t1", "u1", "acc-1")
	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}

	if _, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if want := "Bearer tok-for-acc-1"; dispatchAuth != want {
		t.Errorf("Dispatch Authorization = %q want %q", dispatchAuth, want)
	}
	if m.last != "acc-1" {
		t.Errorf("minted for principal %q, want acc-1", m.last)
	}

	if _, err := c.DownloadBlob(context.Background(), "t1", "u1", "acc-1", "blob-1", "m.eml"); err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	if want := "Bearer tok-for-acc-1"; downloadAuth != want {
		t.Errorf("DownloadBlob Authorization = %q want %q", downloadAuth, want)
	}
}

// TestInternalClient_DevHeaderBeatsMinter verifies the dev/CI
// admin-Basic path takes precedence when both a dev header and a
// Minter are set, so dev never accidentally mints tokens — mirroring
// the proxy's TestRewrite_DevHeaderBeatsMinter.
func TestInternalClient_DevHeaderBeatsMinter(t *testing.T) {
	t.Parallel()

	const devHeader = "Basic Zm9vOmJhcg==" // foo:bar
	var dispatchAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatchAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"methodResponses":[]}`))
	}))
	defer srv.Close()

	m := &fakeMinter{}
	p, err := NewProxy(ProxyConfig{
		StalwartURL:           srv.URL,
		Pool:                  newDummyPool(t),
		Logger:                log.New(io.Discard, "", 0),
		DevStalwartAuthHeader: devHeader,
		Minter:                m,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	p.PrimeAccountCache("t1", "u1", "acc-1")
	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}

	if _, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if dispatchAuth != devHeader {
		t.Errorf("Dispatch Authorization = %q want dev header %q", dispatchAuth, devHeader)
	}
	if m.last != "" {
		t.Errorf("minter was called (principal=%q) but dev header should take precedence", m.last)
	}
}

// TestInternalClient_OIDCBearer_FailsClosedOnMintError verifies a
// mint failure fails closed: Dispatch and DownloadBlob return an
// error rather than dispatching an unauthenticated request that
// would act as the wrong (or an admin) principal.
func TestInternalClient_OIDCBearer_FailsClosedOnMintError(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"methodResponses":[]}`))
	}))
	defer srv.Close()

	p, err := NewProxy(ProxyConfig{
		StalwartURL: srv.URL,
		Pool:        newDummyPool(t),
		Logger:      log.New(io.Discard, "", 0),
		Minter:      &fakeMinter{err: errTest},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	p.PrimeAccountCache("t1", "u1", "acc-1")
	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}

	if _, err := c.Dispatch(context.Background(), "t1", "u1", JmapRequest{
		MethodCalls: [][]any{{"Mailbox/get", map[string]any{}, "c0"}},
	}); err == nil {
		t.Error("Dispatch: expected error on mint failure, got nil")
	}
	if _, err := c.DownloadBlob(context.Background(), "t1", "u1", "acc-1", "blob-1", "m.eml"); err == nil {
		t.Error("DownloadBlob: expected error on mint failure, got nil")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0 (must fail closed before dispatch)", n)
	}
}

func TestInternalClient_RequiresProxy(t *testing.T) {
	t.Parallel()
	if _, err := NewInternalClient(nil); err == nil {
		t.Fatal("expected error for nil proxy")
	}
}

func TestJoinPath_NoDoubleSlash(t *testing.T) {
	t.Parallel()
	cases := []struct{ base, rel, want string }{
		{"http://a.b", "/jmap/api", "http://a.b/jmap/api"},
		{"http://a.b/", "/jmap/api", "http://a.b/jmap/api"},
		{"http://a.b/prefix", "/jmap/api", "http://a.b/prefix/jmap/api"},
		{"http://a.b/prefix/", "/jmap/api", "http://a.b/prefix/jmap/api"},
		{"http://a.b", "jmap/api", "http://a.b/jmap/api"},
	}
	for _, tc := range cases {
		got, err := joinPath(tc.base, tc.rel)
		if err != nil {
			t.Errorf("joinPath(%q, %q): %v", tc.base, tc.rel, err)
			continue
		}
		if got != tc.want {
			t.Errorf("joinPath(%q, %q) = %q want %q", tc.base, tc.rel, got, tc.want)
		}
	}
}

// staticShardResolver — minimal ShardResolver for tests. The
// underlying map's first entry is the primary; subsequent
// entries are secondaries (in slice order).
type staticShardResolver map[string][]string

func (s staticShardResolver) GetTenantShard(ctx context.Context, tenantID string) (string, error) {
	if v, ok := s[tenantID]; ok && len(v) > 0 {
		return v[0], nil
	}
	return "", nil
}

func (s staticShardResolver) GetSecondaryShards(ctx context.Context, tenantID string) ([]string, error) {
	if v, ok := s[tenantID]; ok && len(v) > 1 {
		return v[1:], nil
	}
	return nil, nil
}
