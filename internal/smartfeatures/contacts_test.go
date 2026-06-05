package smartfeatures

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestContactTracker_RecordAndTop(t *testing.T) {
	ctx := context.Background()
	tr, err := NewContactTracker(newTestRedis(t), time.Hour)
	if err != nil {
		t.Fatalf("NewContactTracker: %v", err)
	}

	// alice emailed 3x, bob 1x.
	for i := 0; i < 3; i++ {
		if err := tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com"}); err != nil {
			t.Fatalf("RecordSend: %v", err)
		}
	}
	if err := tr.RecordSend(ctx, "t1", "u1", []string{"bob@example.com"}); err != nil {
		t.Fatalf("RecordSend: %v", err)
	}

	top, err := tr.TopContacts(ctx, "t1", "u1", 10)
	if err != nil {
		t.Fatalf("TopContacts: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2 contacts, got %d", len(top))
	}
	if top[0].Email != "alice@example.com" || top[0].Count != 3 {
		t.Fatalf("top contact wrong: %#v", top[0])
	}
}

func TestContactTracker_CapturesDisplayNames(t *testing.T) {
	ctx := context.Background()
	tr, err := NewContactTracker(newTestRedis(t), time.Hour)
	if err != nil {
		t.Fatalf("NewContactTracker: %v", err)
	}

	// Recipients arrive in the `Name <email>` / quoted form the compose
	// field produces. The frequency ZSET keys on the bare email; the
	// display name is captured separately and surfaced on read.
	if err := tr.RecordSend(ctx, "t1", "u1", []string{
		"Alice Smith <alice@example.com>",
		`"Bob, Jr." <bob@example.com>`,
		"carol@example.com", // no display name
	}); err != nil {
		t.Fatalf("RecordSend: %v", err)
	}

	top, err := tr.TopContacts(ctx, "t1", "u1", 10)
	if err != nil {
		t.Fatalf("TopContacts: %v", err)
	}
	byEmail := map[string]Contact{}
	for _, c := range top {
		byEmail[c.Email] = c
	}
	if got := byEmail["alice@example.com"].Name; got != "Alice Smith" {
		t.Errorf("alice name = %q, want %q", got, "Alice Smith")
	}
	if got := byEmail["bob@example.com"].Name; got != "Bob, Jr." {
		t.Errorf("bob name = %q, want %q (quoted name should be unquoted)", got, "Bob, Jr.")
	}
	if got := byEmail["carol@example.com"].Name; got != "" {
		t.Errorf("carol name = %q, want empty (no display name sent)", got)
	}

	// A later send with a different display name overwrites the prior one.
	if err := tr.RecordSend(ctx, "t1", "u1", []string{"Alice S. <alice@example.com>"}); err != nil {
		t.Fatalf("RecordSend (rename): %v", err)
	}
	top, _ = tr.TopContacts(ctx, "t1", "u1", 10)
	for _, c := range top {
		if c.Email == "alice@example.com" && c.Name != "Alice S." {
			t.Errorf("alice name after rename = %q, want %q", c.Name, "Alice S.")
		}
	}
}

func TestContactTracker_SuggestCoRecipientsCarriesNames(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	// Email alice + bob together a few times so bob is a co-recipient of alice.
	for i := 0; i < 2; i++ {
		if err := tr.RecordSend(ctx, "t1", "u1", []string{
			"Alice <alice@example.com>",
			"Bob B <bob@example.com>",
		}); err != nil {
			t.Fatalf("RecordSend: %v", err)
		}
	}
	sug, err := tr.SuggestCoRecipients(ctx, "t1", "u1", "alice@example.com", nil, 5)
	if err != nil {
		t.Fatalf("SuggestCoRecipients: %v", err)
	}
	if len(sug) != 1 || sug[0].Email != "bob@example.com" {
		t.Fatalf("unexpected suggestions: %#v", sug)
	}
	if sug[0].Name != "Bob B" {
		t.Errorf("co-recipient name = %q, want %q", sug[0].Name, "Bob B")
	}
}

