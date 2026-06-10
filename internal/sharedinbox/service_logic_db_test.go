package sharedinbox

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestServiceValidationGuards pins the ErrInvalidInput guards on every
// public method (no DB round-trip needed for the empty-arg paths).
func TestServiceValidationGuards(t *testing.T) {
	svc := NewService(nil, log.New(io.Discard, "", 0))
	ctx := context.Background()

	if _, err := svc.AssignEmail(ctx, "", "i", "e", "u"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("AssignEmail empty tenant: %v", err)
	}
	if err := svc.UnassignEmail(ctx, "t", "", "e"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("UnassignEmail empty inbox: %v", err)
	}
	if _, err := svc.SetStatus(ctx, "t", "i", "e", "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetStatus bad status: %v", err)
	}
	if _, err := svc.ListAssignments(ctx, "", "i", ListAssignmentsOptions{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ListAssignments empty tenant: %v", err)
	}
	if _, err := svc.AddNote(ctx, "t", "i", "e", "", "text"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("AddNote empty author: %v", err)
	}
	if _, err := svc.AddNote(ctx, "t", "i", "e", "u", ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("AddNote empty text: %v", err)
	}
	if _, err := svc.ListNotes(ctx, "t", "", "e"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ListNotes empty inbox: %v", err)
	}
}

// TestUnassignNotFound verifies the ErrNotFound path when no assignment
// row exists for the email.
func TestUnassignNotFound(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	inbox, _ := seedInboxAndUser(t, svc, tenant)

	err := svc.UnassignEmail(context.Background(), tenant, inbox, "never-assigned")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UnassignEmail on missing row = %v, want ErrNotFound", err)
	}
}

// TestListAssignmentsFilters exercises the status + assignee_user_id
// WHERE-clause branches and the limit clamping.
func TestListAssignmentsFilters(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	inbox, user := seedInboxAndUser(t, svc, tenant)
	ctx := context.Background()

	if _, err := svc.AssignEmail(ctx, tenant, inbox, "email-f1", user); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := svc.SetStatus(ctx, tenant, inbox, "email-f1", StatusInProgress); err != nil {
		t.Fatalf("set status: %v", err)
	}

	// Status filter.
	got, err := svc.ListAssignments(ctx, tenant, inbox, ListAssignmentsOptions{Status: StatusInProgress})
	if err != nil {
		t.Fatalf("list (status): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("status filter = %d rows, want 1", len(got))
	}

	// Assignee filter + over-cap limit (clamped to 500).
	got, err = svc.ListAssignments(ctx, tenant, inbox, ListAssignmentsOptions{AssigneeUserID: user, Limit: 9000})
	if err != nil {
		t.Fatalf("list (assignee): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("assignee filter = %d rows, want 1", len(got))
	}
}
