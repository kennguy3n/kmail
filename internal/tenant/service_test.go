package tenant

import (
	"context"
	"errors"
	"testing"
)

// The validation paths in every Create* method and GetTenant return
// before touching the pool, so a nil-pool Service is sufficient for
// input-validation tests.

func nilService() *Service { return &Service{pool: nil} }

func TestCreateTenant_EmptyName(t *testing.T) {
	_, err := nilService().CreateTenant(context.Background(), CreateTenantInput{
		Name: "",
		Slug: "slug",
		Plan: "core",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateTenant_EmptySlug(t *testing.T) {
	_, err := nilService().CreateTenant(context.Background(), CreateTenantInput{
		Name: "Acme",
		Slug: "",
		Plan: "core",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateTenant_EmptyPlan(t *testing.T) {
	_, err := nilService().CreateTenant(context.Background(), CreateTenantInput{
		Name: "Acme",
		Slug: "acme",
		Plan: "",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateTenant_AllEmpty(t *testing.T) {
	_, err := nilService().CreateTenant(context.Background(), CreateTenantInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateUser_MissingRequiredFields(t *testing.T) {
	tests := []struct {
		name  string
		input CreateUserInput
	}{
		{name: "empty kchat_user_id", input: CreateUserInput{
			StalwartAccountID: "sa", Email: "a@b", DisplayName: "A",
		}},
		{name: "empty stalwart_account_id", input: CreateUserInput{
			KChatUserID: "ku", Email: "a@b", DisplayName: "A",
		}},
		{name: "empty email", input: CreateUserInput{
			KChatUserID: "ku", StalwartAccountID: "sa", DisplayName: "A",
		}},
		{name: "empty display_name", input: CreateUserInput{
			KChatUserID: "ku", StalwartAccountID: "sa", Email: "a@b",
		}},
		{name: "all empty", input: CreateUserInput{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nilService().CreateUser(context.Background(), "tid", tc.input)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestCreateDomain_EmptyDomain(t *testing.T) {
	_, err := nilService().CreateDomain(context.Background(), "tid", CreateDomainInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateSharedInbox_MissingFields(t *testing.T) {
	tests := []struct {
		name  string
		input CreateSharedInboxInput
	}{
		{name: "empty address", input: CreateSharedInboxInput{
			DisplayName: "Sales", MLSGroupID: "g1",
		}},
		{name: "empty display_name", input: CreateSharedInboxInput{
			Address: "sales@acme.com", MLSGroupID: "g1",
		}},
		{name: "empty mls_group_id", input: CreateSharedInboxInput{
			Address: "sales@acme.com", DisplayName: "Sales",
		}},
		{name: "all empty", input: CreateSharedInboxInput{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nilService().CreateSharedInbox(context.Background(), "tid", tc.input)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestGetTenant_RequiresDB(t *testing.T) {
	// GetTenant hits the pool immediately (no pre-validation),
	// so without a real database it will panic or error. We
	// verify the method exists and the nil-pool path panics to
	// confirm we're exercising the right code path.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic from nil pool on GetTenant")
		}
	}()
	_, _ = nilService().GetTenant(context.Background(), "00000000-0000-0000-0000-000000000000")
}

// ---------------------------------------------------------------
// UpdateTenant validation
// ---------------------------------------------------------------

func TestUpdateTenant_EmptyID(t *testing.T) {
	_, err := nilService().UpdateTenant(context.Background(), "", UpdateTenantInput{
		Name: strPtr("Acme"),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateTenant_NoFields(t *testing.T) {
	_, err := nilService().UpdateTenant(context.Background(), "tid", UpdateTenantInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateTenant_EmptyName(t *testing.T) {
	empty := ""
	_, err := nilService().UpdateTenant(context.Background(), "tid", UpdateTenantInput{
		Name: &empty,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateTenant_InvalidPlan(t *testing.T) {
	bad := "enterprise"
	_, err := nilService().UpdateTenant(context.Background(), "tid", UpdateTenantInput{
		Plan: &bad,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateTenant_ValidPlans(t *testing.T) {
	// A valid plan should pass validation and proceed to the pool,
	// which is nil in this test — so we expect either a non-
	// ErrInvalidInput error or a panic. The only failure mode is
	// ErrInvalidInput, which would mean validation rejected a value
	// the allowlist says is valid.
	for _, plan := range []string{"core", "pro", "privacy"} {
		p := plan
		assertNotInvalidInput(t, plan, func() error {
			_, err := nilService().UpdateTenant(context.Background(), "tid", UpdateTenantInput{
				Plan: &p,
			})
			return err
		})
	}
}

func TestUpdateTenant_InvalidStatus(t *testing.T) {
	bad := "archived"
	_, err := nilService().UpdateTenant(context.Background(), "tid", UpdateTenantInput{
		Status: &bad,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateTenant_ValidStatuses(t *testing.T) {
	for _, status := range []string{"active", "suspended", "deleted"} {
		s := status
		assertNotInvalidInput(t, status, func() error {
			_, err := nilService().UpdateTenant(context.Background(), "tid", UpdateTenantInput{
				Status: &s,
			})
			return err
		})
	}
}

// ---------------------------------------------------------------
// DeleteTenant validation
// ---------------------------------------------------------------

func TestDeleteTenant_EmptyID(t *testing.T) {
	err := nilService().DeleteTenant(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

// ---------------------------------------------------------------
// ListUsers validation
// ---------------------------------------------------------------

func TestListUsers_EmptyTenantID(t *testing.T) {
	_, err := nilService().ListUsers(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

// ---------------------------------------------------------------
// GetUser validation
// ---------------------------------------------------------------

func TestGetUser_EmptyIDs(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		userID   string
	}{
		{"empty tenant", "", "uid"},
		{"empty user", "tid", ""},
		{"both empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nilService().GetUser(context.Background(), tc.tenantID, tc.userID)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------
// UpdateUser validation
// ---------------------------------------------------------------

func TestUpdateUser_EmptyIDs(t *testing.T) {
	_, err := nilService().UpdateUser(context.Background(), "", "uid", UpdateUserInput{
		DisplayName: strPtr("A"),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty tenantID, got %v", err)
	}
	_, err = nilService().UpdateUser(context.Background(), "tid", "", UpdateUserInput{
		DisplayName: strPtr("A"),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty userID, got %v", err)
	}
}

func TestUpdateUser_NoFields(t *testing.T) {
	_, err := nilService().UpdateUser(context.Background(), "tid", "uid", UpdateUserInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateUser_EmptyDisplayName(t *testing.T) {
	empty := ""
	_, err := nilService().UpdateUser(context.Background(), "tid", "uid", UpdateUserInput{
		DisplayName: &empty,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateUser_InvalidRole(t *testing.T) {
	bad := "superadmin"
	_, err := nilService().UpdateUser(context.Background(), "tid", "uid", UpdateUserInput{
		Role: &bad,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateUser_ValidRoles(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member", "billing", "deliverability"} {
		r := role
		assertNotInvalidInput(t, role, func() error {
			_, err := nilService().UpdateUser(context.Background(), "tid", "uid", UpdateUserInput{
				Role: &r,
			})
			return err
		})
	}
}

func TestUpdateUser_InvalidStatus(t *testing.T) {
	bad := "banned"
	_, err := nilService().UpdateUser(context.Background(), "tid", "uid", UpdateUserInput{
		Status: &bad,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestUpdateUser_NegativeQuota(t *testing.T) {
	neg := int64(-1)
	_, err := nilService().UpdateUser(context.Background(), "tid", "uid", UpdateUserInput{
		QuotaBytes: &neg,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

// ---------------------------------------------------------------
// DeleteUser validation
// ---------------------------------------------------------------

func TestDeleteUser_EmptyIDs(t *testing.T) {
	err := nilService().DeleteUser(context.Background(), "", "uid")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty tenantID, got %v", err)
	}
	err = nilService().DeleteUser(context.Background(), "tid", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty userID, got %v", err)
	}
}

// ---------------------------------------------------------------
// GetDomain validation
// ---------------------------------------------------------------

func TestGetDomain_EmptyIDs(t *testing.T) {
	_, err := nilService().GetDomain(context.Background(), "", "did")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty tenantID, got %v", err)
	}
	_, err = nilService().GetDomain(context.Background(), "tid", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty domainID, got %v", err)
	}
}

// ---------------------------------------------------------------
// ListSharedInboxes / AddSharedInboxMember / RemoveSharedInboxMember
// ---------------------------------------------------------------

func TestListSharedInboxes_EmptyTenantID(t *testing.T) {
	_, err := nilService().ListSharedInboxes(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestAddSharedInboxMember_MissingIDs(t *testing.T) {
	cases := []struct {
		name                        string
		tenantID, inboxID, userID   string
	}{
		{"empty tenant", "", "iid", "uid"},
		{"empty inbox", "tid", "", "uid"},
		{"empty user", "tid", "iid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nilService().AddSharedInboxMember(context.Background(), tc.tenantID, tc.inboxID, tc.userID, "member")
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestAddSharedInboxMember_InvalidRole(t *testing.T) {
	_, err := nilService().AddSharedInboxMember(context.Background(), "tid", "iid", "uid", "superuser")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for bad role, got %v", err)
	}
}

func TestAddSharedInboxMember_ValidRoles(t *testing.T) {
	for _, role := range []string{"", "owner", "member", "viewer"} {
		r := role
		assertNotInvalidInput(t, role, func() error {
			_, err := nilService().AddSharedInboxMember(context.Background(), "tid", "iid", "uid", r)
			return err
		})
	}
}

func TestRemoveSharedInboxMember_MissingIDs(t *testing.T) {
	cases := []struct {
		name                        string
		tenantID, inboxID, userID   string
	}{
		{"empty tenant", "", "iid", "uid"},
		{"empty inbox", "tid", "", "uid"},
		{"empty user", "tid", "iid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nilService().RemoveSharedInboxMember(context.Background(), tc.tenantID, tc.inboxID, tc.userID)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------
// helpers
// ---------------------------------------------------------------

func strPtr(s string) *string { return &s }

// assertNotInvalidInput runs `fn` and fails the test only if `fn`
// returns `ErrInvalidInput`. Panics from downstream nil-pool use
// are caught and treated as "validation passed" — the only thing
// these tests care about is that the input was accepted by the
// validation allowlist.
func assertNotInvalidInput(t *testing.T, label string, fn func() error) {
	t.Helper()
	defer func() { _ = recover() }()
	if err := fn(); errors.Is(err, ErrInvalidInput) {
		t.Errorf("%q unexpectedly rejected as invalid input: %v", label, err)
	}
}
