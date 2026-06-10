package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// seedStorageCreds inserts a tenant_storage_credentials row (the
// placement policy lives here) for the given tenant and registers
// cleanup. The pool connects as a superuser in tests, so the RLS
// policy on the table is bypassed.
func seedStorageCreds(t *testing.T, tenantID, ref, mode string) {
	t.Helper()
	pool := testsupport.Pool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO tenant_storage_credentials
			(tenant_id, bucket_name, access_key, encrypted_secret_key,
			 placement_policy_ref, encryption_mode_default)
		VALUES ($1::uuid, $2, 'ak', '\x00'::bytea, $3, $4)
	`, tenantID, "bucket-"+tenantID, ref, mode)
	if err != nil {
		t.Fatalf("seed storage creds: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenant_storage_credentials WHERE tenant_id = $1::uuid`, tenantID)
	})
}

func TestPlacementGetNoConsoleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	seedStorageCreds(t, tenant, "ref-1", "client_side")

	svc := NewPlacementService(pool, "")
	got, err := svc.GetPlacementPolicy(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetPlacementPolicy: %v", err)
	}
	if got.PolicyRef != "ref-1" || got.EncryptionMode != "client_side" {
		t.Errorf("policy=%+v want ref-1/client_side", got)
	}

	// Guard clauses.
	if _, err := svc.GetPlacementPolicy(context.Background(), ""); err == nil {
		t.Error("empty tenantID should error")
	}
	if _, err := NewPlacementService(nil, "").GetPlacementPolicy(context.Background(), tenant); err == nil {
		t.Error("nil pool should error")
	}
}

func TestPlacementGetWithConsoleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	seedStorageCreds(t, tenant, "ref-2", "managed")

	// Console returns an authoritative policy that overrides the local row.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"countries":          []string{"EU", "US"},
			"preferred_provider": "wasabi",
			"encryption_mode":    "managed",
			"erasure_profile":    "rs-6-3",
		})
	}))
	defer srv.Close()

	svc := NewPlacementService(pool, srv.URL)
	got, err := svc.GetPlacementPolicy(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetPlacementPolicy: %v", err)
	}
	if len(got.Countries) != 2 || got.PreferredProvider != "wasabi" || got.ErasureProfile != "rs-6-3" {
		t.Errorf("console merge failed: %+v", got)
	}

	// Console 5xx → best-effort fallback to defaults (no error).
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	fallback, err := NewPlacementService(pool, errSrv.URL).GetPlacementPolicy(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetPlacementPolicy fallback: %v", err)
	}
	if fallback.PreferredProvider != "wasabi" || len(fallback.Countries) != 1 {
		t.Errorf("expected default fallback, got %+v", fallback)
	}
}

func TestPlacementUpdateDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	seedStorageCreds(t, tenant, "ref-3", "managed")

	var gotPush bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			gotPush = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	svc := NewPlacementService(pool, srv.URL)
	ctx := context.Background()

	// Validation: empty countries.
	if _, err := svc.UpdatePlacementPolicy(ctx, tenant, "privacy", PlacementPolicy{}); err == nil {
		t.Error("empty countries should error")
	}
	// Validation: client_side requires privacy plan.
	if _, err := svc.UpdatePlacementPolicy(ctx, tenant, "pro", PlacementPolicy{
		Countries: []string{"US"}, EncryptionMode: "client_side",
	}); err == nil {
		t.Error("client_side on non-privacy plan should error")
	}

	// Happy path on privacy plan: persists + pushes to console.
	out, err := svc.UpdatePlacementPolicy(ctx, tenant, "privacy", PlacementPolicy{
		Countries: []string{"EU"}, EncryptionMode: "client_side", PolicyRef: "ref-3b",
		PreferredProvider: "wasabi", ErasureProfile: "rs-6-3",
	})
	if err != nil {
		t.Fatalf("UpdatePlacementPolicy: %v", err)
	}
	if out.EncryptionMode != "client_side" || out.TenantID != tenant {
		t.Errorf("unexpected output %+v", out)
	}
	if !gotPush {
		t.Error("expected PUT to console")
	}
	// Persisted to the row.
	var mode, ref string
	if err := pool.QueryRow(ctx, `SELECT encryption_mode_default, placement_policy_ref FROM tenant_storage_credentials WHERE tenant_id = $1::uuid`, tenant).Scan(&mode, &ref); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if mode != "client_side" || ref != "ref-3b" {
		t.Errorf("persisted mode=%q ref=%q", mode, ref)
	}

	// Empty encryption mode defaults to managed.
	out2, err := svc.UpdatePlacementPolicy(ctx, tenant, "pro", PlacementPolicy{Countries: []string{"US"}})
	if err != nil {
		t.Fatalf("UpdatePlacementPolicy default mode: %v", err)
	}
	if out2.EncryptionMode != "managed" {
		t.Errorf("default mode=%q want managed", out2.EncryptionMode)
	}

	// Console PUT failure surfaces as an error.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failSrv.Close()
	if _, err := NewPlacementService(pool, failSrv.URL).UpdatePlacementPolicy(ctx, tenant, "pro", PlacementPolicy{Countries: []string{"US"}}); err == nil {
		t.Error("console PUT failure should surface as error")
	}
}

func TestListAvailableRegions(t *testing.T) {
	svc := NewPlacementService(nil, "")
	regions := svc.ListAvailableRegions()
	if len(regions) != 3 {
		t.Fatalf("regions=%d want 3", len(regions))
	}
}
