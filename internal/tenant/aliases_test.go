package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// recordedJMAPCall captures one method invocation seen by the fake.
type recordedJMAPCall struct {
	method string
	args   json.RawMessage
}

// fakeStalwart is a minimal JMAP endpoint stand-in. It records
// every method call and delegates the response to a per-test
// `respond` func, which returns the method-response args JSON and
// an HTTP status (0 => 200). On a 2xx status the args are wrapped
// in a `methodResponses` envelope; otherwise the string is written
// as the raw error body.
type fakeStalwart struct {
	mu      sync.Mutex
	calls   []recordedJMAPCall
	auth    string
	path    string
	respond func(method string, args json.RawMessage) (string, int)
}

func (f *fakeStalwart) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, _ := r.BasicAuth()
	raw, _ := io.ReadAll(r.Body)
	var env struct {
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	_ = json.Unmarshal(raw, &env)
	var method string
	var args json.RawMessage
	if len(env.MethodCalls) > 0 && len(env.MethodCalls[0]) >= 2 {
		_ = json.Unmarshal(env.MethodCalls[0][0], &method)
		args = env.MethodCalls[0][1]
	}
	f.mu.Lock()
	f.calls = append(f.calls, recordedJMAPCall{method: method, args: args})
	f.auth = user + ":" + pass
	f.path = r.URL.Path
	f.mu.Unlock()

	body, status := f.respond(method, args)
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if status >= 200 && status < 300 {
		_, _ = io.WriteString(w, `{"methodResponses":[["`+method+`",`+body+`,"0"]]}`)
		return
	}
	_, _ = io.WriteString(w, body)
}

