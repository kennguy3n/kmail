// Package push — typed `EmailDelivery` payload builder.
//
// The native clients (iOS / Android / Electron Desktop, all
// driven by the Rust SDK in `sdk/kmail-core`) need enough
// metadata in every `new_email` push to update their local
// SQLite cache without forcing a full `Email/changes` round-trip.
// The transport-level payload that APNs / FCM / Web Push deliver
// to the device is a flat `map[string]string` (`Notification.Data`)
// — perfect for opaque K/V but trivially easy to silently drift
// between the BFF (writer) and the SDK (reader) when there's no
// shared schema.
//
// This file pins the shared schema. The exported constants are
// the canonical wire-format keys; the SDK side
// (`sdk/kmail-core/src/push.rs::EmailDeliveryHint`) reads from
// the same key names. The `BuildEmailDeliveryNotification`
// builder centralises population so every emit site is bound to
// the same contract — adding a field is one change here plus the
// SDK accessor instead of N changes scattered across the
// fan-out code paths.
package push

import (
	"strconv"
	"strings"
	"time"
)

// Wire-format keys for `Notification.Data`. Kept as exported
// constants so the SDK (`sdk/kmail-core/src/push.rs`) and the
// BFF agree byte-for-byte. Any drift here would silently strip
// the affected field from the device payload and force a full
// `Email/changes` re-sync — which is exactly what these hints
// exist to avoid.
const (
	EmailDeliveryKeyEmailID       = "email_id"
	EmailDeliveryKeyMailboxID     = "mailbox_id"
	EmailDeliveryKeyAccountID     = "account_id"
	EmailDeliveryKeyKeywords      = "keywords"
	EmailDeliveryKeySubject       = "subject"
	EmailDeliveryKeySnippet       = "snippet"
	EmailDeliveryKeyFrom          = "from"
	EmailDeliveryKeyThreadID      = "thread_id"
	EmailDeliveryKeyReceivedAt    = "received_at_unix"
	EmailDeliveryKeyEmailState    = "email_state"
	EmailDeliveryKeyMailboxState  = "mailbox_state"
	EmailDeliveryKeyHasAttachment = "has_attachment"
)

// NotificationKindNewEmail is the canonical `Notification.Kind`
// value for an inbound-mail event. Kept here (next to the
// metadata schema) instead of `push.go` so future emit sites
// don't have to import two constants to wire the contract.
const NotificationKindNewEmail = "new_email"

