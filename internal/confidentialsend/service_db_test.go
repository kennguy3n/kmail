package confidentialsend

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// testPool dials the integration database named by
// KMAIL_TEST_DATABASE_URL (or DATABASE_URL). When neither is set —
// the default for `make test` and CI, which have no Postgres — the
// calling test is skipped. The returned pool is closed via t.Cleanup.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run confidential-send DB tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable (%v); skipping integration test", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedTenant inserts an active tenant and registers cleanup that
// removes it (cascading to its confidential_send_links rows).
func seedTenant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("cs-mls-test-%d", time.Now().UnixNano())
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status)
		VALUES ($1, $2, 'pro', 'active')
		RETURNING id::text
	`, "confidential-send-test", slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, id)
	})
	return id
}

// TestListSentSecureMessages_ReportsMLSColumns is a regression guard
// for the list queries omitting the MLS columns: a message created
// with MLS wrapping must report MLSWrapped=true and its epoch in the
// list view, while a plain link-portal message reports false. The
// list must also never leak mls_wrapping_key.
func TestListSentSecureMessages_ReportsMLSColumns(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)
	ctx := context.Background()

	// Mock deriver => MLSEnabled() reports true.
	svc := NewService(pool).WithMLS(&mockDeriver{wrapKey: "ab12cd34"})

	wrapped, err := svc.CreateSecureMessage(ctx, CreateRequest{
		TenantID:         tenantID,
		SenderID:         "alice",
		EncryptedBlobRef: "blob://wrapped",
		SenderLeafKey:    "leaf-xyz",
		Recipients:       []string{"bob@x.test"},
	})
	if err != nil {
		t.Fatalf("create MLS-wrapped: %v", err)
	}
	if !wrapped.MLSWrapped {
		t.Fatalf("precondition: created message should be MLS-wrapped")
	}

	linkOnly, err := svc.CreateSecureMessage(ctx, CreateRequest{
		TenantID:         tenantID,
		SenderID:         "alice",
		EncryptedBlobRef: "blob://link-only",
	})
	if err != nil {
		t.Fatalf("create link-only: %v", err)
	}

	// Bump the wrapped row's epoch (inside the tenant GUC so RLS
	// permits the write) to prove the list reads mls_epoch from the
	// row rather than hardcoding 0.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE confidential_send_links SET mls_epoch = 3 WHERE id = $1::uuid`,
			wrapped.ID)
		return err
	}); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}

	got, err := svc.ListSentSecureMessages(ctx, tenantID, "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := make(map[string]SecureMessage, len(got))
	for _, m := range got {
		byID[m.ID] = m
	}

	w, ok := byID[wrapped.ID]
	if !ok {
		t.Fatalf("wrapped message missing from list")
	}
	if !w.MLSWrapped {
		t.Errorf("list MLSWrapped = false for an MLS-wrapped message; want true")
	}
	if w.MLSEpoch != 3 {
		t.Errorf("list MLSEpoch = %d; want 3 (must reflect the row)", w.MLSEpoch)
	}
	if w.MLSWrappingKey != "" {
		t.Errorf("list must not expose mls_wrapping_key, got %q", w.MLSWrappingKey)
	}

	l, ok := byID[linkOnly.ID]
	if !ok {
		t.Fatalf("link-only message missing from list")
	}
	if l.MLSWrapped {
		t.Errorf("list MLSWrapped = true for a link-only message; want false")
	}
}
