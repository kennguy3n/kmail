package valkeyurl

import (
	"strings"
	"testing"
)

func TestParse_BareHostPort(t *testing.T) {
	opts, err := Parse("valkey:6379")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if opts.Addr != "valkey:6379" {
		t.Errorf("Addr = %q, want %q", opts.Addr, "valkey:6379")
	}
}

func TestParse_RedisURL(t *testing.T) {
	opts, err := Parse("redis://valkey:6379/2")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if opts.Addr != "valkey:6379" {
		t.Errorf("Addr = %q, want %q", opts.Addr, "valkey:6379")
	}
	if opts.DB != 2 {
		t.Errorf("DB = %d, want 2", opts.DB)
	}
}

func TestParse_RedissURL_TLSCertificatesOK(t *testing.T) {
	opts, err := Parse("rediss://user:pw@valkey.example.com:6380")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if opts.Addr != "valkey.example.com:6380" {
		t.Errorf("Addr = %q, want valkey.example.com:6380", opts.Addr)
	}
	if opts.TLSConfig == nil {
		t.Error("expected non-nil TLSConfig for rediss:// URL")
	}
	if opts.Username != "user" || opts.Password != "pw" {
		t.Errorf("creds = %q/%q, want user/pw", opts.Username, opts.Password)
	}
}

func TestParse_Empty(t *testing.T) {
	_, err := Parse("")
	if err == nil {
		t.Fatal("expected error on empty url")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want \"empty\" in message", err)
	}
}

// TestParse_BareHostPort_RegressionForKMailValkeyURLBug pins the
// specific bug surfaced in PR #31 round-7: when the Helm Secret
// ships `KMAIL_VALKEY_URL=redis://valkey:6379` (the chart default),
// `redis.NewClient(&redis.Options{Addr: rawURL})` would try to
// resolve "redis://valkey" as a DNS name and fail. Parsing the URL
// first yields Addr=valkey:6379 which is what redis.Options expects.
func TestParse_BareHostPort_RegressionForKMailValkeyURLBug(t *testing.T) {
	opts, err := Parse("redis://valkey:6379")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.Contains(opts.Addr, "://") {
		t.Errorf("Addr must NOT contain scheme (got %q); redis.NewClient will fail DNS lookup", opts.Addr)
	}
	if opts.Addr != "valkey:6379" {
		t.Errorf("Addr = %q, want valkey:6379", opts.Addr)
	}
}
