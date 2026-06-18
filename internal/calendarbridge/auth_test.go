package calendarbridge

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// stubMinter records the principal it was asked to mint for and
// returns a canned token (or error) so the three-armed switch in
// setAuth can be exercised without a real signer.
type stubMinter struct {
	token   string
	err     error
	calls   int
	lastFor string
}

func (m *stubMinter) Mint(principal string) (string, error) {
	m.calls++
	m.lastFor = principal
	if m.err != nil {
		return "", m.err
	}
	return m.token, nil
}

func newAuthReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://stalwart/dav/cal/alice@kmail.dev/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// TestSetAuth_AdminBasicPrecedence verifies arm 1: when admin Basic
// is configured it is used and the minter is never consulted, even
// if one is set (dev/CI must never mint).
func TestSetAuth_AdminBasicPrecedence(t *testing.T) {
	m := &stubMinter{token: "should-not-be-used"}
	svc := NewService(Config{StalwartURL: "http://stalwart", AdminUser: "admin", AdminPassword: "pw", Minter: m})
	req := newAuthReq(t)
	if err := svc.setAuth(context.Background(), req, "alice@kmail.dev"); err != nil {
		t.Fatalf("setAuth: %v", err)
	}
	user, pass, ok := req.BasicAuth()
	if !ok || user != "admin" || pass != "pw" {
		t.Fatalf("expected admin Basic creds, got user=%q pass=%q ok=%v", user, pass, ok)
	}
	if req.Header.Get("Authorization") == "" {
		t.Fatal("expected Authorization header")
	}
	if m.calls != 0 {
		t.Fatalf("minter must not be called when admin Basic is set, calls=%d", m.calls)
	}
}

// TestSetAuth_MinterMints verifies arm 2: with no admin creds and a
// minter set, the bridge mints a bearer for the resolved principal
// (the account id, unchanged without admin creds) and forwards it.
func TestSetAuth_MinterMints(t *testing.T) {
	m := &stubMinter{token: "tok-123"}
	svc := NewService(Config{StalwartURL: "http://stalwart", Minter: m})
	req := newAuthReq(t)
	if err := svc.setAuth(context.Background(), req, "alice@kmail.dev"); err != nil {
		t.Fatalf("setAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok-123")
	}
	if m.calls != 1 {
		t.Fatalf("expected exactly one mint, got %d", m.calls)
	}
	if m.lastFor != "alice@kmail.dev" {
		t.Fatalf("minted for principal %q, want %q", m.lastFor, "alice@kmail.dev")
	}
}

// TestSetAuth_MinterErrorFailsClosed verifies arm 2 fails closed: a
// mint error is returned to the caller (no unauthenticated request).
func TestSetAuth_MinterErrorFailsClosed(t *testing.T) {
	wantErr := errors.New("mint boom")
	m := &stubMinter{err: wantErr}
	svc := NewService(Config{StalwartURL: "http://stalwart", Minter: m})
	req := newAuthReq(t)
	err := svc.setAuth(context.Background(), req, "alice@kmail.dev")
	if err == nil {
		t.Fatal("expected fail-closed error on mint failure")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error %v does not wrap %v", err, wantErr)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("no Authorization header must be set on mint failure")
	}
}

// TestSetAuth_MinterEmptyPrincipalFailsClosed verifies arm 2 refuses
// to mint an unscoped token when the resolved principal is empty.
func TestSetAuth_MinterEmptyPrincipalFailsClosed(t *testing.T) {
	m := &stubMinter{token: "tok"}
	svc := NewService(Config{StalwartURL: "http://stalwart", Minter: m})
	req := newAuthReq(t)
	if err := svc.setAuth(context.Background(), req, ""); err == nil {
		t.Fatal("expected fail-closed error on empty principal")
	}
	if m.calls != 0 {
		t.Fatalf("minter must not be called with empty principal, calls=%d", m.calls)
	}
}

// TestSetAuth_LegacyMTLSNoHeader verifies arm 3: with neither admin
// creds nor a minter, the bridge sets no Authorization header and
// defers to the mTLS / trusted-network posture.
func TestSetAuth_LegacyMTLSNoHeader(t *testing.T) {
	svc := NewService(Config{StalwartURL: "http://stalwart"})
	req := newAuthReq(t)
	if err := svc.setAuth(context.Background(), req, "alice@kmail.dev"); err != nil {
		t.Fatalf("setAuth: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("expected no Authorization header, got %q", got)
	}
}
