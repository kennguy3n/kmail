package calendarbridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/chatbridge"
)

// fakeChat records the channel + message of every post.
type fakeChat struct {
	posts []struct {
		channel string
		msg     chatbridge.ChannelMessage
	}
	err error
}

func (f *fakeChat) PostChannelMessage(_ context.Context, channelID string, msg chatbridge.ChannelMessage) error {
	if f.err != nil {
		return f.err
	}
	f.posts = append(f.posts, struct {
		channel string
		msg     chatbridge.ChannelMessage
	}{channelID, msg})
	return nil
}

func TestNotifier_NilDeps(t *testing.T) {
	if NewNotifier(nil, StaticChannelResolver{ChannelID: "c1"}) != nil {
		t.Error("NewNotifier(nil chat) should be nil")
	}
	if NewNotifier(&fakeChat{}, nil) != nil {
		t.Error("NewNotifier(nil resolver) should be nil")
	}
	// nil-receiver-safe methods.
	var n *Notifier
	ctx := context.Background()
	if err := n.NotifyEventCreated(ctx, "t", EventInfo{}); err != nil {
		t.Errorf("nil NotifyEventCreated: %v", err)
	}
	if err := n.NotifyEventUpdated(ctx, "t", EventInfo{}, "x"); err != nil {
		t.Errorf("nil NotifyEventUpdated: %v", err)
	}
	if err := n.NotifyEventCancelled(ctx, "t", EventInfo{}); err != nil {
		t.Errorf("nil NotifyEventCancelled: %v", err)
	}
	if err := n.NotifyReminder(ctx, "t", EventInfo{}, 5); err != nil {
		t.Errorf("nil NotifyReminder: %v", err)
	}
}

func TestNotifier_PostsAllTransitions(t *testing.T) {
	chat := &fakeChat{}
	n := NewNotifier(chat, StaticChannelResolver{ChannelID: "chan-1"})
	ctx := context.Background()
	ev := EventInfo{
		UID: "ev1", Summary: "Standup", Start: "2026-05-01T09:00:00Z",
		End: "2026-05-01T09:30:00Z", Location: "HQ", Organizer: "alice@example.com",
		CalendarID: "default",
	}

	if err := n.NotifyEventCreated(ctx, "t", ev); err != nil {
		t.Fatalf("created: %v", err)
	}
	if err := n.NotifyEventUpdated(ctx, "t", ev, "Time moved to 4pm"); err != nil {
		t.Fatalf("updated: %v", err)
	}
	if err := n.NotifyEventCancelled(ctx, "t", ev); err != nil {
		t.Fatalf("cancelled: %v", err)
	}
	if err := n.NotifyReminder(ctx, "t", ev, 15); err != nil {
		t.Fatalf("reminder: %v", err)
	}

	if len(chat.posts) != 4 {
		t.Fatalf("posts=%d want 4", len(chat.posts))
	}
	titles := []string{
		chat.posts[0].msg.Text, chat.posts[1].msg.Text,
		chat.posts[2].msg.Text, chat.posts[3].msg.Text,
	}
	if !strings.HasPrefix(titles[0], "New meeting: Standup") {
		t.Errorf("created title=%q", titles[0])
	}
	if !strings.Contains(titles[1], "Time moved to 4pm") {
		t.Errorf("updated title=%q", titles[1])
	}
	if !strings.HasPrefix(titles[2], "Meeting cancelled") {
		t.Errorf("cancelled title=%q", titles[2])
	}
	if !strings.HasPrefix(titles[3], "Meeting in 15 minutes") {
		t.Errorf("reminder title=%q", titles[3])
	}
	// Body carries the event detail lines.
	body := chat.posts[0].msg.Attachments[0].Text
	for _, want := range []string{"Start:", "End:", "Location:", "Organizer:"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if chat.posts[0].channel != "chan-1" {
		t.Errorf("channel=%q want chan-1", chat.posts[0].channel)
	}
}

func TestNotifier_EmptySummaryFallback(t *testing.T) {
	chat := &fakeChat{}
	n := NewNotifier(chat, StaticChannelResolver{ChannelID: "c"})
	if err := n.NotifyEventCreated(context.Background(), "t", EventInfo{}); err != nil {
		t.Fatalf("created: %v", err)
	}
	if !strings.Contains(chat.posts[0].msg.Text, "(no title)") {
		t.Errorf("expected (no title) fallback, got %q", chat.posts[0].msg.Text)
	}
}

func TestNotifier_ResolveChannelError(t *testing.T) {
	// Empty StaticChannelResolver returns a resolve error which post
	// propagates.
	n := NewNotifier(&fakeChat{}, StaticChannelResolver{})
	if err := n.NotifyEventCreated(context.Background(), "t", EventInfo{Summary: "x"}); err == nil {
		t.Error("expected resolve error for empty channel")
	}
}

func TestStaticChannelResolver(t *testing.T) {
	if _, err := (StaticChannelResolver{}).ResolveChannel(context.Background(), "t", "c"); err == nil {
		t.Error("empty resolver should error")
	}
	ch, err := (StaticChannelResolver{ChannelID: "x"}).ResolveChannel(context.Background(), "t", "c")
	if err != nil || ch != "x" {
		t.Errorf("resolve=%q err=%v", ch, err)
	}
}

func TestNotifier_PostError(t *testing.T) {
	n := NewNotifier(&fakeChat{err: errors.New("boom")}, StaticChannelResolver{ChannelID: "c"})
	if err := n.NotifyReminder(context.Background(), "t", EventInfo{Summary: "x"}, 5); err == nil {
		t.Error("expected post error to propagate")
	}
}
