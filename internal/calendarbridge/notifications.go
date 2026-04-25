// Package calendarbridge — KChat scheduling notifications.
//
// Phase 4 closes the "Calendar bridge" checklist item by adding
// KChat-side notifications for calendar event lifecycle transitions
// (create, update, cancel) plus pre-event reminders driven by the
// reminder worker. The calendar data plane stays on CalDAV; the
// notifications surface lives on KChat so users see "New meeting
// at 3pm" / "Meeting in 5 minutes" in the same channel they already
// chat in.
//
// Channel routing for Phase 4 is intentionally simple — every
// tenant maps to a single configured "scheduling" channel (set via
// `KMAIL_CALENDAR_NOTIFY_CHANNEL` env). Phase 5 will expand this to
// per-event channel selection via the meeting-room resource calendar
// admin UI.
package calendarbridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/kennguy3n/kmail/internal/chatbridge"
)

// ChannelResolver maps a tenant + event into the KChat channel ID
// the notification should be posted to. Allows tests to inject a
// stub without standing up a Postgres-backed mapping table.
type ChannelResolver interface {
	ResolveChannel(ctx context.Context, tenantID, calendarID string) (string, error)
}

// StaticChannelResolver returns the same channel ID for every
// tenant. Used in dev / single-channel deployments.
type StaticChannelResolver struct {
	ChannelID string
}

func (s StaticChannelResolver) ResolveChannel(_ context.Context, _, _ string) (string, error) {
	if s.ChannelID == "" {
		return "", fmt.Errorf("calendarbridge: no notification channel configured")
	}
	return s.ChannelID, nil
}

// Notifier posts KChat messages for calendar event transitions.
// Wraps the existing `chatbridge.KChatClient` so the calendar
// bridge does not duplicate the KChat REST plumbing.
type Notifier struct {
	chat     chatbridge.KChatClient
	channels ChannelResolver
}

// NewNotifier returns a Notifier wired to the given chat client and
// channel resolver. Returns nil when either dependency is nil so
// the caller can guard against the dev-mode "no chat configured"
// case without conditionals at every call site.
func NewNotifier(chat chatbridge.KChatClient, channels ChannelResolver) *Notifier {
	if chat == nil || channels == nil {
		return nil
	}
	return &Notifier{chat: chat, channels: channels}
}

// EventInfo is the trimmed event view the notifications surface
// renders. The full iCalendar payload lives in CalDAV — this struct
// just carries the fields the message-card template needs.
type EventInfo struct {
	UID         string
	Summary     string
	Start       string
	End         string
	Location    string
	Organizer   string
	CalendarID  string
}

// NotifyEventCreated posts a "New meeting" card to the resolved
// channel. nil-receiver-safe so callers can ignore the dev-mode
// no-chat case.
func (n *Notifier) NotifyEventCreated(ctx context.Context, tenantID string, ev EventInfo) error {
	if n == nil {
		return nil
	}
	return n.post(ctx, tenantID, ev, fmt.Sprintf("New meeting: %s", or(ev.Summary, "(no title)")))
}

// NotifyEventUpdated posts an updated-meeting card. `changes` is a
// freeform summary of what changed (e.g. "Time moved to 4pm");
// passed in by the handler since the calendar bridge does not
// (yet) diff iCalendar payloads.
func (n *Notifier) NotifyEventUpdated(ctx context.Context, tenantID string, ev EventInfo, changes string) error {
	if n == nil {
		return nil
	}
	title := fmt.Sprintf("Meeting updated: %s", or(ev.Summary, "(no title)"))
	if changes != "" {
		title += " — " + changes
	}
	return n.post(ctx, tenantID, ev, title)
}

// NotifyEventCancelled posts a cancellation card.
func (n *Notifier) NotifyEventCancelled(ctx context.Context, tenantID string, ev EventInfo) error {
	if n == nil {
		return nil
	}
	return n.post(ctx, tenantID, ev, fmt.Sprintf("Meeting cancelled: %s", or(ev.Summary, "(no title)")))
}

// NotifyReminder fires a pre-event reminder. `minutesBefore` is the
// configured reminder window so the message can say "in 15 minutes"
// rather than recomputing the delta on the client.
func (n *Notifier) NotifyReminder(ctx context.Context, tenantID string, ev EventInfo, minutesBefore int) error {
	if n == nil {
		return nil
	}
	title := fmt.Sprintf("Meeting in %d minutes: %s", minutesBefore, or(ev.Summary, "(no title)"))
	return n.post(ctx, tenantID, ev, title)
}

func (n *Notifier) post(ctx context.Context, tenantID string, ev EventInfo, title string) error {
	channelID, err := n.channels.ResolveChannel(ctx, tenantID, ev.CalendarID)
	if err != nil {
		return err
	}
	body := strings.Builder{}
	if ev.Start != "" {
		body.WriteString("Start: " + ev.Start + "\n")
	}
	if ev.End != "" {
		body.WriteString("End: " + ev.End + "\n")
	}
	if ev.Location != "" {
		body.WriteString("Location: " + ev.Location + "\n")
	}
	if ev.Organizer != "" {
		body.WriteString("Organizer: " + ev.Organizer + "\n")
	}
	msg := chatbridge.ChannelMessage{
		Text: title,
		Attachments: []chatbridge.Attachment{{
			Title: title,
			Text:  body.String(),
		}},
	}
	return n.chat.PostChannelMessage(ctx, channelID, msg)
}

func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
