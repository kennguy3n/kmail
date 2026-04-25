package approval

import (
	"context"
	"strings"
	"testing"
)

// TestResolveMethods_RequireTenantID is the regression guard for
// PR #21 Devin Review #1: ApproveRequest / RejectRequest /
// ExecuteApproved used to look up rows by UUID alone, letting any
// caller resolve another tenant's pending request. The signature
// now requires a tenantID, and all three methods must reject an
// empty value before touching the pool.
func TestResolveMethods_RequireTenantID(t *testing.T) {
	s := &Service{pool: nil}
	approvalID := "00000000-0000-0000-0000-000000000000"

	if _, err := s.ApproveRequest(context.Background(), "", approvalID, "approver"); err == nil || !strings.Contains(err.Error(), "tenantID required") {
		t.Errorf("ApproveRequest empty tenantID = %v, want tenantID required", err)
	}
	if _, err := s.RejectRequest(context.Background(), "", approvalID, "approver", ""); err == nil || !strings.Contains(err.Error(), "tenantID required") {
		t.Errorf("RejectRequest empty tenantID = %v, want tenantID required", err)
	}
	if err := s.ExecuteApproved(context.Background(), "", approvalID); err == nil || !strings.Contains(err.Error(), "tenantID required") {
		t.Errorf("ExecuteApproved empty tenantID = %v, want tenantID required", err)
	}
}