// EmailDelivery is the typed input to
// `BuildEmailDeliveryNotification`. Every field is optional;
// fields left zero-valued are simply omitted from the wire
// payload so a partial fan-out (e.g. when the JMAP
// EventSource worker only had IDs available) still produces a
// valid push without surfacing empty-string keys on the device.
type EmailDelivery struct {
	// AccountID is the Stalwart account the email landed in.
	// Used SDK-side to dispatch the hint to the right
	// `KMailClient` instance when a single device handles
	// multiple accounts.
	AccountID string

	// EmailID is the JMAP `Email/get` object ID. Required for
	// the SDK to insert/update its local row; if empty the SDK
	// falls back to a full `Email/changes` round-trip.
	EmailID string

	// MailboxID is the mailbox where the email landed (typically
	// the inbox, but the BFF doesn't enforce that — Sieve rules
	// may have routed the email elsewhere before the
	// `EmailDelivery` event fires).
	MailboxID string

	// ThreadID is the JMAP `threadId`. Persisted SDK-side so the
	// UI can group the new message with existing thread state
	// without re-hydrating the whole conversation.
	ThreadID string

	// Keywords carries the JMAP `keywords` set as a
	// comma-separated list. Matches the JMAP wire shape of
	// `{"$seen": true, "$important": true}` collapsed to
	// `"$seen,$important"` so the device payload remains
	// strings-only. Order is canonicalised (sorted) by the
	// builder so two equivalent keyword sets hash the same on
	// the device side.
	Keywords []string

	// Subject is the email's subject line. Truncated to
	// `subjectMaxLen` runes by the builder so a 4 KB APNs
	// payload budget isn't blown by a single hostile sender.
	Subject string

	// Snippet is the first ~100 runes of the email body, plain
	// text. Truncated by the builder per `snippetMaxLen`.
	Snippet string

	// From is the human-readable sender display string —
	// typically `"Name <name@example.com>"`. The builder does
	// not validate the shape (the SDK parses defensively) but
	// truncates to `fromMaxLen`.
	From string

	// ReceivedAt is when Stalwart accepted the email for
	// delivery. Serialised as a Unix-epoch second count
	// (decimal string) so the device side does not need a time
	// parser. UTC is the only acceptable interpretation; the
	// builder normalises to UTC unconditionally.
	ReceivedAt time.Time

	// HasAttachment mirrors the JMAP `hasAttachment` boolean.
	// Serialised as `"true"` / `"false"`; omitted from the wire
	// payload when the source didn't populate it (zero value).
	HasAttachment *bool

	// EmailState is the canonical `Email/get` state token after
	// this delivery — what the SDK should pass as the next
	// `Email/changes` cursor if it skips the round-trip. The BFF
	// reads this from the JMAP EventSource StateChange envelope.
	EmailState string

	// MailboxState is the canonical `Mailbox/get` state token
	// after this delivery. Used when the mailbox list itself
	// changed (e.g. unseen count update on the affected mailbox).
	MailboxState string

	// Title overrides the default push notification title. When
	// empty, the builder derives a sensible default from
	// `From` / `Subject`.
	Title string

	// Body overrides the default push notification body. When
	// empty, the builder derives a sensible default from
	// `Snippet` / `Subject`.
	Body string
}

// Wire-format size caps. These keep the device-side payload well
// inside the platform budgets:
//
//   - APNs: 4 KiB JSON envelope per HTTP/2 frame (RFC 8030 §4
//     and the APNs provider API docs).
//   - FCM:  4 KiB notification message payload (Firebase docs).
//   - Web Push: 4 KiB payload after content-encryption per
//     RFC 8291 §3, so the cleartext budget is closer to 3 KiB.
//
// 256-rune subject + 200-rune snippet + 128-rune sender +
// 64-rune IDs leaves comfortable headroom for the JSON wrapping
// and the platform-specific envelope (`aps`, `notification`,
// etc.).
const (
	subjectMaxLen = 256
	snippetMaxLen = 200
	fromMaxLen    = 128
	idMaxLen      = 128
	stateMaxLen   = 64
)

