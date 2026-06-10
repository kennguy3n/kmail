package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestSignupRepositoryDB exercises the Postgres-backed
// SignupRepository through its full lifecycle: create → set checkout
// session → look up by id and session → mark active (and replay) →
// mark failed, plus the not-found sentinels.
func TestSignupRepositoryDB(t *testing.T) {
	pool := testsupport.Pool(t)
	repo := NewSignupRepository(pool)
	ctx := context.Background()
	u := uniq()
	email := "signup-" + u + "@example.com"

	sr, err := repo.Create(ctx, email, "Org "+u, "pro")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sr.Status != "pending" {
		t.Errorf("status=%q want pending", sr.Status)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM signup_requests WHERE id = $1::uuid`, sr.ID)
	})

	got, err := repo.GetByID(ctx, sr.ID)
	if err != nil || got.Email != email {
		t.Fatalf("GetByID: %v got=%+v", err, got)
	}

	sessionID := "cs_test_" + u
	if err := repo.SetCheckoutSession(ctx, sr.ID, sessionID); err != nil {
		t.Fatalf("SetCheckoutSession: %v", err)
	}
	bySession, err := repo.GetByCheckoutSession(ctx, sessionID)
	if err != nil || bySession.ID != sr.ID {
		t.Fatalf("GetByCheckoutSession: %v got=%+v", err, bySession)
	}

	// SetCheckoutSession on an unknown id → ErrSignupNotFound.
	if err := repo.SetCheckoutSession(ctx, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, ErrSignupNotFound) {
		t.Errorf("SetCheckoutSession unknown=%v want ErrSignupNotFound", err)
	}

	completed := time.Now().UTC().Truncate(time.Second)
	if err := repo.MarkActive(ctx, sr.ID, completed); err != nil {
		t.Fatalf("MarkActive: %v", err)
	}
	active, _ := repo.GetByID(ctx, sr.ID)
	if active.Status != "active" || active.CompletedAt == nil {
		t.Fatalf("after MarkActive: status=%q completed=%v", active.Status, active.CompletedAt)
	}
	firstCompleted := *active.CompletedAt

	// Replayed completion is a no-op: completed_at is not re-stamped
	// because the row is already active.
	if err := repo.MarkActive(ctx, sr.ID, completed.Add(time.Hour)); err != nil {
		t.Fatalf("MarkActive replay: %v", err)
	}
	replayed, _ := repo.GetByID(ctx, sr.ID)
	if !replayed.CompletedAt.Equal(firstCompleted) {
		t.Errorf("replayed completed_at moved: %v != %v", replayed.CompletedAt, firstCompleted)
	}

	// MarkFailed only flips a still-pending row; an already-active row
	// is untouched.
	if err := repo.MarkFailed(ctx, sr.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	stillActive, _ := repo.GetByID(ctx, sr.ID)
	if stillActive.Status != "active" {
		t.Errorf("MarkFailed flipped an active row to %q", stillActive.Status)
	}

	// GetByID / GetByCheckoutSession not-found sentinels.
	if _, err := repo.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrSignupNotFound) {
		t.Errorf("GetByID unknown=%v want ErrSignupNotFound", err)
	}
	if _, err := repo.GetByCheckoutSession(ctx, "missing"); !errors.Is(err, ErrSignupNotFound) {
		t.Errorf("GetByCheckoutSession unknown=%v want ErrSignupNotFound", err)
	}
}

// TestSignupProvisionerDB covers the TenantProvisioner adapter:
// tenant creation, slug lookup, self-service flagging, admin-user
// creation, and the duplicate-user idempotency sentinel.
func TestSignupProvisionerDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	prov := NewSignupProvisioner(svc, pool)
	ctx := context.Background()
	u := uniq()
	slug := "ss-" + u

	tn, err := prov.CreateTenant(ctx, CreateTenantInput{Name: "SS " + u, Slug: slug, Plan: "pro"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE tenant_id = $1::uuid`, tn.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, tn.ID)
	})

	// Re-creating the same slug surfaces ErrTenantExists.
	if _, err := prov.CreateTenant(ctx, CreateTenantInput{Name: "dup", Slug: slug, Plan: "pro"}); !errors.Is(err, ErrTenantExists) {
		t.Errorf("duplicate slug=%v want ErrTenantExists", err)
	}

	bySlug, err := prov.GetTenantBySlug(ctx, slug)
	if err != nil || bySlug.ID != tn.ID {
		t.Fatalf("GetTenantBySlug: %v got=%+v", err, bySlug)
	}
	if _, err := prov.GetTenantBySlug(ctx, "no-such-"+u); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTenantBySlug unknown=%v want ErrNotFound", err)
	}

	if err := prov.MarkSelfService(ctx, tn.ID); err != nil {
		t.Fatalf("MarkSelfService: %v", err)
	}
	var selfService bool
	if err := pool.QueryRow(ctx, `SELECT self_service FROM tenants WHERE id = $1::uuid`, tn.ID).Scan(&selfService); err != nil || !selfService {
		t.Errorf("self_service not set: %v %v", err, selfService)
	}

	// EnsureProvisioned with no provisioner/billing hooks wired is a no-op.
	if err := prov.EnsureProvisioned(ctx, tn.ID, "pro"); err != nil {
		t.Errorf("EnsureProvisioned no-op=%v", err)
	}

	adminEmail := "admin-" + u + "@example.com"
	admin, err := prov.CreateAdminUser(ctx, tn.ID, adminEmail, "Admin")
	if err != nil {
		t.Fatalf("CreateAdminUser: %v", err)
	}
	if admin.Role != "owner" {
		t.Errorf("admin role=%q want owner", admin.Role)
	}
	// Replayed admin creation collides on the email UNIQUE constraint.
	if _, err := prov.CreateAdminUser(ctx, tn.ID, adminEmail, "Admin"); !errors.Is(err, ErrUserExists) {
		t.Errorf("duplicate admin=%v want ErrUserExists", err)
	}
}

