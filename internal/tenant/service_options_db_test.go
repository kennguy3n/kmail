package tenant

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// --- stubs for the optional collaborators --------------------------

type stubSeatAccounter struct {
	checkErr  error
	increments int
}

func (s *stubSeatAccounter) CheckSeatAvailable(ctx context.Context, tenantID string) error {
	return s.checkErr
}
func (s *stubSeatAccounter) IncrementSeatCount(ctx context.Context, tenantID string, delta int) error {
	s.increments += delta
	return nil
}

type stubProvisioner struct {
	called bool
	err    error
}

func (p *stubProvisioner) Provision(ctx context.Context, tenantID, plan string) (*StorageCredential, error) {
	p.called = true
	if p.err != nil {
		return nil, p.err
	}
	return &StorageCredential{TenantID: tenantID}, nil
}

type stubBilling struct {
	created, deleted bool
}

func (b *stubBilling) OnTenantCreated(ctx context.Context, tenantID, plan string) error {
	b.created = true
	return nil
}
func (b *stubBilling) OnTenantDeleted(ctx context.Context, tenantID string) error {
	b.deleted = true
	return nil
}

// TestServiceHooksDB exercises the optional StorageProvisioner and
// BillingLifecycleHook wired via the With* setters: both fire on
// CreateTenant, the billing hook fires on DeleteTenant, and a
// provisioner failure surfaces (with the tenant preserved).
func TestServiceHooksDB(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	u := uniq()

	prov := &stubProvisioner{}
	bill := &stubBilling{}
	svc := NewService(pool).
		WithStorageProvisioner(prov).
		WithBillingLifecycle(bill).
		WithLogger(log.New(io.Discard, "", 0))

	tn, err := svc.CreateTenant(ctx, CreateTenantInput{Name: "Hk " + u, Slug: "hk-" + u, Plan: "pro"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE tenant_id = $1::uuid`, tn.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, tn.ID)
	})
	if !prov.called || !bill.created {
		t.Errorf("hooks not invoked: prov=%v billing=%v", prov.called, bill.created)
	}

	if err := svc.DeleteTenant(ctx, tn.ID); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
	if !bill.deleted {
		t.Error("billing OnTenantDeleted not invoked")
	}

	// A failing provisioner surfaces a wrapped error but still returns
	// the created tenant so the caller can re-drive idempotent hooks.
	failProv := &stubProvisioner{err: errors.New("fabric down")}
	failSvc := NewService(pool).WithStorageProvisioner(failProv)
	tn2, err := failSvc.CreateTenant(ctx, CreateTenantInput{Name: "Fp " + u, Slug: "fp-" + u, Plan: "pro"})
	if err == nil {
		t.Error("expected provisioner failure to surface")
	}
	if tn2 == nil {
		t.Error("tenant should be preserved despite provisioner failure")
	} else {
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, tn2.ID)
		})
	}
}

// TestServiceSeatAccounterDB exercises the seat-enforcement path:
// CreateUser checks availability and increments the counter, and a
// full tenant is rejected.
func TestServiceSeatAccounterDB(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	u := uniq()
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE tenant_id = $1::uuid`, tenant)
	})

	seats := &stubSeatAccounter{}
	svc := NewService(pool).WithSeatAccounter(seats)
	if _, err := svc.CreateUser(ctx, tenant, CreateUserInput{
		KChatUserID: "kc-" + u, StalwartAccountID: "sw-" + u,
		Email: "seat-" + u + "@example.com", DisplayName: "Seat", Role: "member",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if seats.increments != 1 {
		t.Errorf("increments=%d want 1", seats.increments)
	}

	// A full tenant rejects the next user before the INSERT.
	full := &stubSeatAccounter{checkErr: errors.New("seat limit reached")}
	svcFull := NewService(pool).WithSeatAccounter(full)
	if _, err := svcFull.CreateUser(ctx, tenant, CreateUserInput{
		KChatUserID: "kc2-" + u, StalwartAccountID: "sw2-" + u,
		Email: "seat2-" + u + "@example.com", DisplayName: "Seat2", Role: "member",
	}); err == nil {
		t.Error("expected seat-limit rejection")
	}
}

// TestSharedInboxMembershipHookDB verifies the post-mutation MLS hook
// fires on add/remove with the refreshed member set (which exercises
// listSharedInboxMemberIDs).
func TestSharedInboxMembershipHookDB(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	u := uniq()
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM shared_inbox_members WHERE tenant_id = $1::uuid`, tenant)
		_, _ = pool.Exec(context.Background(), `DELETE FROM shared_inboxes WHERE tenant_id = $1::uuid`, tenant)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE tenant_id = $1::uuid`, tenant)
	})

	type hookCall struct {
		members []string
		reason  string
	}
	var calls []hookCall
	svc := NewService(pool).WithSharedInboxMembershipHook(
		func(ctx context.Context, tenantID, inboxID string, members []string, reason string) {
			calls = append(calls, hookCall{members: members, reason: reason})
		},
	)

	usr, err := svc.CreateUser(ctx, tenant, CreateUserInput{
		KChatUserID: "kc-" + u, StalwartAccountID: "sw-" + u,
		Email: "m-" + u + "@example.com", DisplayName: "M", Role: "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	inbox, err := svc.CreateSharedInbox(ctx, tenant, CreateSharedInboxInput{
		Address: "team-" + u + "@example.com", DisplayName: "Team", MLSGroupID: "mls-" + u,
	})
	if err != nil {
		t.Fatalf("CreateSharedInbox: %v", err)
	}

	if _, err := svc.AddSharedInboxMember(ctx, tenant, inbox.ID, usr.ID, "member"); err != nil {
		t.Fatalf("AddSharedInboxMember: %v", err)
	}
	if err := svc.RemoveSharedInboxMember(ctx, tenant, inbox.ID, usr.ID); err != nil {
		t.Fatalf("RemoveSharedInboxMember: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("hook calls=%d want 2: %+v", len(calls), calls)
	}
	if calls[0].reason != "member_added" || len(calls[0].members) != 1 || calls[0].members[0] != usr.ID {
		t.Errorf("add hook=%+v", calls[0])
	}
	if calls[1].reason != "member_removed" || len(calls[1].members) != 0 {
		t.Errorf("remove hook=%+v", calls[1])
	}
}