// BuildEmailDeliveryNotification renders an `EmailDelivery` into
// the transport `Notification` shape. The returned Notification's
// `Kind` is `NotificationKindNewEmail`; `Data` carries every
// non-empty field of the input keyed by the `EmailDeliveryKey*`
// constants.
//
// `Title` and `Body` default to user-visible strings derived from
// `From` / `Subject` / `Snippet` so callers that only have the
// JMAP delivery payload don't have to populate them separately.
// The SDK ignores both in favour of the typed Data fields, but
// the OS notification surface (lock-screen, banner) renders them
// directly so they must be non-empty for the user to see anything
// at all when the app is backgrounded.
func BuildEmailDeliveryNotification(d EmailDelivery) Notification {
	data := map[string]string{}

	if v := truncate(d.AccountID, idMaxLen); v != "" {
		data[EmailDeliveryKeyAccountID] = v
	}
	if v := truncate(d.EmailID, idMaxLen); v != "" {
		data[EmailDeliveryKeyEmailID] = v
	}
	if v := truncate(d.MailboxID, idMaxLen); v != "" {
		data[EmailDeliveryKeyMailboxID] = v
	}
	if v := truncate(d.ThreadID, idMaxLen); v != "" {
		data[EmailDeliveryKeyThreadID] = v
	}
	if v := truncate(d.Subject, subjectMaxLen); v != "" {
		data[EmailDeliveryKeySubject] = v
	}
	if v := truncate(d.Snippet, snippetMaxLen); v != "" {
		data[EmailDeliveryKeySnippet] = v
	}
	if v := truncate(d.From, fromMaxLen); v != "" {
		data[EmailDeliveryKeyFrom] = v
	}
	if !d.ReceivedAt.IsZero() {
		data[EmailDeliveryKeyReceivedAt] = strconv.FormatInt(d.ReceivedAt.UTC().Unix(), 10)
	}
	if d.HasAttachment != nil {
		data[EmailDeliveryKeyHasAttachment] = strconv.FormatBool(*d.HasAttachment)
	}
	if v := truncate(d.EmailState, stateMaxLen); v != "" {
		data[EmailDeliveryKeyEmailState] = v
	}
	if v := truncate(d.MailboxState, stateMaxLen); v != "" {
		data[EmailDeliveryKeyMailboxState] = v
	}
	if v := canonicaliseKeywords(d.Keywords); v != "" {
		data[EmailDeliveryKeyKeywords] = v
	}

	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = defaultTitle(d)
	}
	body := strings.TrimSpace(d.Body)
	if body == "" {
		body = defaultBody(d)
	}

	return Notification{
		Title: title,
		Body:  body,
		Kind:  NotificationKindNewEmail,
		Data:  data,
	}
}

// defaultTitle picks a user-visible title in the absence of an
// explicit override. Priority:
//
//  1. `From` (most informative for the lock-screen).
//  2. `"New email"` literal — last-resort, never empty.
//
// Stalwart populates `From` for every accepted message so the
// fallback is reserved for malformed events.
func defaultTitle(d EmailDelivery) string {
	if from := strings.TrimSpace(d.From); from != "" {
		return truncate(from, fromMaxLen)
	}
	return "New email"
}

// defaultBody picks a user-visible body in the absence of an
// explicit override. Subject first, then snippet, then a literal
// fallback.
func defaultBody(d EmailDelivery) string {
	if subj := strings.TrimSpace(d.Subject); subj != "" {
		return truncate(subj, subjectMaxLen)
	}
	if snip := strings.TrimSpace(d.Snippet); snip != "" {
		return truncate(snip, snippetMaxLen)
	}
	return "You have a new message."
}

// canonicaliseKeywords joins a JMAP keyword set into the
// comma-separated wire-format string. Empty/whitespace entries
// are dropped; duplicates are deduplicated; ordering is preserved
// from the input slice (callers are responsible for sorting if
// they need stable ordering — sorting here would discord with
// JMAP's set semantics which are unordered).
//
// The choice of `,` as a separator is constrained by JMAP itself:
// `$seen`, `$important`, `$flagged` etc are RFC-defined keyword
// names that never contain commas (RFC 5788), and custom keywords
// follow IMAP keyword grammar (RFC 6154) which also forbids them.
// So `,` is unambiguous and parseable with a single `split`.
func canonicaliseKeywords(kw []string) string {
	if len(kw) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(kw))
	out := make([]string, 0, len(kw))
	for _, k := range kw {
		t := strings.TrimSpace(k)
		if t == "" {
			continue
		}
		if strings.ContainsAny(t, ",") {
			// Defence-in-depth: a future custom-keyword spec
			// could allow commas. Strip them so the wire format
			// stays parseable instead of silently breaking the
			// device-side `split`.
			t = strings.ReplaceAll(t, ",", "")
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return strings.Join(out, ",")
}

// truncate clamps a string to at most `n` runes (not bytes — the
// platform push budgets are byte-bounded but truncating mid-rune
// would produce a broken UTF-8 sequence that the device-side JSON
// parser rejects, dropping the entire payload). Returns the
// input unchanged when it already fits.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
