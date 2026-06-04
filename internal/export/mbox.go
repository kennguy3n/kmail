// Package export — RFC 4155 mbox writer.
//
// mbox.go implements the minimal slice of the mbox family
// (RFC 4155, "The application/mbox Media Type") the eDiscovery
// export runner needs to serialise a stream of RFC 5322 messages
// into a single mbox file.
//
// The writer emits the `mboxrd` variant of "From " quoting: any
// body line that already consists of zero or more ">" characters
// followed by "From " has one extra ">" prepended. mboxrd is
// chosen over plain mbox because it is *reversible* — a reader can
// strip exactly one leading ">" from every `>*From ` line and
// recover the original bytes, which matters for an export whose
// whole point is faithful reproduction of the source mail.
package export

import (
	"bytes"
	"fmt"
	"io"
	"time"
)

// mboxFromPlaceholder is used as the envelope sender on the mbox
// "From " separator line when a message carries no usable From
// address. "MAILER-DAEMON" is the conventional placeholder
// (RFC 4155 §2 references the traditional Unix mbox "From " line,
// whose first token is the envelope sender).
const mboxFromPlaceholder = "MAILER-DAEMON"

// mboxDateLayout is the ctime/asctime layout used on the mbox
// "From " separator line (RFC 4155 §2): "Mon Jan _2 15:04:05 2006".
const mboxDateLayout = "Mon Jan _2 15:04:05 2006"

// Writer serialises messages into an mbox stream. It is not safe
// for concurrent use; serialise writes through one goroutine.
type Writer struct {
	w io.Writer
}

// NewMboxWriter returns a Writer that appends messages to w.
func NewMboxWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteMessage appends one message to the mbox stream.
//
// `from` is the envelope sender written verbatim onto the "From "
// separator line (a bare address, no angle brackets); an empty
// value falls back to MAILER-DAEMON. A zero `receivedAt` falls
// back to the current time so the separator line is always
// well-formed. `rawRFC5322` is the complete message (headers,
// blank line, body) exactly as fetched from the server.
//
// The message body is "From "-quoted per the mboxrd convention and
// the entry is terminated with a blank line so the next "From "
// separator unambiguously starts a new message.
func (m *Writer) WriteMessage(from string, receivedAt time.Time, rawRFC5322 []byte) error {
	if from == "" {
		from = mboxFromPlaceholder
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	// RFC 4155 forbids embedded CR/LF in the "From " line; the
	// sender token is a bare address so newlines would only appear
	// via malformed input — strip them defensively so a crafted
	// address cannot forge a message boundary.
	from = stripLineBreaks(from)

	if _, err := fmt.Fprintf(m.w, "From %s %s\n", from, receivedAt.UTC().Format(mboxDateLayout)); err != nil {
		return err
	}
	if _, err := m.w.Write(quoteFromLines(rawRFC5322)); err != nil {
		return err
	}
	// Guarantee the body ends on a line boundary before the
	// trailing blank-line separator, even when the source message
	// did not end with a newline.
	if n := len(rawRFC5322); n == 0 || rawRFC5322[n-1] != '\n' {
		if _, err := io.WriteString(m.w, "\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(m.w, "\n")
	return err
}

// quoteFromLines returns body with the mboxrd ">From " quoting
// applied: every line matching `^>*From ` gets one extra leading
// ">". Line terminators (LF or CRLF) are preserved exactly.
func quoteFromLines(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var out bytes.Buffer
	out.Grow(len(body) + 16)
	// SplitAfter keeps each line's trailing "\n"; the final element
	// is "" when body ends with "\n" (skipped by isFromLine).
	for _, line := range bytes.SplitAfter(body, []byte("\n")) {
		if isFromLine(line) {
			out.WriteByte('>')
		}
		out.Write(line)
	}
	return out.Bytes()
}

// isFromLine reports whether line (which may retain a trailing LF
// or CRLF) begins with zero or more ">" followed by "From ".
func isFromLine(line []byte) bool {
	i := 0
	for i < len(line) && line[i] == '>' {
		i++
	}
	return bytes.HasPrefix(line[i:], []byte("From "))
}

// stripLineBreaks removes CR and LF bytes so a value cannot inject
// a spurious mbox message boundary.
func stripLineBreaks(s string) string {
	if !bytes.ContainsAny([]byte(s), "\r\n") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}