func TestContactTracker_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com"})

	other, err := tr.TopContacts(ctx, "t2", "u1", 10)
	if err != nil {
		t.Fatalf("TopContacts: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected tenant isolation, got %#v", other)
	}
}

func TestContactTracker_SendCount(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "alice@example.com"})

	// Same address listed twice in one send counts once.
	c, err := tr.SendCount(ctx, "t1", "u1", "ALICE@example.com")
	if err != nil {
		t.Fatalf("SendCount: %v", err)
	}
	if c != 1 {
		t.Fatalf("SendCount = %v, want 1", c)
	}
	miss, err := tr.SendCount(ctx, "t1", "u1", "nobody@example.com")
	if err != nil {
		t.Fatalf("SendCount miss: %v", err)
	}
	if miss != 0 {
		t.Fatalf("SendCount miss = %v, want 0", miss)
	}
}

func TestContactTracker_CoRecipients(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	// alice + bob emailed together twice, alice + carol once.
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "bob@example.com"})
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "bob@example.com"})
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "carol@example.com"})

	sug, err := tr.SuggestCoRecipients(ctx, "t1", "u1", "alice@example.com", nil, 5)
	if err != nil {
		t.Fatalf("SuggestCoRecipients: %v", err)
	}
	if len(sug) != 2 {
		t.Fatalf("expected 2 co-recipients, got %#v", sug)
	}
	if sug[0].Email != "bob@example.com" {
		t.Fatalf("expected bob first, got %#v", sug)
	}
	// Excluding bob (already on the draft) drops him.
	sug2, _ := tr.SuggestCoRecipients(ctx, "t1", "u1", "alice@example.com", []string{"bob@example.com"}, 5)
	if len(sug2) != 1 || sug2[0].Email != "carol@example.com" {
		t.Fatalf("exclude failed: %#v", sug2)
	}
}

// TestContactTracker_CoRecipientsDisplayNameForm pins that a
// `Display Name <email>` anchor/exclude (what the compose To field
// yields) is canonicalized to the bare email, so it matches the
// stored recipients instead of leaking an already-added contact.
func TestContactTracker_CoRecipientsDisplayNameForm(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "bob@example.com"})
	_ = tr.RecordSend(ctx, "t1", "u1", []string{"alice@example.com", "carol@example.com"})

	// Anchor carries a display name; bob is already on the draft, also
	// with a display name. Both must normalize to bare emails.
	sug, err := tr.SuggestCoRecipients(ctx, "t1", "u1",
		"Alice <alice@example.com>", []string{"Bob <bob@example.com>"}, 5)
	if err != nil {
		t.Fatalf("SuggestCoRecipients: %v", err)
	}
	if len(sug) != 1 || sug[0].Email != "carol@example.com" {
		t.Fatalf("display-name anchor/exclude not normalized: %#v", sug)
	}
}

func TestNormalizeAddress(t *testing.T) {
	cases := map[string]string{
		"  Alice <Alice@Example.com> ": "alice@example.com",
		"BOB@example.com":              "bob@example.com",
		"no-at-sign":                   "",
		"":                             "",
		"<carol@x.io>":                 "carol@x.io",
	}
	for in, want := range cases {
		if got := normalizeAddress(in); got != want {
			t.Errorf("normalizeAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContactTracker_RecordValidations(t *testing.T) {
	ctx := context.Background()
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	if err := tr.RecordSend(ctx, "", "u1", []string{"a@b.com"}); err == nil {
		t.Fatalf("expected error for empty tenant")
	}
	// Empty recipients is a no-op, not an error.
	if err := tr.RecordSend(ctx, "t1", "u1", nil); err != nil {
		t.Fatalf("empty recipients should be no-op: %v", err)
	}
}
