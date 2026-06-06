package smartfeatures

import "testing"

func TestParseUnsubscribe_None(t *testing.T) {
	if _, ok := ParseUnsubscribe(Message{}); ok {
		t.Fatalf("expected no unsubscribe info for empty message")
	}
}

func TestParseUnsubscribe_MailtoAndHTTP(t *testing.T) {
	m := Message{Headers: map[string]string{
		"List-Unsubscribe": "<mailto:u@list.example?subject=unsub>, <https://list.example/u/abc>",
	}}
	info, ok := ParseUnsubscribe(m)
	if !ok {
		t.Fatalf("expected unsubscribe info")
	}
	if len(info.MailtoURLs) != 1 || info.MailtoURLs[0] != "mailto:u@list.example?subject=unsub" {
		t.Fatalf("mailto parse wrong: %#v", info.MailtoURLs)
	}
	if len(info.HTTPURLs) != 1 || info.HTTPURLs[0] != "https://list.example/u/abc" {
		t.Fatalf("http parse wrong: %#v", info.HTTPURLs)
	}
	if info.OneClick {
		t.Fatalf("OneClick should be false without List-Unsubscribe-Post")
	}
}

func TestParseUnsubscribe_OneClick(t *testing.T) {
	m := Message{Headers: map[string]string{
		"List-Unsubscribe":      "<https://list.example/u/abc>",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}}
	info, ok := ParseUnsubscribe(m)
	if !ok {
		t.Fatalf("expected unsubscribe info")
	}
	if !info.OneClick {
		t.Fatalf("expected OneClick=true")
	}
	got, ok := info.PreferredHTTP()
	if !ok || got != "https://list.example/u/abc" {
		t.Fatalf("PreferredHTTP = %q,%v", got, ok)
	}
}

func TestParseUnsubscribe_OneClickRequiresHTTP(t *testing.T) {
	// One-click POST to a mailto target is meaningless; OneClick must
	// stay false when there is no http(s) target.
	m := Message{Headers: map[string]string{
		"List-Unsubscribe":      "<mailto:u@list.example>",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}}
	info, ok := ParseUnsubscribe(m)
	if !ok {
		t.Fatalf("expected info")
	}
	if info.OneClick {
		t.Fatalf("OneClick must be false with no http target")
	}
}

func TestParseUnsubscribe_ListIDPreferred(t *testing.T) {
	m := Message{Headers: map[string]string{
		"List-Unsubscribe": "<https://list.example/u/abc>",
		"List-Id":          "Marketing <promo.shop.example.com>",
	}}
	info, ok := ParseUnsubscribe(m)
	if !ok {
		t.Fatalf("expected info")
	}
	if info.ListID != "promo.shop.example.com" {
		t.Fatalf("ListID = %q, want bracketed List-Id token", info.ListID)
	}
}

func TestParseUnsubscribe_ListIDFallsBackToTarget(t *testing.T) {
	m := Message{Headers: map[string]string{
		"List-Unsubscribe": "<https://list.example/u/abc>",
	}}
	info, _ := ParseUnsubscribe(m)
	if info.ListID != "https://list.example/u/abc" {
		t.Fatalf("ListID fallback = %q", info.ListID)
	}
}

func TestParseUnsubscribe_GarbageHeader(t *testing.T) {
	m := Message{Headers: map[string]string{"List-Unsubscribe": "not-a-bracketed-uri"}}
	if _, ok := ParseUnsubscribe(m); ok {
		t.Fatalf("expected no actionable info for unparseable header")
	}
}
