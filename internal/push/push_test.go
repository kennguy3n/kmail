package push

import (
	"testing"
	"time"
)

func TestInQuietHoursWrap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		now     string
		inQuiet bool
	}{
		{"2026-04-25T23:00:00Z", true},
		{"2026-04-25T05:00:00Z", true},
		{"2026-04-25T12:00:00Z", false},
		{"2026-04-25T21:59:00Z", false},
	}
	for _, tc := range cases {
		n, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got := inQuietHours(n, "22:00", "06:00"); got != tc.inQuiet {
			t.Errorf("inQuietHours(%s)=%v want %v", tc.now, got, tc.inQuiet)
		}
	}
}

func TestInQuietHoursNonWrap(t *testing.T) {
	t.Parallel()
	n, _ := time.Parse(time.RFC3339, "2026-04-25T13:30:00Z")
	if !inQuietHours(n, "13:00", "14:00") {
		t.Errorf("expected 13:30 to be in quiet hours 13-14")
	}
	n2, _ := time.Parse(time.RFC3339, "2026-04-25T15:00:00Z")
	if inQuietHours(n2, "13:00", "14:00") {
		t.Errorf("expected 15:00 to be outside quiet hours 13-14")
	}
}
