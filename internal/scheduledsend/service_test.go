package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The validation paths in Schedule fire before touching the
// pool, so a nil-pool Service is sufficient for input-validation
// tests. This mirrors the same pattern used in
// `internal/tenant/service_test.go`.

func nilService(nowFunc func() time.Time) *Service {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Service{pool: nil, now: nowFunc}
}

func validInput(now time.Time) ScheduleInput {
	return ScheduleInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		IdentityID:        "ident-1",
		SubmissionPayload: json.RawMessage(`{"emailId":"email-1","identityId":"ident-1"}`),
		SendAt:            now.Add(2 * time.Minute),
	}
}

func TestSchedule_MissingTenantID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.TenantID = ""
	_, err := svc.Schedule(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for missing TenantID")
	}
}

func TestSchedule_MissingKChatUserID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.KChatUserID = ""
	_, err := svc.Schedule(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for missing KChatUserID")
	}
}

func TestSchedule_MissingEmailID(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.EmailID = ""
	_, err := svc.Schedule(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for missing EmailID")
	}
}

func TestSchedule_MissingSubmissionPayload(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SubmissionPayload = nil
	_, err := svc.Schedule(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for missing SubmissionPayload")
	}
}

func TestSchedule_ZeroSendAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	in.SendAt = time.Time{}
	_, err := svc.Schedule(context.Background(), in)
	if !errors.Is(err, ErrInvalidSchedule) {
		t.Fatalf("expected ErrInvalidSchedule, got %v", err)
	}
}

func TestSchedule_SendAtTooSoon(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	// 30s in the future — below MinScheduleHorizon (1m).
	in.SendAt = now.Add(30 * time.Second)
	_, err := svc.Schedule(context.Background(), in)
	if !errors.Is(err, ErrInvalidSchedule) {
		t.Fatalf("expected ErrInvalidSchedule for sub-minute horizon, got %v", err)
	}
}

func TestSchedule_SendAtTooFar(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	// 400 days out — above MaxScheduleHorizon (365 days).
	in.SendAt = now.Add(400 * 24 * time.Hour)
	_, err := svc.Schedule(context.Background(), in)
	if !errors.Is(err, ErrInvalidSchedule) {
		t.Fatalf("expected ErrInvalidSchedule for >1y horizon, got %v", err)
	}
}

func TestSchedule_SendAtBoundaryMin(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := nilService(func() time.Time { return now })
	in := validInput(now)
	// Exactly MinScheduleHorizon — should pass validation. We
	// can't actually proceed to the DB insert (pool is nil), so
	// we exercise validateSchedule directly: a boundary value
	// must not be classified as ErrInvalidSchedule.
	in.SendAt = now.Add(MinScheduleHorizon)
	if err := svc.validateSchedule(in); err != nil {
		t.Fatalf("send_at at exact MinScheduleHorizon should be valid, got %v", err)
	}
}

func TestNewService_RejectsNilPool(t *testing.T) {
	_, err := NewService(Config{Pool: nil})
	if err == nil {
		t.Fatalf("expected error for nil Pool")
	}
}

func TestGet_RejectsEmptyTenant(t *testing.T) {
	svc := nilService(nil)
	_, err := svc.Get(context.Background(), "", "kchat-a", "some-id")
	if err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
}

func TestGet_RejectsEmptyUser(t *testing.T) {
	svc := nilService(nil)
	_, err := svc.Get(context.Background(), "tenant-a", "", "some-id")
	if err == nil {
		t.Fatalf("expected error for empty kchatUserID (per-user authz fence)")
	}
}

func TestGet_EmptyIDReturnsNotFound(t *testing.T) {
	svc := nilService(nil)
	_, err := svc.Get(context.Background(), "tenant-a", "kchat-a", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCancel_RejectsEmptyTenant(t *testing.T) {
	svc := nilService(nil)
	err := svc.Cancel(context.Background(), "", "kchat-a", "some-id")
	if err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
}

func TestCancel_RejectsEmptyUser(t *testing.T) {
	svc := nilService(nil)
	err := svc.Cancel(context.Background(), "tenant-a", "", "some-id")
	if err == nil {
		t.Fatalf("expected error for empty kchatUserID (per-user authz fence)")
	}
}

func TestCancel_EmptyIDReturnsNotFound(t *testing.T) {
	svc := nilService(nil)
	err := svc.Cancel(context.Background(), "tenant-a", "kchat-a", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
