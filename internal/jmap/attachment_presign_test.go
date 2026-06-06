package jmap

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGeneratePresignedURLUnconfigured(t *testing.T) {
	// Missing S3 config → error.
	s := NewAttachmentService(AttachmentConfig{})
	if _, err := s.GeneratePresignedURL("key", time.Hour); err == nil {
		t.Error("expected error when S3 endpoint not configured")
	}
}

func TestGeneratePresignedURLSignsRequest(t *testing.T) {
	s := NewAttachmentService(AttachmentConfig{
		S3URL:     "https://s3.example.com",
		Bucket:    "mybucket",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secretkey",
		Region:    "us-west-2",
	})
	got, err := s.GeneratePresignedURL("path/to/file.bin", time.Hour)
	if err != nil {
		t.Fatalf("GeneratePresignedURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if u.Host != "s3.example.com" || u.Path != "/mybucket/path/to/file.bin" {
		t.Fatalf("unexpected host/path: %s", got)
	}
	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Errorf("algorithm=%q", q.Get("X-Amz-Algorithm"))
	}
	if !strings.HasPrefix(q.Get("X-Amz-Credential"), "AKIAEXAMPLE/") {
		t.Errorf("credential=%q", q.Get("X-Amz-Credential"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Error("missing signature")
	}
	if q.Get("X-Amz-Expires") != "3600" {
		t.Errorf("expires=%q want 3600", q.Get("X-Amz-Expires"))
	}
}

func TestGeneratePresignedURLClampsExpiry(t *testing.T) {
	s := NewAttachmentService(AttachmentConfig{
		S3URL:     "https://s3.example.com",
		Bucket:    "b",
		AccessKey: "AK",
		SecretKey: "sk",
	})
	// Expiry above the 7-day cap is clamped.
	got, err := s.GeneratePresignedURL("k", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("GeneratePresignedURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("X-Amz-Expires") != "604800" {
		t.Errorf("expires=%q want clamped to 604800", u.Query().Get("X-Amz-Expires"))
	}
}

func TestCanonicalQueryStringSorted(t *testing.T) {
	q := url.Values{}
	q.Set("b", "2")
	q.Set("a", "1")
	q.Set("c", "x y")
	got := canonicalQueryString(q)
	if got != "a=1&b=2&c=x%20y" {
		t.Errorf("canonicalQueryString=%q", got)
	}
}

func TestAWSEscapeSpaces(t *testing.T) {
	if got := awsEscape("a b+c"); got != "a%20b%2Bc" {
		t.Errorf("awsEscape=%q want a%%20b%%2Bc", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"normal.txt", "normal.txt"},
		{"../../etc/passwd", ".._.._etc_passwd"},
		{"with space.pdf", "with_space.pdf"},
		{"", "attachment"},
		{"weird*chars?.png", "weird_chars_.png"},
	}
	for _, tc := range tests {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	// Long names are truncated to 128 chars.
	long := strings.Repeat("a", 200)
	if got := sanitizeFilename(long); len(got) != 128 {
		t.Errorf("long filename len=%d want 128", len(got))
	}
}

func TestDeriveSigningKeyDeterministic(t *testing.T) {
	k1 := deriveSigningKey("secret", "20240101", "us-east-1", "s3")
	k2 := deriveSigningKey("secret", "20240101", "us-east-1", "s3")
	if string(k1) != string(k2) {
		t.Error("deriveSigningKey not deterministic")
	}
	k3 := deriveSigningKey("secret", "20240102", "us-east-1", "s3")
	if string(k1) == string(k3) {
		t.Error("deriveSigningKey should differ by date")
	}
}
