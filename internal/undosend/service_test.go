package undosend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestService(t *testing.T, opts ...func(*Config)) (*Service, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cfg := Config{Client: client}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, mr, client
}

func TestNewService_ValidatesConfig(t *testing.T) {
	if _, err := NewService(Config{}); err == nil {
		t.Fatalf("NewService(empty): expected error")
	}
}

func TestNewService_DelayLargerThanMaxRejected(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	_, err = NewService(Config{
		Client:   client,
		Delay:    10 * time.Minute,
		MaxDelay: 5 * time.Minute,
	})
	if err == nil {
		t.Fatalf("expected delay>max to fail")
	}
}

func TestHold_PersistsCompanionAndSortedSet(t *testing.T) {
	svc, mr, _ := newTestService(t)
	ctx := context.Background()
	ps, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		CreateID:          "submission",
		EmailID:           "email-1",
		IdentityID:        "ident-1",
		SubmissionPayload: []byte(`{"emailId":"email-1","identityId":"ident-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if ps.ID == "" || ps.Status != StatusPending {
		t.Fatalf("Hold: bad PendingSend %+v", ps)
	}
	if !mr.Exists(companionKey(ps.ID)) {
		t.Fatalf("companion key not written")
	}
	score, err := mr.ZScore(sortedSetKey, ps.ID)
	if err != nil {
		t.Fatalf("sorted set score: %v", err)
	}
	if int64(score) != ps.DeadlineAt.Unix() {
		t.Fatalf("sorted set score = %d, want %d", int64(score), ps.DeadlineAt.Unix())
	}
}

func TestHold_RequiresTenantID(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Hold(context.Background(), HoldInput{
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: []byte(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "TenantID") {
		t.Fatalf("expected TenantID error, got %v", err)
	}
}

func TestHold_RequiresPayload(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: nil,
	})
	if err == nil || !strings.Contains(err.Error(), "SubmissionPayload") {
		t.Fatalf("expected SubmissionPayload error, got %v", err)
	}
}

func TestCancel_HappyPath(t *testing.T) {
	svc, mr, _ := newTestService(t)
	ctx := context.Background()
	ps, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := svc.Cancel(ctx, ps.ID, "tenant-a"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if mr.Exists(companionKey(ps.ID)) {
		t.Fatalf("companion key still exists after cancel")
	}
	if _, err := mr.ZScore(sortedSetKey, ps.ID); err == nil {
		t.Fatalf("sorted set entry still present after cancel")
	}
}

func TestCancel_TenantMismatchReturnsTenantMismatch(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	ps, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	err = svc.Cancel(ctx, ps.ID, "tenant-OTHER")
	if !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("Cancel cross-tenant = %v, want ErrTenantMismatch", err)
	}
}

func TestCancel_MissingReturnsAlreadySent(t *testing.T) {
	svc, _, _ := newTestService(t)
	err := svc.Cancel(context.Background(), "nope", "tenant-a")
	if !errors.Is(err, ErrAlreadySent) {
		t.Fatalf("Cancel missing = %v, want ErrAlreadySent", err)
	}
}

func TestGet_ReadsBackPayload(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	ps, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		IdentityID:        "ident-1",
		CreateID:          "sub",
		SubmissionPayload: []byte(`{"emailId":"email-1","identityId":"ident-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	got, err := svc.Get(ctx, ps.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EmailID != "email-1" || got.TenantID != "tenant-a" || got.IdentityID != "ident-1" {
		t.Fatalf("Get: unexpected fields %+v", got)
	}
}

func TestGet_MissingReturnsNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Get(context.Background(), "no-such-id")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestClaim_OnlyFirstCallerWins(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	ps, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	first, err := svc.claim(ctx, ps.ID)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true, nil", first, err)
	}
	second, err := svc.claim(ctx, ps.ID)
	if err != nil || second {
		t.Fatalf("second claim = %v, %v; want false, nil", second, err)
	}
}

func TestMarkFailed_PushesOntoDeadLetter(t *testing.T) {
	svc, mr, _ := newTestService(t)
	ctx := context.Background()
	ps, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := svc.markFailed(ctx, ps, "stalwart timeout"); err != nil {
		t.Fatalf("markFailed: %v", err)
	}
	items, err := mr.List(failedListKey)
	if err != nil {
		t.Fatalf("List dead-letter: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("dead-letter len = %d, want 1", len(items))
	}
	var got PendingSend
	if err := json.Unmarshal([]byte(items[0]), &got); err != nil {
		t.Fatalf("decode dead-letter: %v", err)
	}
	if got.Status != StatusFailed || got.LastError != "stalwart timeout" {
		t.Fatalf("dead-letter shape: %+v", got)
	}
}

func TestDueIDs_BeforeDeadlineEmpty(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, _, _ := newTestService(t, func(c *Config) {
		c.Delay = 10 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	ctx := context.Background()
	_, err := svc.Hold(ctx, HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	ids, err := svc.dueIDs(ctx, now, 50)
	if err != nil {
		t.Fatalf("dueIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("dueIDs before deadline = %v, want empty", ids)
	}
	due, err := svc.dueIDs(ctx, now.Add(11*time.Second), 50)
	if err != nil {
		t.Fatalf("dueIDs: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("dueIDs after deadline = %v, want 1", due)
	}
}
