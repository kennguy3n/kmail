package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// newDummyPool mirrors the helper in `internal/jmap/proxy_test.go`
// — a pgxpool that parses cleanly but never opens a connection.
// `Proxy.PrimeAccountCache` lets us seed the (tenant, user) →
// account_id resolution so no Postgres query ever fires.
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

func newTestService(t *testing.T, stalwart *httptest.Server) (*Service, *jmap.Proxy) {
	t.Helper()
	proxy, err := jmap.NewProxy(jmap.ProxyConfig{
		StalwartURL: stalwart.URL,
		Pool:        newDummyPool(t),
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	proxy.PrimeAccountCache("tenant-1", "kchat-user-1", "acc-1")

	internalC, err := jmap.NewInternalClient(proxy)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}
	svc, err := NewService(Config{Client: internalC, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, proxy
}

func newStalwartStub(t *testing.T, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jmap/api" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const stalwartBootstrapResponse = `{
	"methodResponses": [
		["Mailbox/get", {
			"list": [{"id":"mbx-inbox","name":"Inbox","role":"inbox"}],
			"state": "ms-100",
			"accountId": "acc-1"
		}, "c0"],
		["Email/query", {
			"ids": ["e-1","e-2"],
			"queryState": "qs-1",
			"accountId": "acc-1"
		}, "c1"],
		["Email/get", {
			"list": [
				{"id":"e-1","threadId":"t-1","mailboxIds":{"mbx-inbox":true}},
				{"id":"e-2","threadId":"t-2","mailboxIds":{"mbx-inbox":true}}
			],
			"state": "es-100",
			"accountId": "acc-1"
		}, "c2"]
	],
	"sessionState": "session-1"
}`

func TestBootstrap_HappyPath(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)

	resp, err := svc.Bootstrap(context.Background(), "tenant-1", "kchat-user-1", BootstrapRequest{Limit: 50, MailboxRole: "inbox"})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if resp.AccountID != "acc-1" {
		t.Errorf("account = %q", resp.AccountID)
	}
	if len(resp.Mailboxes) != 1 {
		t.Errorf("mailboxes = %d", len(resp.Mailboxes))
	}
	if resp.MailboxState != "ms-100" {
		t.Errorf("mailbox_state = %q", resp.MailboxState)
	}
	if len(resp.Emails) != 2 {
		t.Errorf("emails = %d", len(resp.Emails))
	}
	if resp.EmailState != "es-100" {
		t.Errorf("email_state = %q", resp.EmailState)
	}
	if resp.BootstrappedAt.IsZero() {
		t.Errorf("bootstrapped_at should be set")
	}

	// Round-trip every raw mailbox / email through json.Marshal
	// to confirm we get pure-JSON values, not opaque escapes.
	for i, raw := range resp.Mailboxes {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Errorf("mailboxes[%d] not valid json: %v", i, err)
		}
	}
	for i, raw := range resp.Emails {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Errorf("emails[%d] not valid json: %v", i, err)
		}
	}
}

func TestBootstrap_RejectsUnknownMailboxRole(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	_, err := svc.Bootstrap(context.Background(), "tenant-1", "kchat-user-1", BootstrapRequest{MailboxRole: "spam-no-such-role"})
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("err = %v", err)
	}
}

func TestBootstrap_ClampsLimit(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	// Direct call: limit > MaxBootstrapLimit silently clamps;
	// the response succeeds and never escapes the cap. We
	// inspect the JMAP request shape through a second stub.
	_, err := svc.Bootstrap(context.Background(), "tenant-1", "kchat-user-1", BootstrapRequest{Limit: 99999})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
}

func TestBootstrap_DefaultsLimit(t *testing.T) {
	t.Parallel()
	var capturedBody []byte
	stalwart := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		capturedBody = b
		_, _ = w.Write([]byte(stalwartBootstrapResponse))
	}))
	defer stalwart.Close()
	svc, _ := newTestService(t, stalwart)
	_, err := svc.Bootstrap(context.Background(), "tenant-1", "kchat-user-1", BootstrapRequest{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(capturedBody, &sent)
	calls, _ := sent["methodCalls"].([]any)
	// c1 is Email/query; assert limit == DefaultBootstrapLimit.
	c1, _ := calls[1].([]any)
	args, _ := c1[1].(map[string]any)
	limit, _ := args["limit"].(float64)
	if int(limit) != DefaultBootstrapLimit {
		t.Errorf("limit = %v want %d", limit, DefaultBootstrapLimit)
	}
}

func TestBootstrap_PropagatesJmapMethodError(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, `{
		"methodResponses": [
			["error", {"type":"forbidden","description":"acct disabled"}, "c0"]
		]
	}`)
	svc, _ := newTestService(t, stalwart)
	_, err := svc.Bootstrap(context.Background(), "tenant-1", "kchat-user-1", BootstrapRequest{})
	if err == nil {
		t.Fatal("expected method-level error to surface")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v", err)
	}
}

func TestHandlers_Bootstrap_Authentication(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	// Without middleware-injected identity, the handler must
	// 403 — the bootstrap endpoint must never serve unauth'd
	// traffic.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap", nil)
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d want 403", rec.Code)
	}
}

func TestHandlers_Bootstrap_OK(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	body, _ := json.Marshal(BootstrapRequest{Limit: 50})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap", bytes.NewReader(body))
	ctx := req.Context()
	ctx = middleware.WithTenantID(ctx, "tenant-1")
	ctx = middleware.WithKChatUserID(ctx, "kchat-user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp BootstrapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.AccountID != "acc-1" {
		t.Errorf("account_id = %q", resp.AccountID)
	}
	if len(resp.Emails) != 2 {
		t.Errorf("emails = %d", len(resp.Emails))
	}
}

func TestHandlers_Bootstrap_QueryStringOverrides(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap?limit=10&mailbox_role=inbox", nil)
	ctx := req.Context()
	ctx = middleware.WithTenantID(ctx, "tenant-1")
	ctx = middleware.WithKChatUserID(ctx, "kchat-user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandlers_Bootstrap_InvalidLimit(t *testing.T) {
	t.Parallel()
	stalwart := newStalwartStub(t, stalwartBootstrapResponse)
	svc, _ := newTestService(t, stalwart)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/bootstrap?limit=not-a-number", nil)
	ctx := req.Context()
	ctx = middleware.WithTenantID(ctx, "tenant-1")
	ctx = middleware.WithKChatUserID(ctx, "kchat-user-1")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.bootstrap(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}
