package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------
// Input validation — every Create/List/Delete path pre-validates
// before touching the pool, so a nil-pool Service is sufficient to
// exercise the validation logic.
// ---------------------------------------------------------------

func TestCreateAlias_MissingTenantID(t *testing.T) {
	_, err := nilService().CreateAlias(context.Background(), "", CreateAliasInput{
		UserID: "uid", AliasEmail: "alias@example.com",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateAlias_MissingUserID(t *testing.T) {
	_, err := nilService().CreateAlias(context.Background(), "tid", CreateAliasInput{
		AliasEmail: "alias@example.com",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateAlias_MissingAliasEmail(t *testing.T) {
	_, err := nilService().CreateAlias(context.Background(), "tid", CreateAliasInput{
		UserID: "uid",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCreateAlias_InvalidAliasEmail(t *testing.T) {
	for _, bad := range []string{
		"no-at-sign",
		"@no-local-part",
		"local@",
		"two spaces in@local.com",
		"",
		"   ",
	} {
		t.Run(bad, func(t *testing.T) {
			_, err := nilService().CreateAlias(context.Background(), "tid", CreateAliasInput{
				UserID: "uid", AliasEmail: bad,
			})
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestListAliases_MissingTenantID(t *testing.T) {
	_, err := nilService().ListAliases(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestListUserAliases_MissingIDs(t *testing.T) {
	tests := []struct{ tenantID, userID string }{
		{"", "uid"},
		{"tid", ""},
		{"", ""},
	}
	for _, tc := range tests {
		_, err := nilService().ListUserAliases(context.Background(), tc.tenantID, tc.userID)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ListUserAliases(%q,%q): expected ErrInvalidInput, got %v",
				tc.tenantID, tc.userID, err)
		}
	}
}

func TestDeleteAlias_MissingIDs(t *testing.T) {
	tests := []struct{ tenantID, aliasID string }{
		{"", "aid"},
		{"tid", ""},
		{"", ""},
	}
	for _, tc := range tests {
		err := nilService().DeleteAlias(context.Background(), tc.tenantID, tc.aliasID)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("DeleteAlias(%q,%q): expected ErrInvalidInput, got %v",
				tc.tenantID, tc.aliasID, err)
		}
	}
}

// ---------------------------------------------------------------
// Email normalization — RFC 5321 §2.4 says SMTP local-part
// comparisons are case-insensitive in practice, so we lower-case
// at the boundary. Strip surrounding whitespace and accept the
// angle-bracketed form because `mail.ParseAddress` already does.
// ---------------------------------------------------------------

func TestNormalizeAliasEmail(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"alias@example.com", "alias@example.com"},
		{"ALIAS@EXAMPLE.com", "alias@example.com"},
		{"  alias@example.com  ", "alias@example.com"},
		{"Mixed.Case@Example.Org", "mixed.case@example.org"},
		// `mail.ParseAddress` accepts the display-name form;
		// the BFF stores only the bare address.
		{`"Alice" <alice@example.com>`, "alice@example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeAliasEmail(tc.in)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeAliasEmail_Invalid(t *testing.T) {
	for _, bad := range []string{
		"",
		"   ",
		"no-at-sign",
		"@no-local-part",
		"local@",
		"two spaces in@local.com",
	} {
		t.Run(bad, func(t *testing.T) {
			if _, err := normalizeAliasEmail(bad); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------
// Stalwart alias HTTP sync — verifies the wire format against a
// real httptest server. No pgx pool involved.
// ---------------------------------------------------------------

// stubResolver implements StalwartShardResolver for tests, returning
// the configured URL regardless of tenant id.
type stubResolver struct {
	url string
	err error
}

func (r *stubResolver) GetTenantShard(_ context.Context, _ string) (string, error) {
	return r.url, r.err
}

func TestStalwartAliasHTTPSync_AddAlias_WireShape(t *testing.T) {
	var seen atomic.Pointer[http.Request]
	var body atomic.Pointer[[]stalwartPrincipalOp]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		copy := r.Clone(r.Context())
		seen.Store(copy)
		var ops []stalwartPrincipalOp
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &ops); err != nil {
			t.Errorf("decode body: %v", err)
		}
		body.Store(&ops)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sync, err := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	if err := sync.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}

	req := seen.Load()
	if req == nil {
		t.Fatal("no request captured")
	}
	if req.Method != http.MethodPatch {
		t.Errorf("method = %s want PATCH", req.Method)
	}
	if got, want := req.URL.Path, "/api/principal/acc-1"; got != want {
		t.Errorf("path = %q want %q", got, want)
	}
	user, pass, ok := req.BasicAuth()
	if !ok || user != "admin" || pass != "secret" {
		t.Errorf("basic auth = (%q,%q,%v) want (admin,secret,true)", user, pass, ok)
	}
	ops := body.Load()
	if ops == nil || len(*ops) != 1 {
		t.Fatalf("ops = %v", ops)
	}
	op := (*ops)[0]
	if op.Action != "addItem" || op.Field != "emails" || op.Value != "alias@example.com" {
		t.Errorf("op = %+v", op)
	}
}

func TestStalwartAliasHTTPSync_RemoveAlias_WireShape(t *testing.T) {
	var body atomic.Pointer[[]stalwartPrincipalOp]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ops []stalwartPrincipalOp
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &ops); err != nil {
			t.Errorf("decode body: %v", err)
		}
		body.Store(&ops)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sync, err := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	if err := sync.RemoveAlias(context.Background(), "tid", "acc-1", "alias@example.com"); err != nil {
		t.Fatalf("RemoveAlias: %v", err)
	}
	ops := body.Load()
	if ops == nil || len(*ops) != 1 {
		t.Fatalf("ops = %v", ops)
	}
	op := (*ops)[0]
	if op.Action != "removeItem" || op.Field != "emails" || op.Value != "alias@example.com" {
		t.Errorf("op = %+v", op)
	}
}

func TestStalwartAliasHTTPSync_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)

	sync, err := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	err = sync.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500: %v", err)
	}
}

func TestNewStalwartAliasHTTPSync_RejectsMissingDeps(t *testing.T) {
	if _, err := NewStalwartAliasHTTPSync(nil, "u", "p"); err == nil {
		t.Error("expected error on nil resolver")
	}
	if _, err := NewStalwartAliasHTTPSync(&stubResolver{url: "x"}, "", "p"); err == nil {
		t.Error("expected error on empty admin user")
	}
}

func TestStalwartAliasHTTPSync_RejectsEmptyInputs(t *testing.T) {
	sync, err := NewStalwartAliasHTTPSync(&stubResolver{url: "http://example.com"}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	if err := sync.AddAlias(context.Background(), "tid", "", "alias@example.com"); err == nil {
		t.Error("expected error on empty account id")
	}
	if err := sync.AddAlias(context.Background(), "tid", "acc-1", ""); err == nil {
		t.Error("expected error on empty alias")
	}
}

func TestStalwartAliasHTTPSync_ResolverError(t *testing.T) {
	sync, err := NewStalwartAliasHTTPSync(&stubResolver{err: errors.New("no shard")}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	err = sync.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com")
	if err == nil {
		t.Fatal("expected error from resolver")
	}
	if !strings.Contains(err.Error(), "no shard") {
		t.Errorf("error should propagate resolver msg: %v", err)
	}
}
