package integrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/testsupport"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// dbHarness bundles a fully-wired integration Service over a live
// Postgres plus the seeded tenant / client / user identifiers the
// DB-backed tests need.
type dbHarness struct {
	svc      *Service
	pool     *pgxpool.Pool
	tenant   string
	clientID string // oauth_clients.id (uuid PK) — what webhook_endpoints.oauth_client_id references
	userID   string // users.id (uuid PK)
}

func newDBHarness(t *testing.T, limiter *fakeLimiterStore) *dbHarness {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	wh := webhooks.NewService(pool)
	oa := oauth.NewService(pool)

	cfg := ServiceConfig{
		Pool:     pool,
		Webhooks: wh,
		OAuth:    oa,
		Logger:   log.New(io.Discard, "", 0),
	}
	if limiter != nil {
		cfg.LimiterStore = limiter
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	client, _, err := oa.RegisterClient(ctx, tenant, "Zapier",
		oauth.ClientTypeConfidential,
		[]string{"https://app.example.com/callback"},
		[]string{oauth.ScopeReadMail, oauth.ScopeReadCalendar}, "", "")
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	userID := seedUserRow(t, pool, tenant)
	return &dbHarness{svc: svc, pool: pool, tenant: tenant, clientID: client.ID, userID: userID}
}

// seedUserRow inserts a minimal active user and returns its uuid.
func seedUserRow(t *testing.T, pool *pgxpool.Pool, tenant string) string {
	t.Helper()
	suffix := randHex(t, 6)
	var id string
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(context.Background(), tx, tenant); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `
			INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name)
			VALUES ($1::uuid, $2, $3, $4, $5)
			RETURNING id::text
		`, tenant, "kc-"+suffix, "sw-"+suffix, "u-"+suffix+"@example.com", "User "+suffix).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedAccessToken inserts a live (non-revoked, unexpired) access
// token row granting the given scopes for the harness client+user.
func (h *dbHarness) seedAccessToken(t *testing.T, scopes []string, expires time.Time, revoked bool) {
	t.Helper()
	scopesJSON := "[]"
	if len(scopes) > 0 {
		b := `["` + scopes[0] + `"`
		for _, s := range scopes[1:] {
			b += `,"` + s + `"`
		}
		b += "]"
		scopesJSON = b
	}
	var revokedAt *time.Time
	if revoked {
		now := time.Now()
		revokedAt = &now
	}
	err := pgx.BeginFunc(context.Background(), h.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(context.Background(), tx, h.tenant); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `
			INSERT INTO oauth_access_tokens (tenant_id, client_id, user_id, token_hash, scopes, expires_at, revoked_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, $6, $7)
		`, h.tenant, h.clientID, h.userID, randHex(t, 16), scopesJSON, expires, revokedAt)
		return err
	})
	if err != nil {
		t.Fatalf("seed access token: %v", err)
	}
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// TestNewServiceValidation pins the constructor's required-field
// contract.
func TestNewServiceValidation(t *testing.T) {
	if _, err := NewService(ServiceConfig{}); err == nil {
		t.Error("NewService with no Pool should error")
	}
	pool := testsupport.Pool(t)
	if _, err := NewService(ServiceConfig{Pool: pool}); err == nil {
		t.Error("NewService with no Webhooks should error")
	}
	if _, err := NewService(ServiceConfig{Pool: pool, Webhooks: webhooks.NewService(pool)}); err == nil {
		t.Error("NewService with no OAuth should error")
	}
	svc, err := NewService(ServiceConfig{
		Pool: pool, Webhooks: webhooks.NewService(pool), OAuth: oauth.NewService(pool),
		DefaultClientDispatchPerHour: 0,
	})
	if err != nil {
		t.Fatalf("NewService valid: %v", err)
	}
	if svc.cfg.DefaultClientDispatchPerHour != DefaultClientDispatchPerHour {
		t.Errorf("default quota = %d; want %d", svc.cfg.DefaultClientDispatchPerHour, DefaultClientDispatchPerHour)
	}
}

