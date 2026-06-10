package chatbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// dbService builds a chatbridge Service over a live Postgres pool
// with a recording KChat client, plus a seeded tenant.
func dbService(t *testing.T, kc KChatClient) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	if kc == nil {
		kc = &recordKChat{}
	}
	svc := NewService(Config{Pool: pool, KChat: kc, Logger: log.New(io.Discard, "", 0)})
	return svc, tenant
}

// TestConfigureListDeleteRouteDB drives route upsert → list →
// delete (and channel rotation on conflict) against Postgres.
func TestConfigureListDeleteRouteDB(t *testing.T) {
	svc, tenant := dbService(t, nil)
	ctx := context.Background()

	route, err := svc.ConfigureAlertRoute(ctx, tenant, "Alerts@x.test", "chan-1")
	if err != nil {
		t.Fatalf("ConfigureAlertRoute: %v", err)
	}
	if route.ID == "" || route.AliasAddress != "alerts@x.test" {
		t.Fatalf("unexpected route: %+v", route)
	}

	// Re-configuring the same alias rotates the channel (ON CONFLICT).
	rotated, err := svc.ConfigureAlertRoute(ctx, tenant, "alerts@x.test", "chan-2")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.ID != route.ID || rotated.ChannelID != "chan-2" {
		t.Errorf("expected same row with rotated channel, got %+v", rotated)
	}

	// List → exactly one route.
	routes, err := svc.ListRoutes(ctx, tenant)
	if err != nil {
		t.Fatalf("ListRoutes: %v", err)
	}
	if len(routes) != 1 || routes[0].ChannelID != "chan-2" {
		t.Fatalf("list = %+v", routes)
	}

	// Delete unknown → ErrNotFound.
	if err := svc.DeleteRoute(ctx, tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown: want ErrNotFound got %v", err)
	}
	// Delete the real route → ok, then list empty.
	if err := svc.DeleteRoute(ctx, tenant, route.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	routes, _ = svc.ListRoutes(ctx, tenant)
	if len(routes) != 0 {
		t.Errorf("expected empty after delete, got %d", len(routes))
	}
}

// TestProcessInboundAlertRoutesDB verifies the lookupRoute → post
// path: an inbound alert on a configured alias posts to the mapped
// channel.
func TestProcessInboundAlertRoutesDB(t *testing.T) {
	kc := &recordKChat{}
	svc, tenant := dbService(t, kc)
	ctx := context.Background()

	if _, err := svc.ConfigureAlertRoute(ctx, tenant, "ops@x.test", "chan-ops"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	if err := svc.ProcessInboundAlert(ctx, tenant, "OPS@x.test", "email-1"); err != nil {
		t.Fatalf("ProcessInboundAlert: %v", err)
	}
	if len(kc.posts) != 1 || kc.posts[0].channelID != "chan-ops" {
		t.Fatalf("expected post to chan-ops, got %+v", kc.posts)
	}

	// Validation guard.
	if err := svc.ProcessInboundAlert(ctx, "", "a", "b"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput got %v", err)
	}
}

// --- HTTP handler coverage via dev-bypass OIDC ---

func chatHarness(t *testing.T, svc *Service) (*http.ServeMux, string) {
	t.Helper()
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	NewHandlers(svc, log.New(io.Discard, "", 0)).Register(mux, authMW)
	return mux, "dev-token"
}

func chatReq(t *testing.T, mux *http.ServeMux, tenant, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "alice")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandlersRouteCRUDHTTP(t *testing.T) {
	kc := &recordKChat{}
	svc, tenant := dbService(t, kc)
	mux, _ := chatHarness(t, svc)

	// create route → 201.
	rec := chatReq(t, mux, tenant, http.MethodPost, "/api/v1/chat-bridge/routes", createRouteRequest{
		AliasAddress: "team@x.test", ChannelID: "chan-team",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create route: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var route Route
	_ = json.Unmarshal(rec.Body.Bytes(), &route)
	if route.ID == "" {
		t.Fatalf("no route id: %s", rec.Body.String())
	}

	// list routes → 200.
	rec = chatReq(t, mux, tenant, http.MethodGet, "/api/v1/chat-bridge/routes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d", rec.Code)
	}
	var listResp struct {
		Routes []Route `json:"routes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Routes) != 1 {
		t.Fatalf("list returned %d", len(listResp.Routes))
	}

	// create route bad JSON → 400.
	rec = chatReq(t, mux, tenant, http.MethodPost, "/api/v1/chat-bridge/routes", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat-bridge/routes", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "alice")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: code=%d want 400", rec.Code)
	}

	// create route missing fields → 400 (ErrInvalidInput).
	rec = chatReq(t, mux, tenant, http.MethodPost, "/api/v1/chat-bridge/routes", createRouteRequest{AliasAddress: "x@y"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing channel: code=%d want 400", rec.Code)
	}

	// delete route → 204.
	rec = chatReq(t, mux, tenant, http.MethodDelete, "/api/v1/chat-bridge/routes/"+route.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d", rec.Code)
	}
	// delete again → 404 (ErrNotFound).
	rec = chatReq(t, mux, tenant, http.MethodDelete, "/api/v1/chat-bridge/routes/"+route.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("repeat delete: code=%d want 404", rec.Code)
	}
}

func TestHandlersShareHTTP(t *testing.T) {
	kc := &recordKChat{}
	svc, tenant := dbService(t, kc)
	mux, _ := chatHarness(t, svc)

	// share happy path (no StalwartURL → summary is the unknown
	// fallback, still posts) → 204.
	rec := chatReq(t, mux, tenant, http.MethodPost, "/api/v1/chat-bridge/share", shareRequest{
		EmailID: "e1", ChannelID: "chan-share",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("share: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(kc.posts) != 1 || kc.posts[0].channelID != "chan-share" {
		t.Fatalf("expected post to chan-share, got %+v", kc.posts)
	}

	// share missing channelId → 400 (ErrInvalidInput).
	rec = chatReq(t, mux, tenant, http.MethodPost, "/api/v1/chat-bridge/share", shareRequest{EmailID: "e1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing channel: code=%d want 400", rec.Code)
	}
}

// TestKChatAccessor pins the KChat() accessor used by sibling
// packages, including the nil-receiver guard.
func TestKChatAccessor(t *testing.T) {
	kc := &recordKChat{}
	svc := NewService(Config{KChat: kc})
	if svc.KChat() != kc {
		t.Error("KChat() should return the wired client")
	}
	var nilSvc *Service
	if nilSvc.KChat() != nil {
		t.Error("nil receiver KChat() should be nil")
	}
}

// TestPostChannelMessageDropsWhenUnconfigured covers the dev
// fall-through where KChat is not configured.
func TestPostChannelMessageDropsWhenUnconfigured(t *testing.T) {
	c := &httpKChatClient{cfg: Config{Logger: log.New(io.Discard, "", 0)}}
	if err := c.PostChannelMessage(context.Background(), "c1", ChannelMessage{Text: "x"}); err != nil {
		t.Errorf("unconfigured KChat should drop silently, got %v", err)
	}
}