// TestStripeCheckoutHTTP covers the production checkout client against
// a stub Stripe endpoint, plus its failure branches.
func TestStripeCheckoutHTTP(t *testing.T) {
	ctx := context.Background()
	params := CheckoutSessionParams{
		Plan: "pro", PriceID: "price_123", CustomerEmail: "a@b.com",
		SuccessURL: "https://ok", CancelURL: "https://no", SignupRequestID: "sr_1",
	}

	// No API key configured → ErrCheckoutUnavailable.
	noKey := &StripeCheckoutHTTP{}
	if _, err := noKey.CreateCheckoutSession(ctx, params); !errors.Is(err, ErrCheckoutUnavailable) {
		t.Errorf("no key=%v want ErrCheckoutUnavailable", err)
	}

	// Happy path: stub returns a session id/url.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/checkout/sessions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("mode") != "subscription" || r.Form.Get("client_reference_id") != "sr_1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(CheckoutSession{ID: "cs_live_1", URL: "https://checkout"})
	}))
	defer srv.Close()

	client := &StripeCheckoutHTTP{APIKey: "sk_test", BaseURL: srv.URL, HTTP: srv.Client()}
	cs, err := client.CreateCheckoutSession(ctx, params)
	if err != nil || cs.ID != "cs_live_1" || cs.URL != "https://checkout" {
		t.Fatalf("CreateCheckoutSession: %v got=%+v", err, cs)
	}

	// Stripe 4xx → error.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer errSrv.Close()
	errClient := &StripeCheckoutHTTP{APIKey: "sk_test", BaseURL: errSrv.URL, HTTP: errSrv.Client()}
	if _, err := errClient.CreateCheckoutSession(ctx, params); err == nil {
		t.Error("expected error on Stripe 4xx")
	}

	// Response missing id/url → error.
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer emptySrv.Close()
	emptyClient := &StripeCheckoutHTTP{APIKey: "sk_test", BaseURL: emptySrv.URL, HTTP: emptySrv.Client()}
	if _, err := emptyClient.CreateCheckoutSession(ctx, params); err == nil {
		t.Error("expected error on missing id/url")
	}
}

// TestChecklistAdapterAndHelpers covers the small pure wiring helpers.
func TestChecklistAdapterAndHelpers(t *testing.T) {
	if NewChecklistAdapter(nil) != nil {
		t.Error("NewChecklistAdapter(nil) should be nil")
	}
	var called bool
	a := NewChecklistAdapter(func(ctx context.Context, tenantID string) error {
		called = true
		return nil
	})
	if err := a.InitChecklist(context.Background(), "t1"); err != nil || !called {
		t.Errorf("InitChecklist: err=%v called=%v", err, called)
	}

	// truncateForLog leaves short input intact and elides long input.
	if got := truncateForLog([]byte("short")); got != "short" {
		t.Errorf("truncateForLog short=%q", got)
	}
	long := strings.Repeat("x", 600)
	if got := truncateForLog([]byte(long)); len(got) >= len(long) || !strings.HasSuffix(got, "…") {
		t.Errorf("truncateForLog long not elided: len=%d", len(got))
	}

	// NewJMAPWelcomeMailer returns nil when any required field is empty.
	if NewJMAPWelcomeMailer(nil, "t", "u", "from@x", "id", nil) != nil {
		t.Error("nil submitter should yield nil mailer")
	}
}
