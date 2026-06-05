package priority

import (
	"context"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/smartfeatures"
)

type fakeSource struct {
	msgs []smartfeatures.Message
	addr string
	err  error
}

func (f *fakeSource) ListInbox(_ context.Context, _, _ string, _ int) ([]smartfeatures.Message, error) {
	return f.msgs, f.err
}
func (f *fakeSource) UserAddress(_ context.Context, _, _ string) (string, error) {
	return f.addr, nil
}

type fakeHistory map[string]float64

func (h fakeHistory) SendCount(_ context.Context, _, _, sender string) (float64, error) {
	return h[sender], nil
}

func msg(id, from string, ts time.Time) smartfeatures.Message {
	return smartfeatures.Message{
		ID:         id,
		From:       []smartfeatures.Address{{Email: from}},
		ReceivedAt: ts,
		Keywords:   map[string]bool{},
	}
}

func TestService_Compute_Ranking(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	src := &fakeSource{
		addr: "me@acme.com",
		msgs: []smartfeatures.Message{
			msg("promo", "deals@shop.example.com", now),                // nothing → low
			msg("colleague", "coworker@acme.com", now.Add(-time.Hour)), // same tenant
			msg("friend", "alice@friends.com", now.Add(-2*time.Hour)),  // in contacts
		},
	}
	hist := fakeHistory{"alice@friends.com": 7}

	svc, err := NewService(Config{Source: src, History: hist})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	got, err := svc.Compute(ctx, "t1", "u1", 10)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 scored, got %d", len(got))
	}
	// promo (no signals) must rank last.
	if got[len(got)-1].Message.ID != "promo" {
		t.Fatalf("expected promo last, got order %v", ids(got))
	}
	// The in-contacts friend (30 + 15 send pts) should beat the
	// same-tenant colleague (20).
	if got[0].Message.ID != "friend" {
		t.Fatalf("expected friend first, got %v", ids(got))
	}
}

func TestService_Compute_Mention(t *testing.T) {
	ctx := context.Background()
	m := msg("m1", "someone@else.com", time.Now())
	m.Subject = "Hey me@acme.com can you review"
	src := &fakeSource{addr: "me@acme.com", msgs: []smartfeatures.Message{m}}
	svc, _ := NewService(Config{Source: src})
	got, _ := svc.Compute(ctx, "t1", "u1", 10)
	if got[0].Score < WeightMentionsUser {
		t.Fatalf("expected mention to contribute, score=%d", got[0].Score)
	}
}

func TestContainsMention_Boundary(t *testing.T) {
	cases := []struct {
		hay, local string
		want       bool
	}{
		{"ping @ada about it", "ada", true},   // standalone handle
		{"cc @ada, please", "ada", true},      // trailing punctuation is a boundary
		{"talk to @adam tomorrow", "ada", false}, // @adam must not match @ada
		{"@adams reported", "ada", false},
		{"no mention here", "ada", false},
		{"ends with @ada", "ada", true}, // end-of-string boundary
	}
	for _, c := range cases {
		if got := containsMention(c.hay, c.local); got != c.want {
			t.Errorf("containsMention(%q, %q) = %v, want %v", c.hay, c.local, got, c.want)
		}
	}
}

// TestMentionsUser_ShortLocalIgnored pins that a sub-threshold local
// part is not treated as an @handle (only the full-address signal
// can fire for it).
func TestMentionsUser_ShortLocalIgnored(t *testing.T) {
	m := smartfeatures.Message{Subject: "hey @jo and @joseph", Preview: ""}
	if mentionsUser(m, "jo@acme.com", "jo") {
		t.Fatalf("short local part @jo should not count as a mention")
	}
}

func TestService_Compute_DeterministicTieBreak(t *testing.T) {
	ctx := context.Background()
	ts := time.Now()
	// Two messages with identical (zero) signals and same timestamp:
	// tie-break by id ascending must be stable.
	src := &fakeSource{msgs: []smartfeatures.Message{
		msg("B", "x@y.com", ts),
		msg("A", "x@y.com", ts),
	}}
	svc, _ := NewService(Config{Source: src})
	got, _ := svc.Compute(ctx, "t1", "u1", 10)
	// x@y.com appears twice → InWindowCount=2 for both, equal score.
	if got[0].Message.ID != "A" {
		t.Fatalf("expected stable id tie-break (A first), got %v", ids(got))
	}
}

func TestService_Compute_RequiresIdentity(t *testing.T) {
	svc, _ := NewService(Config{Source: &fakeSource{}})
	if _, err := svc.Compute(context.Background(), "", "u1", 10); err == nil {
		t.Fatalf("expected error for empty tenant")
	}
}

func TestService_Compute_PersistsToStore(t *testing.T) {
	ctx := context.Background()
	store, _ := NewStore(newTestRedis(t), time.Minute)
	src := &fakeSource{msgs: []smartfeatures.Message{msg("E1", "a@b.com", time.Now())}}
	svc, _ := NewService(Config{Source: src, Store: store})
	if _, err := svc.Compute(ctx, "t1", "u1", 10); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	top, _ := store.Top(ctx, "t1", "u1", 10)
	if len(top) != 1 || top[0].EmailID != "E1" {
		t.Fatalf("expected cache populated, got %#v", top)
	}
}

func ids(s []Scored) []string {
	out := make([]string, len(s))
	for i, sc := range s {
		out[i] = sc.Message.ID
	}
	return out
}
