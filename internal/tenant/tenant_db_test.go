package tenant

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func uniq() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func TestTenantCRUDLifecycleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	ctx := context.Background()
	u := uniq()

	// CreateTenant
	tn, err := svc.CreateTenant(ctx, CreateTenantInput{Name: "Acme " + u, Slug: "acme-" + u, Plan: "pro"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tn.ID == "" || tn.Status == "" {
		t.Fatalf("tenant not persisted: %+v", tn)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1::uuid`, tn.ID)
	})

	// GetTenant
	got, err := svc.GetTenant(ctx, tn.ID)
	if err != nil || got.Slug != tn.Slug {
		t.Fatalf("GetTenant: %v got=%+v", err, got)
	}

	// GetTenant not found
	if _, err := svc.GetTenant(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTenant missing: want ErrNotFound got %v", err)
	}

	// ListTenants includes ours
	list, err := svc.ListTenants(ctx)
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == tn.ID {
			found = true
		}
	}
	if !found {
		t.Error("ListTenants did not include created tenant")
	}

	// UpdateTenant
	newName := "Acme Renamed"
	newStatus := "suspended"
	upd, err := svc.UpdateTenant(ctx, tn.ID, UpdateTenantInput{Name: &newName, Status: &newStatus})
	if err != nil {
		t.Fatalf("UpdateTenant: %v", err)
	}
	if upd.Name != newName || upd.Status != newStatus {
		t.Errorf("UpdateTenant result wrong: %+v", upd)
	}
}

func TestTenantAliasesDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	ctx := context.Background()
	u := uniq()

	tn, err := svc.CreateTenant(ctx, CreateTenantInput{Name: "Al " + u, Slug: "al-" + u, Plan: "pro"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1::uuid`, tn.ID)
	})
	usr, err := svc.CreateUser(ctx, tn.ID, CreateUserInput{
		KChatUserID: "kc-" + u, StalwartAccountID: "sw-" + u,
		Email: "owner-" + u + "@example.com", DisplayName: "Owner",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// invalid alias address rejected
	if _, err := svc.CreateAlias(ctx, tn.ID, CreateAliasInput{UserID: usr.ID, AliasEmail: "not-an-email"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad alias: want ErrInvalidInput got %v", err)
	}
	// unknown user rejected
	if _, err := svc.CreateAlias(ctx, tn.ID, CreateAliasInput{
		UserID: "00000000-0000-0000-0000-000000000000", AliasEmail: "x-" + u + "@example.com",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: want ErrNotFound got %v", err)
	}

	a, err := svc.CreateAlias(ctx, tn.ID, CreateAliasInput{UserID: usr.ID, AliasEmail: "Alias-" + u + "@Example.com"})
	if err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	if a.AliasEmail != "alias-"+u+"@example.com" {
		t.Errorf("alias not normalized: %q", a.AliasEmail)
	}

	// duplicate alias rejected
	if _, err := svc.CreateAlias(ctx, tn.ID, CreateAliasInput{UserID: usr.ID, AliasEmail: "alias-" + u + "@example.com"}); !errors.Is(err, ErrAliasInUse) {
		t.Errorf("dup alias: want ErrAliasInUse got %v", err)
	}

	if all, err := svc.ListAliases(ctx, tn.ID); err != nil || len(all) != 1 {
		t.Fatalf("ListAliases=%d err=%v", len(all), err)
	}
	if mine, err := svc.ListUserAliases(ctx, tn.ID, usr.ID); err != nil || len(mine) != 1 {
		t.Fatalf("ListUserAliases=%d err=%v", len(mine), err)
	}

	if err := svc.DeleteAlias(ctx, tn.ID, a.ID); err != nil {
		t.Fatalf("DeleteAlias: %v", err)
	}
	if err := svc.DeleteAlias(ctx, tn.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteAlias twice: want ErrNotFound got %v", err)
	}
}

func TestTenantUsersDomainsInboxesDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	ctx := context.Background()
	u := uniq()

	tn, err := svc.CreateTenant(ctx, CreateTenantInput{Name: "Co " + u, Slug: "co-" + u, Plan: "pro"})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id=$1::uuid`, tn.ID)
	})

	// CreateUser
	usr, err := svc.CreateUser(ctx, tn.ID, CreateUserInput{
		KChatUserID:       "kc-" + u,
		StalwartAccountID: "sw-" + u,
		Email:             "a-" + u + "@example.com",
		DisplayName:       "Alice",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if usr.Role != "member" || usr.AccountType != "user" {
		t.Errorf("user defaults wrong: %+v", usr)
	}

	// invalid account type
	if _, err := svc.CreateUser(ctx, tn.ID, CreateUserInput{
		KChatUserID: "k2-" + u, StalwartAccountID: "s2-" + u,
		Email: "b@x.com", DisplayName: "B", AccountType: "robot",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad account_type: want ErrInvalidInput got %v", err)
	}

	// ListUsers / GetUser
	users, err := svc.ListUsers(ctx, tn.ID)
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers=%d err=%v", len(users), err)
	}
	gu, err := svc.GetUser(ctx, tn.ID, usr.ID)
	if err != nil || gu.Email != usr.Email {
		t.Fatalf("GetUser: %v %+v", err, gu)
	}

	// UpdateUser
	dn := "Alice B"
	role := "admin"
	uu, err := svc.UpdateUser(ctx, tn.ID, usr.ID, UpdateUserInput{DisplayName: &dn, Role: &role})
	if err != nil || uu.DisplayName != dn || uu.Role != role {
		t.Fatalf("UpdateUser: %v %+v", err, uu)
	}

	// Domains
	dom, err := svc.CreateDomain(ctx, tn.ID, CreateDomainInput{Domain: "mail-" + u + ".example.com"})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	doms, err := svc.ListDomains(ctx, tn.ID)
	if err != nil || len(doms) != 1 {
		t.Fatalf("ListDomains=%d err=%v", len(doms), err)
	}
	if gd, err := svc.GetDomain(ctx, tn.ID, dom.ID); err != nil || gd.Domain != dom.Domain {
		t.Fatalf("GetDomain: %v %+v", err, gd)
	}

	// Shared inbox + membership
	si, err := svc.CreateSharedInbox(ctx, tn.ID, CreateSharedInboxInput{
		Address: "team-" + u + "@example.com", DisplayName: "Team", MLSGroupID: "mls-" + u,
	})
	if err != nil {
		t.Fatalf("CreateSharedInbox: %v", err)
	}
	inboxes, err := svc.ListSharedInboxes(ctx, tn.ID)
	if err != nil || len(inboxes) != 1 {
		t.Fatalf("ListSharedInboxes=%d err=%v", len(inboxes), err)
	}
	if _, err := svc.AddSharedInboxMember(ctx, tn.ID, si.ID, usr.ID, "owner"); err != nil {
		t.Fatalf("AddSharedInboxMember: %v", err)
	}
	if err := svc.RemoveSharedInboxMember(ctx, tn.ID, si.ID, usr.ID); err != nil {
		t.Fatalf("RemoveSharedInboxMember: %v", err)
	}
	if err := svc.RemoveSharedInboxMember(ctx, tn.ID, si.ID, usr.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveSharedInboxMember twice: want ErrNotFound got %v", err)
	}

	// DeleteUser is a soft delete (status -> "deleted").
	if err := svc.DeleteUser(ctx, tn.ID, usr.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if gu, err := svc.GetUser(ctx, tn.ID, usr.ID); err != nil || gu.Status != "deleted" {
		t.Errorf("after soft delete: err=%v status=%q want deleted", err, gu.Status)
	}
	// Deleting again returns ErrNotFound (already deleted).
	if err := svc.DeleteUser(ctx, tn.ID, usr.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteUser twice: want ErrNotFound got %v", err)
	}
}
