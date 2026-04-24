package migration

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The validation paths in every Create* method and the
// state-transition checks in StartJob / CancelJob return before
// touching the pool, so a nil-pool Service is sufficient for
// these input-validation unit tests.

func newTestService() *Service {
	return NewService(Config{
		Pool:          nil,
		MaxConcurrent: 2,
		Now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
}

// ------------------------------------------------------------------
// Input validation
// ------------------------------------------------------------------

func TestCreateJob_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		in   CreateJobInput
	}{
		{"empty", CreateJobInput{}},
		{"no source host", CreateJobInput{
			SourceUser:     "u",
			SourcePassword: "p",
			DestUser:       "d",
		}},
		{"no source user", CreateJobInput{
			SourceHost:     "imap.example.com",
			SourcePassword: "p",
			DestUser:       "d",
		}},
		{"no source password", CreateJobInput{
			SourceHost: "imap.example.com",
			SourceUser: "u",
			DestUser:   "d",
		}},
		{"no dest user", CreateJobInput{
			SourceHost:     "imap.example.com",
			SourceUser:     "u",
			SourcePassword: "p",
		}},
	}
	svc := newTestService()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateJob(context.Background(), "tid", tc.in)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestCreateJob_MissingTenantID(t *testing.T) {
	_, err := newTestService().CreateJob(context.Background(), "", CreateJobInput{
		SourceHost:     "imap.example.com",
		SourceUser:     "u",
		SourcePassword: "p",
		DestUser:       "d",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestGetJob_EmptyJobID(t *testing.T) {
	_, err := newTestService().GetJob(context.Background(), "tid", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty job id, got %v", err)
	}
}

// ------------------------------------------------------------------
// Lifecycle helpers
// ------------------------------------------------------------------

func TestMigrationJob_Terminal(t *testing.T) {
	cases := map[string]bool{
		"pending":   false,
		"running":   false,
		"paused":    false,
		"completed": true,
		"failed":    true,
		"cancelled": true,
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			j := &MigrationJob{Status: status}
			if got := j.Terminal(); got != want {
				t.Errorf("Terminal() for %q = %v, want %v", status, got, want)
			}
		})
	}
}

// ------------------------------------------------------------------
// Password encoding round-trip
// ------------------------------------------------------------------

func TestEncryptDecryptPassword_RoundTrip(t *testing.T) {
	enc, err := encryptPassword("hunter2")
	if err != nil {
		t.Fatalf("encryptPassword: %v", err)
	}
	if enc == "hunter2" {
		t.Errorf("encryptPassword returned plaintext")
	}
	got, err := decryptPassword(enc)
	if err != nil {
		t.Fatalf("decryptPassword: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("round-trip: got %q, want %q", got, "hunter2")
	}
}

func TestDecryptPassword_RejectsUnknownEncoding(t *testing.T) {
	_, err := decryptPassword("not-a-kmail-enc")
	if err == nil {
		t.Fatal("expected error for unknown encoding")
	}
}

func TestEncryptPassword_EmptyInput(t *testing.T) {
	enc, err := encryptPassword("")
	if err != nil {
		t.Fatalf("encryptPassword: %v", err)
	}
	if enc != "" {
		t.Errorf("empty input should produce empty ciphertext, got %q", enc)
	}
}

// ------------------------------------------------------------------
// Progress regex
// ------------------------------------------------------------------

func TestImapsyncProgressRegex(t *testing.T) {
	line := "++++ Statistics : Folder [INBOX] Messages 1234 of 2345 done"
	m := imapsyncProgressRE.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("expected regex to match: %q", line)
	}
	if m[1] != "1234" || m[2] != "2345" {
		t.Errorf("captures = %v, want [1234 2345]", m[1:])
	}
}

func TestImapsyncProgressRegex_NoMatch(t *testing.T) {
	line := "some unrelated imapsync output"
	if m := imapsyncProgressRE.FindStringSubmatch(line); m != nil {
		t.Errorf("expected no match, got %v", m)
	}
}

// ------------------------------------------------------------------
// NewService defaults
// ------------------------------------------------------------------

func TestNewService_AppliesDefaults(t *testing.T) {
	s := NewService(Config{})
	if s.cfg.MaxConcurrent <= 0 {
		t.Errorf("MaxConcurrent default not applied")
	}
	if s.cfg.ImapsyncBin == "" {
		t.Errorf("ImapsyncBin default not applied")
	}
	if s.cfg.Now == nil {
		t.Errorf("Now default not applied")
	}
	if s.sema == nil {
		t.Errorf("sema not initialised")
	}
	if s.cancels == nil {
		t.Errorf("cancels map not initialised")
	}
}
