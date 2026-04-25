package vault

import (
	"context"
	"strings"
	"testing"
)

func TestProtectedFolder_Validation(t *testing.T) {
	s := NewProtectedFolderService(nil)
	ctx := context.Background()

	if _, err := s.CreateProtectedFolder(ctx, ProtectedFolder{}); err == nil || !strings.Contains(err.Error(), "tenant_id required") {
		t.Errorf("create empty tenant = %v", err)
	}
	if _, err := s.CreateProtectedFolder(ctx, ProtectedFolder{TenantID: "t"}); err == nil || !strings.Contains(err.Error(), "owner_id required") {
		t.Errorf("create empty owner = %v", err)
	}
	if _, err := s.CreateProtectedFolder(ctx, ProtectedFolder{TenantID: "t", OwnerID: "o"}); err == nil || !strings.Contains(err.Error(), "folder_name required") {
		t.Errorf("create empty name = %v", err)
	}
	if _, err := s.ListProtectedFolders(ctx, "", ""); err == nil || !strings.Contains(err.Error(), "tenantID required") {
		t.Errorf("list empty tenant = %v", err)
	}
	if _, err := s.ShareFolder(ctx, "t", "f", "o", "", "read"); err == nil {
		t.Errorf("share empty grantee expected error")
	}
	if _, err := s.ShareFolder(ctx, "t", "f", "o", "g", "owner"); err == nil || !strings.Contains(err.Error(), "invalid permission") {
		t.Errorf("share invalid permission = %v", err)
	}
	if err := s.UnshareFolder(ctx, "t", "", "o", "g"); err == nil {
		t.Errorf("unshare empty folder expected error")
	}
	if _, err := s.ListFolderAccess(ctx, "", "f"); err == nil {
		t.Errorf("list-access empty tenant expected error")
	}
	if _, err := s.GetFolderAccessLog(ctx, "t", ""); err == nil {
		t.Errorf("access-log empty folder expected error")
	}
}

func TestProtectedFolder_NilPoolGuards(t *testing.T) {
	s := NewProtectedFolderService(nil)
	ctx := context.Background()
	if _, err := s.CreateProtectedFolder(ctx, ProtectedFolder{TenantID: "t", OwnerID: "o", FolderName: "n"}); err == nil {
		t.Errorf("Create nil-pool expected error")
	}
	if _, err := s.ShareFolder(ctx, "t", "f", "o", "g", "read"); err == nil {
		t.Errorf("Share nil-pool expected error")
	}
	if err := s.UnshareFolder(ctx, "t", "f", "o", "g"); err == nil {
		t.Errorf("Unshare nil-pool expected error")
	}
}
