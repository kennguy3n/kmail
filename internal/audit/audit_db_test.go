package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// dbService returns an audit Service backed by an RLS-enforced pool so
// Query / VerifyChain are tenant-scoped exactly as in production (the
// superuser bypasses RLS, which would leak other tenants' rows). The
// superuser pool is returned separately for seeding and tamper writes.
func dbService(t *testing.T) (*Service, string, *pgxpool.Pool) {
	t.Helper()
	admin := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, admin, "pro", "active")
	rls := testsupport.RLSPool(t)
	return NewService(rls), tenant, admin
}

func TestLogQueryExportVerifyDB(t *testing.T) {
	svc, tenant, admin := dbService(t)
	ctx := context.Background()

	e1, err := svc.Log(ctx, Entry{
		TenantID: tenant, ActorID: "admin-1", ActorType: ActorAdmin,
		Action: "user.create", ResourceType: "user", ResourceID: "u-1",
		Metadata: map[string]any{"email": "a@example.com"}, IPAddress: "192.0.2.5",
	})
	if err != nil {
		t.Fatalf("Log e1: %v", err)
	}
	if e1.PrevHash != "" {
		t.Errorf("first entry prev_hash=%q want empty", e1.PrevHash)
	}
	if e1.EntryHash == "" {
		t.Error("entry hash not computed")
	}

	e2, err := svc.Log(ctx, Entry{
		TenantID: tenant, ActorID: "admin-1", ActorType: ActorAdmin,
		Action: "user.delete", ResourceType: "user", ResourceID: "u-1",
	})
	if err != nil {
		t.Fatalf("Log e2: %v", err)
	}
	if e2.PrevHash != e1.EntryHash {
		t.Errorf("e2.prev_hash=%q want %q", e2.PrevHash, e1.EntryHash)
	}

	// Query all + filtered by action.
	all, err := svc.Query(ctx, tenant, QueryFilters{})
	if err != nil || len(all) != 2 {
		t.Fatalf("Query all=%d err=%v", len(all), err)
	}
	filtered, err := svc.Query(ctx, tenant, QueryFilters{Action: "user.delete"})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("Query filtered=%d err=%v", len(filtered), err)
	}

	// Export json + csv.
	jsonOut, err := svc.Export(ctx, tenant, "json", time.Time{}, time.Time{})
	if err != nil || !strings.Contains(string(jsonOut), "user.create") {
		t.Fatalf("Export json err=%v out=%s", err, jsonOut)
	}
	csvOut, err := svc.Export(ctx, tenant, "csv", time.Time{}, time.Time{})
	if err != nil || !strings.Contains(string(csvOut), "entry_hash") {
		t.Fatalf("Export csv err=%v", err)
	}
	if _, err := svc.Export(ctx, tenant, "xml", time.Time{}, time.Time{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Export xml want ErrInvalidInput, got %v", err)
	}

	// Intact chain verifies.
	if err := svc.VerifyChain(ctx, tenant); err != nil {
		t.Fatalf("VerifyChain intact: %v", err)
	}

	// Tamper a row in place (bypasses RLS as superuser) → chain breaks.
	if _, err := admin.Exec(ctx, `UPDATE audit_log SET action = 'tampered' WHERE id = $1::uuid`, e2.ID); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := svc.VerifyChain(ctx, tenant); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("VerifyChain after tamper=%v want ErrChainBroken", err)
	}
}

func TestLogValidationDB(t *testing.T) {
	svc, _, _ := dbService(t)
	ctx := context.Background()
	if _, err := svc.Log(ctx, Entry{Action: "x", ActorType: ActorUser}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing tenant want ErrInvalidInput, got %v", err)
	}
	if _, err := svc.Query(ctx, "", QueryFilters{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty tenant query want ErrInvalidInput, got %v", err)
	}
}

func TestLogDropsInvalidIPDB(t *testing.T) {
	svc, tenant, _ := dbService(t)
	ctx := context.Background()
	e, err := svc.Log(ctx, Entry{
		TenantID: tenant, ActorID: "a", ActorType: ActorSystem,
		Action: "x", ResourceType: "y", IPAddress: "not-an-ip",
	})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if e.IPAddress != "" {
		t.Errorf("invalid IP should be dropped, got %q", e.IPAddress)
	}
	// Chain still verifies with the normalised (empty) IP.
	if err := svc.VerifyChain(ctx, tenant); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}
