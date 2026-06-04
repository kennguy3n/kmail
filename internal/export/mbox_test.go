package export

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestMboxWriter_WriteMessage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewMboxWriter(&buf)

	raw := []byte("Subject: Hello\r\nFrom: alice@example.com\r\n\r\nHi there.\r\n")
	when := time.Date(2024, time.March, 2, 13, 4, 5, 0, time.UTC)
	if err := w.WriteMessage("alice@example.com", when, raw); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got := buf.String()
	wantSep := "From alice@example.com Sat Mar  2 13:04:05 2024\n"
	if !strings.HasPrefix(got, wantSep) {
		t.Fatalf("missing/incorrect separator line.\n got: %q\nwant prefix: %q", got, wantSep)
	}
	if !strings.Contains(got, "Subject: Hello") || !strings.Contains(got, "Hi there.") {
		t.Fatalf("message body not preserved: %q", got)
	}
	// Entry must end with a blank-line separator.
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("entry not terminated by blank line: %q", got)
	}
}

func TestMboxWriter_QuotesFromLines(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewMboxWriter(&buf)

	// Body contains lines that must be ">"-quoted (mboxrd): a bare
	// "From " line and an already-quoted ">From " line. A "Fromage"
	// line must NOT be quoted (no trailing space).
	raw := []byte("H: 1\n\nFrom the desk of Bob\n>From earlier\nFromage is cheese\n")
	if err := w.WriteMessage("bob@example.com", time.Unix(0, 0).UTC(), raw); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "\n>From the desk of Bob\n") {
		t.Errorf("bare 'From ' line not quoted: %q", got)
	}
	if !strings.Contains(got, "\n>>From earlier\n") {
		t.Errorf("'>From ' line not re-quoted (mboxrd): %q", got)
	}
	if strings.Contains(got, "\n>Fromage is cheese\n") {
		t.Errorf("'Fromage' line wrongly quoted: %q", got)
	}
}

func TestMboxWriter_RoundTripReversesQuoting(t *testing.T) {
	t.Parallel()

	// mboxrd quoting must be reversible: stripping exactly one
	// leading ">" from every >*From line recovers the body.
	var buf bytes.Buffer
	w := NewMboxWriter(&buf)
	body := "From a\n>From b\ntext\n"
	raw := []byte("X: y\n\n" + body)
	if err := w.WriteMessage("s@x", time.Unix(0, 0).UTC(), raw); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	out := buf.String()
	// Drop the "From " separator line and the trailing blank line,
	// then unquote.
	idx := strings.IndexByte(out, '\n')
	rest := out[idx+1:]
	rest = strings.TrimSuffix(rest, "\n") // trailing separator blank line
	unq := unquoteFrom(rest)
	if !strings.HasSuffix(unq, body) {
		t.Fatalf("round-trip failed.\n got: %q\nwant suffix: %q", unq, body)
	}
}

func TestMboxWriter_EmptyFromFallsBackToPlaceholder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewMboxWriter(&buf)
	if err := w.WriteMessage("", time.Unix(0, 0).UTC(), []byte("X: y\n\nbody")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "From "+mboxFromPlaceholder+" ") {
		t.Fatalf("placeholder sender not used: %q", buf.String())
	}
	// Body without trailing newline must still be newline-terminated
	// before the blank-line separator.
	if !strings.HasSuffix(buf.String(), "body\n\n") {
		t.Fatalf("missing newline normalisation: %q", buf.String())
	}
}

func TestMboxWriter_StripsLineBreaksInSender(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewMboxWriter(&buf)
	// A crafted sender must not be able to inject a second "From "
	// boundary via embedded newlines.
	if err := w.WriteMessage("evil\nFrom attacker@x", time.Unix(0, 0).UTC(), []byte("\r\nbody\n")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	firstLine := strings.SplitN(buf.String(), "\n", 2)[0]
	if !strings.Contains(firstLine, "evilFrom attacker@x") {
		t.Fatalf("newlines not stripped from sender: %q", firstLine)
	}
	// The security property: stripping the newline means no NEW
	// message boundary (a line *beginning* with "From ") is forged
	// by the body. Exactly one line starts with "From ".
	boundaries := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "From ") {
			boundaries++
		}
	}
	if boundaries != 1 {
		t.Fatalf("expected exactly one 'From ' boundary line, got %d: %q", boundaries, buf.String())
	}
}

// unquoteFrom reverses mboxrd quoting: strips one leading ">" from
// every line matching ^>+From . Test helper only.
func unquoteFrom(s string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		i := 0
		for i < len(line) && line[i] == '>' {
			i++
		}
		if i > 0 && strings.HasPrefix(line[i:], "From ") {
			b.WriteString(line[1:])
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}
