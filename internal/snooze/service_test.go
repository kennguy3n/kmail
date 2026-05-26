package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Validation paths in Snooze fire before touching the pool, so a
// nil-pool Service is sufficient for input-validation tests.
// Mirrors scheduledsend.nilService.

func nilService(nowFunc func() time.Time) *Service {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Service{pool: nil, now: nowFunc}
}

func validInput(now time.Time) SnoozeInput {
	return SnoozeInput{
		TenantID:           "tenant-a",
		KChatUserID:        "kchat-a",
		StalwartAccountID:  "acct-a",
		EmailID:            "email-1",
		OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
		SnoozedMailboxID:   "mb-snoozed",
		SnoozeUntil:        now.Add(2 * time.Minute),
		MarkUnreadOnWake:   true,
	}
}

func TestNewService_RejectsNilPool(t *testing.T) {
	if _, err := NewService(Config{Pool: nil}); err == nil {
		t.Fatalf("expected error for nil Pool")
	}
}

func TestSnooze_MissingTenantID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.TenantID = ""
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for missing TenantID")
	}
}

func TestSnooze_MissingKChatUserID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.KChatUserID = ""
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for missing KChatUserID")
	}
}

func TestSnooze_MissingStalwartAccountID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.StalwartAccountID = ""
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for missing StalwartAccountID")
	}
}

func TestSnooze_MissingEmailID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.EmailID = ""
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for missing EmailID")
	}
}

func TestSnooze_MissingOriginalMailboxIDs(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.OriginalMailboxIDs = nil
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for missing OriginalMailboxIDs")
	}
}

func TestSnooze_MalformedOriginalMailboxIDs(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.OriginalMailboxIDs = json.RawMessage(`"not an object"`)
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for malformed OriginalMailboxIDs")
	}
}

func TestSnooze_EmptyOriginalMailboxMap(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.OriginalMailboxIDs = json.RawMessage(`{}`)
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for empty OriginalMailboxIDs map")
	}
}

func TestSnooze_MissingSnoozedMailboxID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SnoozedMailboxID = ""
	if _, err := svc.Snooze(context.Background(), in); err == nil {
		t.Fatalf("expected error for missing SnoozedMailboxID")
	}
}

func TestSnooze_ZeroSnoozeUntil(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SnoozeUntil = time.Time{}
	if _, err := svc.Snooze(context.Background(), in); !errors.Is(err, ErrInvalidSnooze) {
		t.Fatalf("expected ErrInvalidSnooze, got %v", err)
	}
}

func TestSnooze_SnoozeUntilTooSoon(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SnoozeUntil = now.Add(30 * time.Second)
	if _, err := svc.Snooze(context.Background(), in); !errors.Is(err, ErrInvalidSnooze) {
		t.Fatalf("expected ErrInvalidSnooze for sub-minute horizon, got %v", err)
	}
}

func TestSnooze_SnoozeUntilTooFar(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SnoozeUntil = now.Add(400 * 24 * time.Hour)
	if _, err := svc.Snooze(context.Background(), in); !errors.Is(err, ErrInvalidSnooze) {
		t.Fatalf("expected ErrInvalidSnooze for >1y horizon, got %v", err)
	}
}

func TestSnooze_BoundaryMin(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SnoozeUntil = now.Add(MinSnoozeHorizon)
	if err := svc.validateSnooze(in); err != nil {
		t.Fatalf("snooze at exact MinSnoozeHorizon should be valid, got %v", err)
	}
}

func TestGet_RejectsEmptyTenant(t *testing.T) {
	svc := nilService(nil)
	if _, err := svc.Get(context.Background(), "", "user-a", "some-id"); err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
}

// TestGet_RejectsEmptyUser pins the per-user authz guard at the
// Service layer: a zero-value kchatUserID must be rejected
// BEFORE the SQL fires so an inadvertent fall-through to
// tenant-only scoping can never widen the row visibility.
func TestGet_RejectsEmptyUser(t *testing.T) {
	svc := nilService(nil)
	if _, err := svc.Get(context.Background(), "tenant-a", "", "some-id"); err == nil {
		t.Fatalf("expected error for empty kchatUserID")
	}
}

func TestGet_EmptyIDReturnsNotFound(t *testing.T) {
	svc := nilService(nil)
	_, err := svc.Get(context.Background(), "tenant-a", "user-a", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCancel_RejectsEmptyTenant(t *testing.T) {
	svc := nilService(nil)
	if err := svc.Cancel(context.Background(), "", "user-a", "some-id"); err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
}

// TestCancel_RejectsEmptyUser — same shape as the Get test.
func TestCancel_RejectsEmptyUser(t *testing.T) {
	svc := nilService(nil)
	if err := svc.Cancel(context.Background(), "tenant-a", "", "some-id"); err == nil {
		t.Fatalf("expected error for empty kchatUserID")
	}
}

func TestCancel_EmptyIDReturnsNotFound(t *testing.T) {
	svc := nilService(nil)
	err := svc.Cancel(context.Background(), "tenant-a", "user-a", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListByUser_RejectsEmptyTenant(t *testing.T) {
	svc := nilService(nil)
	if _, err := svc.ListByUser(context.Background(), "", "kchat-a"); err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
}

func TestListByUser_RejectsEmptyKChatUserID(t *testing.T) {
	svc := nilService(nil)
	if _, err := svc.ListByUser(context.Background(), "tenant-a", ""); err == nil {
		t.Fatalf("expected error for empty kchatUserID")
	}
}

func TestIsUniqueViolation_DetectsSQLSTATE(t *testing.T) {
	if !isUniqueViolation(errors.New("ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)")) {
		t.Fatalf("expected SQLSTATE 23505 to be detected")
	}
	if !isUniqueViolation(errors.New("duplicate key value violates unique constraint \"x\"")) {
		t.Fatalf("expected duplicate-key message to be detected")
	}
	if isUniqueViolation(errors.New("connection reset")) {
		t.Fatalf("unrelated error must not be classified as unique violation")
	}
}
