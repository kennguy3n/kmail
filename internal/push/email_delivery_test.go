package push

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildEmailDeliveryNotification_AllFields(t *testing.T) {
	t.Parallel()
	received := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	hasAttach := true
	d := EmailDelivery{
		AccountID:     "acct-1",
		EmailID:       "email-42",
		MailboxID:     "mbx-inbox",
		ThreadID:      "thread-7",
		Keywords:      []string{"$seen", "$important", "$seen"},
		Subject:       "Hello",
		Snippet:       "World snippet",
		From:          "Alice <alice@example.com>",
		ReceivedAt:    received,
		HasAttachment: &hasAttach,
		EmailState:    "es-1",
		MailboxState:  "ms-1",
	}
	got := BuildEmailDeliveryNotification(d)
	if got.Kind != NotificationKindNewEmail {
		t.Fatalf("kind=%q want %q", got.Kind, NotificationKindNewEmail)
	}
	cases := map[string]string{
		EmailDeliveryKeyAccountID:     "acct-1",
		EmailDeliveryKeyEmailID:       "email-42",
		EmailDeliveryKeyMailboxID:     "mbx-inbox",
		EmailDeliveryKeyThreadID:      "thread-7",
		EmailDeliveryKeySubject:       "Hello",
		EmailDeliveryKeySnippet:       "World snippet",
		EmailDeliveryKeyFrom:          "Alice <alice@example.com>",
		EmailDeliveryKeyReceivedAt:    "1735787045",
		EmailDeliveryKeyHasAttachment: "true",
		EmailDeliveryKeyEmailState:    "es-1",
		EmailDeliveryKeyMailboxState:  "ms-1",
		EmailDeliveryKeyKeywords:      "$seen,$important", // dedupes
	}
	for k, want := range cases {
		if g := got.Data[k]; g != want {
			t.Errorf("data[%q]=%q want %q", k, g, want)
		}
	}
	if got.Title != "Alice <alice@example.com>" {
		t.Errorf("title=%q want sender-derived", got.Title)
	}
	if got.Body != "Hello" {
		t.Errorf("body=%q want subject-derived", got.Body)
	}
}

func TestBuildEmailDeliveryNotification_OmitsEmpty(t *testing.T) {
	t.Parallel()
	got := BuildEmailDeliveryNotification(EmailDelivery{EmailID: "x"})
	if _, ok := got.Data[EmailDeliveryKeyReceivedAt]; ok {
		t.Errorf("received_at_unix should be omitted when zero")
	}
	if _, ok := got.Data[EmailDeliveryKeyHasAttachment]; ok {
		t.Errorf("has_attachment should be omitted when nil pointer")
	}
	if _, ok := got.Data[EmailDeliveryKeyKeywords]; ok {
		t.Errorf("keywords should be omitted when empty slice")
	}
	if got.Title != "New email" {
		t.Errorf("title fallback=%q want %q", got.Title, "New email")
	}
	if got.Body != "You have a new message." {
		t.Errorf("body fallback=%q want %q", got.Body, "You have a new message.")
	}
}

func TestBuildEmailDeliveryNotification_TruncatesAtRuneBoundary(t *testing.T) {
	t.Parallel()
	// 300 4-byte runes — well beyond subjectMaxLen (256). Result
	// must be valid UTF-8 (no broken trailing rune).
	long := strings.Repeat("\u00e9", 300) // "é" — 2 bytes each
	d := EmailDelivery{Subject: long, EmailID: "x"}
	got := BuildEmailDeliveryNotification(d)
	subj := got.Data[EmailDeliveryKeySubject]
	if rc := len([]rune(subj)); rc != 256 {
		t.Errorf("subject rune-count=%d want 256", rc)
	}
	// Round-trip through JSON to assert the payload is well-formed
	// UTF-8 — a mid-rune truncation would produce invalid bytes.
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("marshal: %v", err)
	}
}

func TestBuildEmailDeliveryNotification_KeywordsCommaStripped(t *testing.T) {
	t.Parallel()
	d := EmailDelivery{
		EmailID:  "x",
		Keywords: []string{"$seen", "evil,keyword", "$flagged"},
	}
	got := BuildEmailDeliveryNotification(d)
	kw := got.Data[EmailDeliveryKeyKeywords]
	// Comma stripped from "evil,keyword" -> "evilkeyword".
	if !strings.Contains(kw, "evilkeyword") {
		t.Errorf("keywords=%q want comma-stripped", kw)
	}
	parts := strings.Split(kw, ",")
	if len(parts) != 3 {
		t.Errorf("parts=%d want 3 entries: %q", len(parts), kw)
	}
}

func TestBuildEmailDeliveryNotification_HasAttachmentFalse(t *testing.T) {
	t.Parallel()
	no := false
	got := BuildEmailDeliveryNotification(EmailDelivery{EmailID: "x", HasAttachment: &no})
	if got.Data[EmailDeliveryKeyHasAttachment] != "false" {
		t.Errorf("has_attachment=%q want %q", got.Data[EmailDeliveryKeyHasAttachment], "false")
	}
}

func TestCanonicaliseKeywords(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name string
		in   []string
		out  string
	}{
		{"empty", nil, ""},
		{"single", []string{"$seen"}, "$seen"},
		{"dedupe", []string{"$seen", "$seen"}, "$seen"},
		{"whitespace trimmed", []string{"  $seen  "}, "$seen"},
		{"empty filtered", []string{"$seen", "", "$flagged"}, "$seen,$flagged"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			if g := canonicaliseKeywords(tc.in); g != tc.out {
				t.Errorf("got %q want %q", g, tc.out)
			}
		})
	}
}
