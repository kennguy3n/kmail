package deliverability

import (
	"testing"
	"time"
)

// TestParseARF covers the ARF header parser across every recognised
// field plus the two accepted arrival-date formats and unknown lines.
func TestParseARF_AllFields(t *testing.T) {
	body := []byte(
		"Feedback-Type: abuse\r\n" +
			"Source-IP: 203.0.113.10\r\n" +
			"Original-Rcpt-To: victim@example.com\r\n" +
			"Original-Mail-From: sender@example.net\r\n" +
			"Reporting-MTA: dns; mx.example.org\r\n" +
			"User-Agent: SomeFBL/1.0\r\n" +
			"Authentication-Results: spf=pass\r\n" +
			"Source: example.net\r\n" +
			"Arrival-Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
			"X-Unknown-Header: ignored\r\n" +
			"\r\n",
	)
	r, err := ParseARF(body)
	if err != nil {
		t.Fatalf("ParseARF: %v", err)
	}
	if r.FeedbackType != "abuse" || r.SourceIP != "203.0.113.10" ||
		r.OriginalRcptTo != "victim@example.com" || r.OriginalMailID != "sender@example.net" ||
		r.ReportingMTA != "dns; mx.example.org" || r.UserAgent != "SomeFBL/1.0" ||
		r.AuthResults != "spf=pass" || r.SourceDomain != "example.net" {
		t.Errorf("ParseARF fields mismatch: %+v", r)
	}
	if r.ArrivalDate.IsZero() {
		t.Errorf("ArrivalDate not parsed (RFC1123Z)")
	}

	// RFC1123 (no numeric zone) is also accepted.
	r2, _ := ParseARF([]byte("Arrival-Date: Mon, 02 Jan 2006 15:04:05 MST\r\n"))
	if r2.ArrivalDate.IsZero() {
		t.Errorf("ArrivalDate not parsed (RFC1123)")
	}

	// Unparseable arrival-date leaves the zero value without error.
	r3, err := ParseARF([]byte("Arrival-Date: not-a-date\r\n"))
	if err != nil || !r3.ArrivalDate.Equal(time.Time{}) {
		t.Errorf("bad arrival-date r3=%+v err=%v", r3, err)
	}

	// Empty body parses to a zero report.
	if r4, err := ParseARF(nil); err != nil || r4.FeedbackType != "" {
		t.Errorf("empty ARF r4=%+v err=%v", r4, err)
	}
}
