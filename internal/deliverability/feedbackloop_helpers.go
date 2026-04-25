package deliverability

import (
	"bytes"
	"strings"
)

// splitARFLines splits the ARF block body on bare `\n` or `\r\n`
// boundaries and trims trailing whitespace. Empty lines are
// dropped so the caller can tokenise header-style rows without a
// separate state machine.
func splitARFLines(body []byte) []string {
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	raw := strings.Split(string(body), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// splitHeader splits "Name: Value" into a normalised lowercase
// name and trimmed value. Lines without a colon produce ("", "").
func splitHeader(line string) (string, string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", ""
	}
	return strings.ToLower(strings.TrimSpace(line[:idx])), strings.TrimSpace(line[idx+1:])
}

// deriveDomainFromEmail returns everything after the `@` in an
// email, lowercased, or the empty string when the input is not an
// addr-spec.
func deriveDomainFromEmail(addr string) string {
	idx := strings.LastIndexByte(addr, '@')
	if idx < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(addr[idx+1:]))
}
