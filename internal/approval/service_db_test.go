package approval

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func newDBService(t *testing.T) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return NewService(pool), tenant
}

func TestRequiresApprovalConfigDB(t *testing.T) {
	svc, tenant := newDBService(t)
	ctx := context.Background()

	// Unset action does not require approval.
	req, err := svc.RequiresApproval(ctx, tenant, "user_delete")
	if err != nil {
		t.Fatalf("RequiresApproval: %v", err)
	}
	if req {
		t.Error("unset action should not require approval")
	}

	if err := svc.SetActionConfig(ctx, tenant, "user_delete", true); err != nil {
		t.Fatalf("SetActionConfig: %v", err)
	}
	req, err = svc.RequiresApproval(ctx, tenant, "user_delete")
	if err != nil || !req {
		t.Fatalf("RequiresApproval after enable=%v err=%v", req, err)
	}

	// Toggling off updates in place (ON CONFLICT).
	if err := svc.SetActionConfig(ctx, tenant, "user_delete", false); err != nil {
		t.Fatalf("SetActionConfig off: %v", err)
	}
	cfg, err := svc.ListActionConfig(ctx, tenant)
	if err != nil {
		t.Fatalf("ListActionConfig: %v", err)
	}
	if cfg["user_delete"] != false {
		t.Errorf("config=%v want user_delete:false", cfg)
	}
}

func TestApproveLifecycleDB(t *testing.T) {
	svc, tenant := newDBService(t)
	ctx := context.Background()

	r, err := svc.CreateRequest(ctx, tenant, "requester-1", "domain_remove", "example.com")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if r.Status != StatusPending {
		t.Errorf("new request status=%s want pending", r.Status)
	}

	pending, err := svc.ListPendingRequests(ctx, tenant)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingRequests=%v err=%v", pending, err)
	}

	approved, err := svc.ApproveRequest(ctx, tenant, r.ID, "approver-9")
	if err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}
	if approved.Status != StatusApproved || approved.ApproverID != "approver-9" {
		t.Errorf("approved=%+v", approved)
	}
	if approved.ResolvedAt == nil {
		t.Error("resolved_at should be set after approval")
	}

	// Re-approving a non-pending request fails (status guard).
	if _, err := svc.ApproveRequest(ctx, tenant, r.ID, "approver-9"); err == nil {
		t.Error("re-approving resolved request should fail")
	}

	// No longer pending.
	pending, _ = svc.ListPendingRequests(ctx, tenant)
	if len(pending) != 0 {
		t.Errorf("want 0 pending after approve, got %d", len(pending))
	}
	all, err := svc.ListAll(ctx, tenant, 0)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAll=%v err=%v", all, err)
	}
}

func TestRejectWithReasonDB(t *testing.T) {
	svc, tenant := newDBService(t)
	ctx := context.Background()

	r, err := svc.CreateRequest(ctx, tenant, "req", "plan_downgrade", "")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	rejected, err := svc.RejectRequest(ctx, tenant, r.ID, "approver", "policy violation")
	if err != nil {
		t.Fatalf("RejectRequest: %v", err)
	}
	if rejected.Status != StatusRejected || rejected.Reason != "policy violation" {
		t.Errorf("rejected=%+v", rejected)
	}
}

func TestExecuteApprovedDB(t *testing.T) {
	svc, tenant := newDBService(t)
	ctx := context.Background()

	r, err := svc.CreateRequest(ctx, tenant, "req", "data_export", "tenant-data")
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	// Executing a not-yet-approved request fails.
	if err := svc.ExecuteApproved(ctx, tenant, r.ID); err == nil {
		t.Error("executing pending request should fail")
	}

	if _, err := svc.ApproveRequest(ctx, tenant, r.ID, "approver"); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}

	// Approved but no executor registered → ErrNoExecutor.
	if err := svc.ExecuteApproved(ctx, tenant, r.ID); !errors.Is(err, ErrNoExecutor) {
		t.Errorf("want ErrNoExecutor, got %v", err)
	}

	// Register an executor and confirm it runs with the request.
	var ran Request
	svc.RegisterExecutor("data_export", func(_ context.Context, req Request) error {
		ran = req
		return nil
	})
	if err := svc.ExecuteApproved(ctx, tenant, r.ID); err != nil {
		t.Fatalf("ExecuteApproved with executor: %v", err)
	}
	if ran.ID != r.ID || ran.Action != "data_export" {
		t.Errorf("executor got wrong request: %+v", ran)
	}
}
