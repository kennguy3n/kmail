package smartfeatures

import (
	"strings"
)

// UnsubscribeInfo is the parsed result of a message's
// List-Unsubscribe / List-Unsubscribe-Post headers (RFC 2369,
// RFC 8058).
type UnsubscribeInfo struct {
	// HTTPSURLs are the https(s) targets the client may POST (when
	// OneClick) or open in a new tab.
	HTTPURLs []string `json:"http_urls"`
	// MailtoURLs are mailto: targets the client opens to send an
	// unsubscribe email.
	MailtoURLs []string `json:"mailto_urls"`
	// OneClick is true when the sender advertised RFC 8058
	// one-click support (List-Unsubscribe-Post: List-Unsubscribe=One-Click).
	// Only then is a server-initiated POST safe; otherwise a POST
	// could be a CSRF-style action the sender did not intend.
	OneClick bool `json:"one_click"`
	// ListID is the stable identifier (List-Id when present, else
	// the first http/mailto target) used to remember that this user
	// unsubscribed from this list.
	ListID string `json:"list_id"`
}

// PreferredHTTP returns the first HTTP(S) unsubscribe target, if
// any. This is the target used for an RFC 8058 one-click POST.
func (u *UnsubscribeInfo) PreferredHTTP() (string, bool) {
	if u == nil || len(u.HTTPURLs) == 0 {
		return "", false
	}
	return u.HTTPURLs[0], true
}

// PreferredMailto returns the first mailto: unsubscribe target.
func (u *UnsubscribeInfo) PreferredMailto() (string, bool) {
	if u == nil || len(u.MailtoURLs) == 0 {
		return "", false
	}
	return u.MailtoURLs[0], true
}

// ParseUnsubscribe extracts unsubscribe targets from a message.
// The second return value is false when the message advertises no
// List-Unsubscribe header at all (i.e. there is no unsubscribe
// affordance to show).
//
// The List-Unsubscribe header is a comma-separated list of
// angle-bracket-wrapped URIs (RFC 2369 §3.2), e.g.:
//
//	List-Unsubscribe: <mailto:u@list.example?subject=unsub>, <https://list.example/u/abc>
//
// One-click is only reported when List-Unsubscribe-Post carries
// the exact RFC 8058 token AND at least one https target exists —
// a one-click POST to a mailto target is meaningless.
func ParseUnsubscribe(m Message) (*UnsubscribeInfo, bool) {
	raw := strings.TrimSpace(m.Header("List-Unsubscribe"))
	if raw == "" {
		return nil, false
	}

	info := &UnsubscribeInfo{}
	for _, uri := range parseAngleList(raw) {
		switch {
		case strings.HasPrefix(strings.ToLower(uri), "http://"),
			strings.HasPrefix(strings.ToLower(uri), "https://"):
			info.HTTPURLs = append(info.HTTPURLs, uri)
		case strings.HasPrefix(strings.ToLower(uri), "mailto:"):
			info.MailtoURLs = append(info.MailtoURLs, uri)
		}
	}

	if len(info.HTTPURLs) == 0 && len(info.MailtoURLs) == 0 {
		// Header present but unparseable — surface it so the UI can
		// still offer a (degraded) manual path, but with no targets
		// there is nothing actionable.
		return nil, false
	}

	post := strings.ToLower(strings.TrimSpace(m.Header("List-Unsubscribe-Post")))
	// RFC 8058 §3.1: the value is exactly "List-Unsubscribe=One-Click".
	info.OneClick = post == "list-unsubscribe=one-click" && len(info.HTTPURLs) > 0

	info.ListID = unsubscribeListID(m, info)
	return info, true
}

// unsubscribeListID derives a stable key for "this user already
// unsubscribed from this list". List-Id (RFC 2919) is preferred
// because it is the sender's own stable identifier; otherwise we
// fall back to the first concrete unsubscribe target so the key is
// still deterministic across messages from the same campaign.
func unsubscribeListID(m Message, info *UnsubscribeInfo) string {
	if lid := strings.TrimSpace(m.Header("List-Id")); lid != "" {
		// List-Id is often "Human name <list.id.example.com>"; the
		// bracketed token is the stable part.
		if inner, ok := firstAngle(lid); ok {
			return strings.ToLower(inner)
		}
		return strings.ToLower(lid)
	}
	if len(info.HTTPURLs) > 0 {
		return strings.ToLower(info.HTTPURLs[0])
	}
	if len(info.MailtoURLs) > 0 {
		return strings.ToLower(info.MailtoURLs[0])
	}
	return ""
}

// parseAngleList splits an RFC 2369 angle-bracket list into the
// inner URI strings, tolerating missing/extra whitespace.
func parseAngleList(raw string) []string {
	var out []string
	rest := raw
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			break
		}
		close := strings.IndexByte(rest[open+1:], '>')
		if close < 0 {
			break
		}
		uri := strings.TrimSpace(rest[open+1 : open+1+close])
		if uri != "" {
			out = append(out, uri)
		}
		rest = rest[open+1+close+1:]
	}
	return out
}

// firstAngle returns the contents of the first <...> token.
func firstAngle(raw string) (string, bool) {
	open := strings.IndexByte(raw, '<')
	if open < 0 {
		return "", false
	}
	close := strings.IndexByte(raw[open+1:], '>')
	if close < 0 {
		return "", false
	}
	return strings.TrimSpace(raw[open+1 : open+1+close]), true
}
