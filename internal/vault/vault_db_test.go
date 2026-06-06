package vault

import (
	"context"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestVaultFolderLifecycleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewVaultService(pool)
	ctx := context.Background()

	created, err := svc.CreateVaultFolder(ctx, Folder{
		TenantID:   tenant,
		UserID:     "user-1",
		FolderName: "Secrets",
		WrappedDEK: []byte("wrapped-key-blob"),
		Nonce:      []byte("nonce-bytes"),
	})
	if err != nil {
		t.Fatalf("CreateVaultFolder: %v", err)
	}
	if created.ID == "" || created.EncryptionMode != "StrictZK" || created.KeyAlgorithm != "XChaCha20-Poly1305" {
		t.Fatalf("created defaults wrong: %+v", created)
	}

	got, err := svc.GetVaultFolder(ctx, tenant, created.ID)
	if err != nil {
		t.Fatalf("GetVaultFolder: %v", err)
	}
	if string(got.WrappedDEK) != "wrapped-key-blob" {
		t.Errorf("wrapped_dek roundtrip mismatch: %q", got.WrappedDEK)
	}

	// List scoped to user and to whole tenant.
	byUser, err := svc.ListVaultFolders(ctx, tenant, "user-1")
	if err != nil || len(byUser) != 1 {
		t.Fatalf("ListVaultFolders byUser=%d err=%v", len(byUser), err)
	}
	if none, _ := svc.ListVaultFolders(ctx, tenant, "user-2"); len(none) != 0 {
		t.Errorf("ListVaultFolders other user=%d want 0", len(none))
	}

	// Re-wrap (key rotation).
	rot, err := svc.SetFolderEncryptionMeta(ctx, tenant, created.ID, []byte("rewrapped"), "", []byte("n2"))
	if err != nil {
		t.Fatalf("SetFolderEncryptionMeta: %v", err)
	}
	if string(rot.WrappedDEK) != "rewrapped" || rot.KeyAlgorithm != "XChaCha20-Poly1305" {
		t.Errorf("rotation result wrong: %+v", rot)
	}

	if err := svc.DeleteVaultFolder(ctx, tenant, created.ID); err != nil {
		t.Fatalf("DeleteVaultFolder: %v", err)
	}
	if _, err := svc.GetVaultFolder(ctx, tenant, created.ID); err == nil {
		t.Error("GetVaultFolder after delete: expected error")
	}
}

func TestVaultFolderValidationDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewVaultService(pool)
	ctx := context.Background()

	if _, err := svc.CreateVaultFolder(ctx, Folder{TenantID: tenant, UserID: "u", FolderName: "x", EncryptionMode: "Loose"}); err == nil {
		t.Error("invalid encryption_mode should be rejected")
	}
}

func TestProtectedFolderLifecycleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewProtectedFolderService(pool)
	ctx := context.Background()

	f, err := svc.CreateProtectedFolder(ctx, ProtectedFolder{
		TenantID: tenant, OwnerID: "owner-1", FolderName: "Team Docs",
	})
	if err != nil {
		t.Fatalf("CreateProtectedFolder: %v", err)
	}

	folders, err := svc.ListProtectedFolders(ctx, tenant, "owner-1")
	if err != nil || len(folders) != 1 {
		t.Fatalf("ListProtectedFolders=%d err=%v", len(folders), err)
	}

	// Grant, then upgrade permission (ON CONFLICT path).
	if _, err := svc.ShareFolder(ctx, tenant, f.ID, "owner-1", "grantee-1", "read"); err != nil {
		t.Fatalf("ShareFolder read: %v", err)
	}
	upgraded, err := svc.ShareFolder(ctx, tenant, f.ID, "owner-1", "grantee-1", "read_write")
	if err != nil {
		t.Fatalf("ShareFolder upgrade: %v", err)
	}
	if upgraded.Permission != "read_write" {
		t.Errorf("permission=%s want read_write", upgraded.Permission)
	}

	// Invalid permission rejected.
	if _, err := svc.ShareFolder(ctx, tenant, f.ID, "owner-1", "g2", "admin"); err == nil {
		t.Error("invalid permission should be rejected")
	}

	grants, err := svc.ListFolderAccess(ctx, tenant, f.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("ListFolderAccess=%d err=%v", len(grants), err)
	}

	if err := svc.UnshareFolder(ctx, tenant, f.ID, "owner-1", "grantee-1"); err != nil {
		t.Fatalf("UnshareFolder: %v", err)
	}
	if grants, _ := svc.ListFolderAccess(ctx, tenant, f.ID); len(grants) != 0 {
		t.Errorf("grants after unshare=%d want 0", len(grants))
	}

	// Access log records create + grant + revoke.
	log, err := svc.GetFolderAccessLog(ctx, tenant, f.ID)
	if err != nil {
		t.Fatalf("GetFolderAccessLog: %v", err)
	}
	if len(log) < 3 {
		t.Errorf("access log=%d want >=3 (create, grant, revoke)", len(log))
	}
}