// TestRegisterListDeleteWebhookForClientDB drives the full
// subscribe → list → delete lifecycle against Postgres.
func TestRegisterListDeleteWebhookForClientDB(t *testing.T) {
	h := newDBHarness(t, nil)
	ctx := context.Background()

	res, err := h.svc.RegisterWebhookForClient(ctx, h.tenant, h.clientID, h.userID,
		[]string{oauth.ScopeReadMail}, "https://hook.example.com/x",
		[]string{webhooks.EventEmailReceived}, webhooks.SigningV1)
	if err != nil {
		t.Fatalf("RegisterWebhookForClient: %v", err)
	}
	if res.Endpoint == nil || res.Secret == "" {
		t.Fatalf("expected endpoint + secret, got %+v", res)
	}

	// List returns the row we just created.
	list, err := h.svc.ListWebhooksForClient(ctx, h.tenant, h.clientID)
	if err != nil {
		t.Fatalf("ListWebhooksForClient: %v", err)
	}
	if len(list) != 1 || list[0].ID != res.Endpoint.ID {
		t.Fatalf("list = %+v; want the one registered endpoint", list)
	}

	// A different client sees nothing (cross-client isolation).
	other, _, err := h.svc.cfg.OAuth.RegisterClient(ctx, h.tenant, "Other",
		oauth.ClientTypeConfidential, []string{"https://o.example.com/cb"},
		[]string{oauth.ScopeReadMail}, "", "")
	if err != nil {
		t.Fatalf("register other client: %v", err)
	}
	otherList, err := h.svc.ListWebhooksForClient(ctx, h.tenant, other.ID)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(otherList) != 0 {
		t.Errorf("cross-client list leaked %d rows", len(otherList))
	}

	// Delete by the wrong client → ErrWebhookNotFound.
	if err := h.svc.DeleteWebhookForClient(ctx, h.tenant, other.ID, res.Endpoint.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("cross-client delete: want ErrWebhookNotFound got %v", err)
	}
	// Delete by the owner → ok.
	if err := h.svc.DeleteWebhookForClient(ctx, h.tenant, h.clientID, res.Endpoint.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	// Second delete → not found.
	if err := h.svc.DeleteWebhookForClient(ctx, h.tenant, h.clientID, res.Endpoint.ID); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("repeat delete: want ErrWebhookNotFound got %v", err)
	}
}

