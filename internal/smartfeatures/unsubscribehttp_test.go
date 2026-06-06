package smartfeatures

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsGlobalUnicast_BlocksNonPublicRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",         // loopback
		"10.0.0.5",          // RFC 1918
		"192.168.1.1",       // RFC 1918
		"169.254.169.254",   // link-local (cloud metadata)
		"100.64.0.1",        // RFC 6598 CGN / shared address space
		"100.127.255.254",   // RFC 6598 upper edge
		"::ffff:100.64.0.1", // IPv4-mapped CGN must also be blocked
		"fd00::1",           // ULA (IsPrivate)
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if isGlobalUnicast(ip) {
			t.Errorf("isGlobalUnicast(%s) = true, want false (non-public)", s)
		}
	}

	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range public {
		ip := net.ParseIP(s)
		if !isGlobalUnicast(ip) {
			t.Errorf("isGlobalUnicast(%s) = false, want true (public)", s)
		}
	}
}

func TestSafeOneClick_RejectsNonHTTPS(t *testing.T) {
	s := NewSafeOneClickUnsubscriber(0)
	err := s.Post(context.Background(), "http://list.example/u")
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("expected non-https rejection, got %v", err)
	}
}

func TestSafeOneClick_RejectsPrivateAddress(t *testing.T) {
	s := NewSafeOneClickUnsubscriber(0)
	// Resolves to loopback / private → guarded at dial time.
	for _, target := range []string{
		"https://127.0.0.1/u",
		"https://10.0.0.5/u",
		"https://169.254.169.254/latest/meta-data", // cloud metadata SSRF classic
	} {
		if err := s.Post(context.Background(), target); err == nil {
			t.Fatalf("expected SSRF guard to reject %q", target)
		}
	}
}

func TestSafeOneClick_PostsForm(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// httptest TLS server listens on 127.0.0.1, which the SSRF guard
	// would normally block; use the server's own client (trusts the
	// test cert) wrapped so we bypass the guard for this unit test.
	s := &SafeOneClickUnsubscriber{client: srv.Client()}
	if err := s.Post(context.Background(), srv.URL); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotBody != "List-Unsubscribe=One-Click" {
		t.Fatalf("body = %q", gotBody)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type = %q", gotCT)
	}
}
