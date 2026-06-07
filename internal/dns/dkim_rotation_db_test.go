package dns

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// seedDomain inserts a tenant-scoped domain row and returns its id.
func seedDomain(t *testing.T, svc *DKIMRotationService, tenant string) string {
	t.Helper()
	name := fmt.Sprintf("dkim-%d.example.com", time.Now().UnixNano())
	var id string
	ctx := context.Background()
	err := pgxBeginDomain(ctx, svc, tenant, &id, name)
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	return id
}

func pgxBeginDomain(ctx context.Context, svc *DKIMRotationService, tenant string, id *string, name string) error {
	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := middleware.SetTenantGUC(ctx, tx, tenant); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO domains (tenant_id, domain) VALUES ($1::uuid, $2) RETURNING id::text`, tenant, name).Scan(id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func dkimService(t *testing.T) (*DKIMRotationService, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return NewDKIMRotationService(pool, nil), tenant
}

func TestDKIMGenerateKeyPair(t *testing.T) {
	svc := NewDKIMRotationService(nil, nil)
	pair, err := svc.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if pair.Selector == "" || pair.PublicKey == "" || pair.PrivateKey == "" {
		t.Fatalf("incomplete key pair: %+v", pair)
	}
}

func TestDKIMRotateListRevokeDB(t *testing.T) {
	svc, tenant := dkimService(t)
	domainID := seedDomain(t, svc, tenant)
	ctx := context.Background()

	key, err := svc.RotateKey(ctx, tenant, domainID)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if key.Status != DKIMKeyActive || key.Selector == "" {
		t.Fatalf("rotated key wrong: %+v", key)
	}

	keys, err := svc.ListKeys(ctx, tenant, domainID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys=%d err=%v", len(keys), err)
	}

	if err := svc.RevokeKey(ctx, tenant, domainID, key.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if err := svc.RevokeKey(ctx, tenant, domainID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeKey missing: want ErrNotFound got %v", err)
	}
}

func TestDKIMValidationDB(t *testing.T) {
	svc := NewDKIMRotationService(nil, nil)
	ctx := context.Background()
	if _, err := svc.ListKeys(ctx, "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ListKeys empty: want ErrInvalidInput got %v", err)
	}
	if _, err := svc.RotateKey(ctx, "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("RotateKey empty: want ErrInvalidInput got %v", err)
	}
	if err := svc.RevokeKey(ctx, "", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("RevokeKey empty: want ErrInvalidInput got %v", err)
	}
}