// methodCalls returns the recorded calls for a given method.
func (f *fakeStalwart) methodCalls(method string) []recordedJMAPCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedJMAPCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// aliasPatch extracts the single `aliases/<n>` patch entry that an
// x:Account/set call applied to the given principal id.
func aliasPatch(t *testing.T, set recordedJMAPCall, jmapID string) (key string, value json.RawMessage) {
	t.Helper()
	var args struct {
		Update map[string]map[string]json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(set.args, &args); err != nil {
		t.Fatalf("decode x:Account/set args: %v", err)
	}
	patch, ok := args.Update[jmapID]
	if !ok {
		t.Fatalf("update missing principal %q: %s", jmapID, set.args)
	}
	if len(patch) != 1 {
		t.Fatalf("want exactly one patch entry, got %d: %s", len(patch), set.args)
	}
	for k, v := range patch {
		return k, v
	}
	return "", nil
}

func TestStalwartAliasHTTPSync_AddAlias_WireShape(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, _ json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			return `{"list":[{"id":"acc-1","name":"user","emailAddress":"user@example.com","aliases":{}}]}`, 0
		case "x:Domain/query":
			return `{"ids":["dom-1"]}`, 0
		case "x:Account/set":
			return `{"updated":{"acc-1":null}}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, err := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	if err := syncer.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}

	if fake.path != "/jmap" {
		t.Errorf("path = %q want /jmap", fake.path)
	}
	if fake.auth != "admin:secret" {
		t.Errorf("auth = %q want admin:secret", fake.auth)
	}
	sets := fake.methodCalls("x:Account/set")
	if len(sets) != 1 {
		t.Fatalf("x:Account/set calls = %d want 1", len(sets))
	}
	key, value := aliasPatch(t, sets[0], "acc-1")
	if key != "aliases/0" {
		t.Errorf("patch key = %q want aliases/0", key)
	}
	var alias struct {
		Enabled  bool   `json:"enabled"`
		Name     string `json:"name"`
		DomainID string `json:"domainId"`
	}
	if err := json.Unmarshal(value, &alias); err != nil {
		t.Fatalf("decode alias value: %v", err)
	}
	if !alias.Enabled || alias.Name != "alias" || alias.DomainID != "dom-1" {
		t.Errorf("alias = %+v want {enabled:true name:alias domainId:dom-1}", alias)
	}
}

func TestStalwartAliasHTTPSync_AddAlias_Idempotent(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, _ json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			return `{"list":[{"id":"acc-1","name":"user","emailAddress":"user@example.com","aliases":{"0":{"enabled":true,"name":"alias","domainId":"dom-1"}}}]}`, 0
		case "x:Domain/query":
			return `{"ids":["dom-1"]}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, _ := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err := syncer.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	if sets := fake.methodCalls("x:Account/set"); len(sets) != 0 {
		t.Errorf("expected no x:Account/set on duplicate add, got %d", len(sets))
	}
}

func TestStalwartAliasHTTPSync_RemoveAlias_WireShape(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, _ json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			return `{"list":[{"id":"acc-1","name":"user","emailAddress":"user@example.com","aliases":{"0":{"enabled":true,"name":"alias","domainId":"dom-1"}}}]}`, 0
		case "x:Domain/query":
			return `{"ids":["dom-1"]}`, 0
		case "x:Account/set":
			return `{"updated":{"acc-1":null}}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, _ := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err := syncer.RemoveAlias(context.Background(), "tid", "acc-1", "alias@example.com"); err != nil {
		t.Fatalf("RemoveAlias: %v", err)
	}
	sets := fake.methodCalls("x:Account/set")
	if len(sets) != 1 {
		t.Fatalf("x:Account/set calls = %d want 1", len(sets))
	}
	key, value := aliasPatch(t, sets[0], "acc-1")
	if key != "aliases/0" {
		t.Errorf("patch key = %q want aliases/0", key)
	}
	if strings.TrimSpace(string(value)) != "null" {
		t.Errorf("patch value = %s want null", value)
	}
}

func TestStalwartAliasHTTPSync_RemoveAlias_Idempotent(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, _ json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			return `{"list":[{"id":"acc-1","name":"user","emailAddress":"user@example.com","aliases":{}}]}`, 0
		case "x:Domain/query":
			return `{"ids":["dom-1"]}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, _ := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err := syncer.RemoveAlias(context.Background(), "tid", "acc-1", "missing@example.com"); err != nil {
		t.Fatalf("RemoveAlias: %v", err)
	}
	if sets := fake.methodCalls("x:Account/set"); len(sets) != 0 {
		t.Errorf("expected no x:Account/set when alias absent, got %d", len(sets))
	}
}

// TestStalwartAliasHTTPSync_ResolvesEmailPlaceholder exercises the
// production identifier story: signup stores the user's email as
// stalwart_account_id (not a JMAP id). The sync must fall through
// get-by-id (empty) and name-query (empty) to the text filter, then
// confirm the exact emailAddress before patching the resolved id.
func TestStalwartAliasHTTPSync_ResolvesEmailPlaceholder(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, args json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			if strings.Contains(string(args), "acc-1") {
				return `{"list":[{"id":"acc-1","name":"user","emailAddress":"user@example.com","aliases":{}}]}`, 0
			}
			return `{"list":[]}`, 0
		case "x:Account/query":
			if strings.Contains(string(args), `"text"`) {
				return `{"ids":["acc-1"]}`, 0
			}
			return `{"ids":[]}`, 0
		case "x:Domain/query":
			return `{"ids":["dom-1"]}`, 0
		case "x:Account/set":
			return `{"updated":{"acc-1":null}}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, _ := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err := syncer.AddAlias(context.Background(), "tid", "user@example.com", "alias@example.com"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	sets := fake.methodCalls("x:Account/set")
	if len(sets) != 1 {
		t.Fatalf("x:Account/set calls = %d want 1", len(sets))
	}
	// The patch must target the *resolved* JMAP id, not the email.
	if _, value := aliasPatch(t, sets[0], "acc-1"); value == nil {
		t.Fatal("expected alias patch on resolved id acc-1")
	}
}

func TestStalwartAliasHTTPSync_PrincipalNotFound(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, _ json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			return `{"list":[]}`, 0
		case "x:Account/query":
			return `{"ids":[]}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, _ := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	err := syncer.AddAlias(context.Background(), "tid", "ghost", "alias@example.com")
	if err == nil {
		t.Fatal("expected error for unresolvable principal")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestStalwartAliasHTTPSync_SetNotUpdated(t *testing.T) {
	fake := &fakeStalwart{respond: func(method string, _ json.RawMessage) (string, int) {
		switch method {
		case "x:Account/get":
			return `{"list":[{"id":"acc-1","name":"user","emailAddress":"user@example.com","aliases":{}}]}`, 0
		case "x:Domain/query":
			return `{"ids":["dom-1"]}`, 0
		case "x:Account/set":
			return `{"notUpdated":{"acc-1":{"type":"invalidProperties","description":"bad alias"}}}`, 0
		default:
			return `{}`, 0
		}
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	syncer, _ := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	err := syncer.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com")
	if err == nil {
		t.Fatal("expected error when account is notUpdated")
	}
	if !strings.Contains(err.Error(), "invalidProperties") {
		t.Errorf("error should surface SetError type: %v", err)
	}
}

func TestStalwartAliasHTTPSync_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)

	syncer, err := NewStalwartAliasHTTPSync(&stubResolver{url: srv.URL}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	err = syncer.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com")
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
	syncer, err := NewStalwartAliasHTTPSync(&stubResolver{url: "http://example.com"}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	if err := syncer.AddAlias(context.Background(), "tid", "", "alias@example.com"); err == nil {
		t.Error("expected error on empty account id")
	}
	if err := syncer.AddAlias(context.Background(), "tid", "acc-1", ""); err == nil {
		t.Error("expected error on empty alias")
	}
}

func TestStalwartAliasHTTPSync_ResolverError(t *testing.T) {
	syncer, err := NewStalwartAliasHTTPSync(&stubResolver{err: errors.New("no shard")}, "admin", "secret")
	if err != nil {
		t.Fatalf("NewStalwartAliasHTTPSync: %v", err)
	}
	err = syncer.AddAlias(context.Background(), "tid", "acc-1", "alias@example.com")
	if err == nil {
		t.Fatal("expected error from resolver")
	}
	if !strings.Contains(err.Error(), "no shard") {
		t.Errorf("error should propagate resolver msg: %v", err)
	}
}
