// Package smartfeatures implements KMail's rule-based "smart"
// intelligence layer: smart-reply suggestions, Gmail-style email
// categorization, frequent-contact tracking, and the
// List-Unsubscribe helper.
//
// The design goal is to close the obvious feature gap with
// Gmail / M365 *without* requiring an ML pipeline. Every signal
// in this package is derived from message metadata (subject,
// preview text, a handful of RFC 5322 / RFC 2369 headers) using
// deterministic rules, so the whole core is pure and trivially
// unit-testable. The Phase-2 hooks (Ollama / OpenAI generative
// replies) are intentionally gated behind a separate, optional
// provider so the rule engine remains the always-available
// default.
//
// The HTTP surface (handlers.go) is the only part that touches
// Stalwart (via the JMAP InternalClient) or Valkey. Everything in
// this file and its rule siblings operates on the in-memory
// Message struct below.
package smartfeatures

import (
	"net/mail"
	"strings"
	"time"
)

// Address is a parsed RFC 5322 mailbox. It mirrors the JMAP
// EmailAddress object (RFC 8621 §4.1.2) but is decoupled from the
// jmap package so the rule engine has no upstream import.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Domain returns the lower-cased domain part of the address, or
// "" when the address has no "@".
func (a Address) Domain() string {
	at := strings.LastIndex(a.Email, "@")
	if at < 0 || at == len(a.Email)-1 {
		return ""
	}
	return strings.ToLower(a.Email[at+1:])
}

// Normalized returns the lower-cased, trimmed email address. Used
// as the stable key for frequency tracking and contact lookups so
// "Alice@Example.com" and "alice@example.com" collapse to one
// entry.
func (a Address) Normalized() string {
	return strings.ToLower(strings.TrimSpace(a.Email))
}

// Message is the parsed metadata view of a single email that every
// rule engine in this package consumes. It deliberately carries
// only what the rules need — there is no body blob here, just the
// short JMAP `preview` snippet plus the structured address fields
// and the headers the rules inspect.
type Message struct {
	ID         string
	ThreadID   string
	From       []Address
	To         []Address
	Cc         []Address
	Subject    string
	Preview    string
	Headers    map[string]string
	Keywords   map[string]bool
	ReceivedAt time.Time
}

// FirstFrom returns the first From address and whether one exists.
func (m Message) FirstFrom() (Address, bool) {
	if len(m.From) == 0 {
		return Address{}, false
	}
	return m.From[0], true
}

// Header looks a header up case-insensitively. RFC 5322 header
// field names are case-insensitive, and JMAP `header:Name` props
// preserve the sender's original casing, so callers must not rely
// on an exact-case map hit.
func (m Message) Header(name string) string {
	if m.Headers == nil {
		return ""
	}
	if v, ok := m.Headers[name]; ok {
		return v
	}
	lower := strings.ToLower(name)
	for k, v := range m.Headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

// HasHeader reports whether a (case-insensitive) header is present
// and non-empty.
func (m Message) HasHeader(name string) bool {
	return strings.TrimSpace(m.Header(name)) != ""
}

// searchText is the lower-cased concatenation of the subject and
// preview that the smart-reply and categorization rules match
// against. Computed once per call rather than per rule.
func (m Message) searchText() string {
	var b strings.Builder
	b.Grow(len(m.Subject) + len(m.Preview) + 1)
	b.WriteString(strings.ToLower(m.Subject))
	b.WriteByte(' ')
	b.WriteString(strings.ToLower(m.Preview))
	return b.String()
}

// ParseAddressList parses an RFC 5322 address-list header value
// (e.g. the raw `From` / `To` header) into Address values. Invalid
// fragments are skipped rather than failing the whole parse, since
// a single malformed addressee in a header must not blank out an
// entire smart-reply or categorization result. A best-effort
// fallback handles the bare "a@b, c@d" shape that `mail.ParseAddressList`
// rejects when display names are unquoted.
func ParseAddressList(raw string) []Address {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if parsed, err := mail.ParseAddressList(raw); err == nil {
		out := make([]Address, 0, len(parsed))
		for _, a := range parsed {
			out = append(out, Address{Name: a.Name, Email: a.Address})
		}
		return out
	}
	// Fallback: split on commas and salvage anything address-shaped.
	var out []Address
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if a, err := mail.ParseAddress(part); err == nil {
			out = append(out, Address{Name: a.Name, Email: a.Address})
			continue
		}
		if strings.Contains(part, "@") && !strings.ContainsAny(part, " <>") {
			out = append(out, Address{Email: part})
		}
	}
	return out
}