// TestRegisterWebhookForClientErrors covers the validation + scope
// rejection branches.
func TestRegisterWebhookForClientErrors(t *testing.T) {
	h := newDBHarness(t, nil)
	ctx := context.Background()

	cases := []struct {
		name    string
		tenant  string
		client  string
		user    string
		url     string
		events  []string
		scopes  []string
		wantErr error
	}{
		{"no tenant", "", h.clientID, h.userID, "https://x", []string{webhooks.EventEmailReceived}, []string{oauth.ScopeReadMail}, nil},
		{"no client", h.tenant, "", h.userID, "https://x", []string{webhooks.EventEmailReceived}, []string{oauth.ScopeReadMail}, nil},
		{"no user", h.tenant, h.clientID, "", "https://x", []string{webhooks.EventEmailReceived}, []string{oauth.ScopeReadMail}, nil},
		{"no url", h.tenant, h.clientID, h.userID, "", []string{webhooks.EventEmailReceived}, []string{oauth.ScopeReadMail}, nil},
		{"no events", h.tenant, h.clientID, h.userID, "https://x", nil, []string{oauth.ScopeReadMail}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.RegisterWebhookForClient(ctx, tc.tenant, tc.client, tc.user, tc.scopes, tc.url, tc.events, webhooks.SigningV1)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	// Every requested event denied → ErrInsufficientScope + denied list.
	res, err := h.svc.RegisterWebhookForClient(ctx, h.tenant, h.clientID, h.userID,
		[]string{oauth.ScopeReadMail}, "https://x",
		[]string{webhooks.EventCalendarCreated}, webhooks.SigningV1)
	if !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("want ErrInsufficientScope got %v", err)
	}
	if len(res.Denied) != 1 || res.Denied[0] != webhooks.EventCalendarCreated {
		t.Errorf("denied = %v; want [calendar.event_created]", res.Denied)
	}
}

// TestTestFireForClientDB covers the ownership-gated test-fire.
func TestTestFireForClientDB(t *testing.T) {
	h := newDBHarness(t, nil)
	ctx := context.Background()

	res, err := h.svc.RegisterWebhookForClient(ctx, h.tenant, h.clientID, h.userID,
		[]string{oauth.ScopeReadMail}, "https://hook.example.com/fire",
		[]string{webhooks.EventEmailReceived}, webhooks.SigningV1)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Owner can test-fire an active endpoint.
	if _, err := h.svc.TestFireForClient(ctx, h.tenant, h.clientID, res.Endpoint.ID); err != nil {
		t.Fatalf("TestFireForClient: %v", err)
	}

	// Unknown endpoint → not found.
	if _, err := h.svc.TestFireForClient(ctx, h.tenant, h.clientID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrWebhookNotFound) {
		t.Errorf("unknown endpoint: want ErrWebhookNotFound got %v", err)
	}

	// Missing args → validation error.
	if _, err := h.svc.TestFireForClient(ctx, "", h.clientID, res.Endpoint.ID); err == nil {
		t.Error("missing tenant should error")
	}
}

// TestDispatchEventIntegrationOwnedDB exercises the full dispatch
// fan-out: a live token grants read:mail, so an integration-owned
// subscriber receives the delivery.
func TestDispatchEventIntegrationOwnedDB(t *testing.T) {
	h := newDBHarness(t, nil)
	ctx := context.Background()

	if _, err := h.svc.RegisterWebhookForClient(ctx, h.tenant, h.clientID, h.userID,
		[]string{oauth.ScopeReadMail}, "https://hook.example.com/disp",
		[]string{webhooks.EventEmailReceived}, webhooks.SigningV1); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Grant the scope via a live token.
	h.seedAccessToken(t, []string{oauth.ScopeReadMail}, time.Now().Add(time.Hour), false)

	n, err := h.svc.DispatchEvent(ctx, h.tenant, webhooks.EventEmailReceived, map[string]any{"id": "e1"})
	if err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	if n < 1 {
		t.Errorf("expected >=1 enqueued delivery, got %d", n)
	}

	// Validation guard.
	if _, err := h.svc.DispatchEvent(ctx, "", "x", nil); err == nil {
		t.Error("DispatchEvent with no tenant should error")
	}
}

// TestDispatchEventScopeRevokedSkips verifies that without a live
// granting token the integration-owned subscriber is skipped.
func TestDispatchEventScopeRevokedSkips(t *testing.T) {
	h := newDBHarness(t, nil)
	ctx := context.Background()

	if _, err := h.svc.RegisterWebhookForClient(ctx, h.tenant, h.clientID, h.userID,
		[]string{oauth.ScopeReadMail}, "https://hook.example.com/skip",
		[]string{webhooks.EventEmailReceived}, webhooks.SigningV1); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Only a REVOKED token exists → no live grant → skipped.
	h.seedAccessToken(t, []string{oauth.ScopeReadMail}, time.Now().Add(time.Hour), true)

	n, err := h.svc.DispatchEvent(ctx, h.tenant, webhooks.EventEmailReceived, map[string]any{"id": "e2"})
	if err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 deliveries (scope revoked), got %d", n)
	}
}

// TestDispatchEventQuotaDeferredDB verifies the rate-limited path:
// once the client's hourly bucket is full, the delivery is still
// enqueued (deferred), preserving at-least-once semantics.
func TestDispatchEventQuotaDeferredDB(t *testing.T) {
	limiter := newFakeLimiterStore()
	h := newDBHarness(t, limiter)
	// Tight quota so the second dispatch defers.
	h.svc.cfg.DefaultClientDispatchPerHour = 1
	ctx := context.Background()

	if _, err := h.svc.RegisterWebhookForClient(ctx, h.tenant, h.clientID, h.userID,
		[]string{oauth.ScopeReadMail}, "https://hook.example.com/q",
		[]string{webhooks.EventEmailReceived}, webhooks.SigningV1); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.seedAccessToken(t, []string{oauth.ScopeReadMail}, time.Now().Add(time.Hour), false)

	// First dispatch consumes the single quota slot.
	if _, err := h.svc.DispatchEvent(ctx, h.tenant, webhooks.EventEmailReceived, map[string]any{"n": 1}); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	// Second dispatch is over quota but still enqueues (deferred).
	n, err := h.svc.DispatchEvent(ctx, h.tenant, webhooks.EventEmailReceived, map[string]any{"n": 2})
	if err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	if n != 1 {
		t.Errorf("deferred dispatch enqueued = %d; want 1", n)
	}
}
