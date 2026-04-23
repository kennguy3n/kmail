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
