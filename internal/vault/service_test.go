package vault

import (
	"context"
	"strings"
	"testing"
)

// nilPoolService returns a Service with a nil pool so the
// validation paths can run without a database.
func nilPoolService() *VaultService { return NewVaultService(nil) }

func TestCreateVaultFolder_Validation(t *testing.T) {
	s := nilPoolService()
	cases := []struct {
		name string
		in   Folder
		want string
	}{
		{"missing tenant", Folder{UserID: "u", FolderName: "secrets"}, "tenant_id required"},
		{"missing user", Folder{TenantID: "t", FolderName: "secrets"}, "user_id required"},
		{"missing name", Folder{TenantID: "t", UserID: "u"}, "folder_name required"},
		{"bad mode", Folder{TenantID: "t", UserID: "u", FolderName: "x", EncryptionMode: "ManagedEncrypted"}, "invalid encryption_mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateVaultFolder(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateVaultFolder(%+v) = %v, want error containing %q", tc.in, err, tc.want)
			}
		})
	}
}

func TestListVaultFolders_RequiresTenantID(t *testing.T) {
	s := nilPoolService()
	if _, err := s.ListVaultFolders(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "tenantID required") {
		t.Fatalf("ListVaultFolders empty tenantID = %v, want tenantID required", err)
	}
}

func TestGetVaultFolder_RequiresIDs(t *testing.T) {
	s := nilPoolService()
	if _, err := s.GetVaultFolder(context.Background(), "", "f"); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("GetVaultFolder empty tenantID = %v", err)
	}
	if _, err := s.GetVaultFolder(context.Background(), "t", ""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("GetVaultFolder empty folderID = %v", err)
	}
}

func TestDeleteVaultFolder_RequiresIDs(t *testing.T) {
	s := nilPoolService()
	if err := s.DeleteVaultFolder(context.Background(), "", "f"); err == nil {
		t.Fatalf("DeleteVaultFolder empty tenantID expected error")
	}
	if err := s.DeleteVaultFolder(context.Background(), "t", ""); err == nil {
		t.Fatalf("DeleteVaultFolder empty folderID expected error")
	}
}

func TestSetFolderEncryptionMeta_RequiresIDs(t *testing.T) {
	s := nilPoolService()
	if _, err := s.SetFolderEncryptionMeta(context.Background(), "", "f", []byte("x"), "", []byte("n")); err == nil {
		t.Fatalf("SetFolderEncryptionMeta empty tenantID expected error")
	}
}

func TestNilPoolGuards(t *testing.T) {
	s := &VaultService{pool: nil}
	if _, err := s.CreateVaultFolder(context.Background(), Folder{TenantID: "t", UserID: "u", FolderName: "x"}); err == nil {
		t.Fatalf("CreateVaultFolder with nil pool expected error")
	}
	if _, err := s.GetVaultFolder(context.Background(), "t", "f"); err == nil {
		t.Fatalf("GetVaultFolder with nil pool expected error")
	}
	if err := s.DeleteVaultFolder(context.Background(), "t", "f"); err == nil {
		t.Fatalf("DeleteVaultFolder with nil pool expected error")
	}
	if _, err := s.SetFolderEncryptionMeta(context.Background(), "t", "f", []byte("x"), "", []byte("n")); err == nil {
		t.Fatalf("SetFolderEncryptionMeta with nil pool expected error")
	}
}
