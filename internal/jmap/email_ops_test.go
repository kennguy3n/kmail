package jmap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newOperatorTestProxy returns a proxy pointed at srv plus an
// InternalClient, with the account cache primed for each account so
// Dispatch never touches the (dummy) database.
func newOperatorTestProxy(t *testing.T, srv *httptest.Server, accts []tenantAccount) *InternalClient {
	t.Helper()
	p := newTestProxy(t)
	target := p.target
	target.Scheme = "http"
	target.Host = srv.Listener.Addr().String()
	p.target = target
	for _, a := range accts {
		p.PrimeAccountCache("t1", a.kchatUserID, a.accountID)
	}
	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}
	return c
}

func TestQualifyAndSplitEmailID(t *testing.T) {
	t.Parallel()
	q := QualifyEmailID("acc-1", "e9")
	if q != "acc-1:e9" {
		t.Fatalf("QualifyEmailID = %q", q)
	}
	acct, id, ok := SplitQualifiedEmailID(q)
	if !ok || acct != "acc-1" || id != "e9" {
		t.Fatalf("SplitQualifiedEmailID = (%q,%q,%v)", acct, id, ok)
	}
	for _, bad := range []string{"", ":", "noColon", ":leading", "trailing:"} {
		if _, _, ok := SplitQualifiedEmailID(bad); ok {
			t.Errorf("SplitQualifiedEmailID(%q) unexpectedly ok", bad)
		}
	}
}

func TestStalwartEmailOperator_QueryEmailsByDate(t *testing.T) {
	t.Parallel()

	var lastQuery map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		calls, _ := req["methodCalls"].([]any)
		call, _ := calls[0].([]any)
		args, _ := call[1].(map[string]any)
		lastQuery = args
		accountID, _ := args["accountId"].(string)

		ids := map[string][]string{
			"acc-1": {"e1", "e2"},
			"acc-2": {"e3"},
		}[accountID]
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/query", map[string]any{"ids": toAnySlice(ids)}, "q0"},
			},
			"sessionState": "s1",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}, {"u2", "acc-2"}}
	c := newOperatorTestProxy(t, srv, accts)
	op, err := NewStalwartEmailOperator(c, c.proxy.cfg.Pool, c.proxy.Logger())
	if err != nil {
		t.Fatalf("NewStalwartEmailOperator: %v", err)
	}
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	cutoff := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := op.QueryEmailsByDate(context.Background(), "t1", "mbx-9", cutoff, 10)
	if err != nil {
		t.Fatalf("QueryEmailsByDate: %v", err)
	}
	want := []string{"acc-1:e1", "acc-1:e2", "acc-2:e3"}
	if !equalStrings(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}

	// Request shaping: receivedAt `before` filter (UTC "Z"), ascending
	// sort, and the inMailbox scope when a mailbox is supplied.
	filter, _ := lastQuery["filter"].(map[string]any)
	if filter["before"] != "2026-01-02T03:04:05Z" {
		t.Errorf("filter.before = %v", filter["before"])
	}
	if filter["inMailbox"] != "mbx-9" {
		t.Errorf("filter.inMailbox = %v", filter["inMailbox"])
	}
}

func TestStalwartEmailOperator_QueryEmailsByDate_LimitStopsEarly(t *testing.T) {
	t.Parallel()

	queried := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		calls, _ := req["methodCalls"].([]any)
		call, _ := calls[0].([]any)
		args, _ := call[1].(map[string]any)
		accountID, _ := args["accountId"].(string)
		queried[accountID]++
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/query", map[string]any{"ids": []any{"e1", "e2"}}, "q0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}, {"u2", "acc-2"}}
	c := newOperatorTestProxy(t, srv, accts)
	op, _ := NewStalwartEmailOperator(c, c.proxy.cfg.Pool, c.proxy.Logger())
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	got, err := op.QueryEmailsByDate(context.Background(), "t1", "", time.Now(), 2)
	if err != nil {
		t.Fatalf("QueryEmailsByDate: %v", err)
	}
	if want := []string{"acc-1:e1", "acc-1:e2"}; !equalStrings(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if queried["acc-2"] != 0 {
		t.Errorf("acc-2 queried %d times; expected 0 (limit reached on acc-1)", queried["acc-2"])
	}
}

func TestStalwartEmailOperator_QueryEmailsByDate_SkipsAccountMissingMailbox(t *testing.T) {
	t.Parallel()

	// JMAP mailbox ids are per-account: the caller's mailbox exists in
	// acc-2 but not acc-1, where Stalwart rejects the inMailbox filter
	// with invalidArguments. The sweep must skip acc-1 and still
	// collect acc-2's matches rather than failing the whole tenant.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		calls, _ := req["methodCalls"].([]any)
		call, _ := calls[0].([]any)
		args, _ := call[1].(map[string]any)
		accountID, _ := args["accountId"].(string)

		var resp map[string]any
		if accountID == "acc-1" {
			resp = map[string]any{
				"methodResponses": []any{
					[]any{"error", map[string]any{"type": "invalidArguments", "description": "unknown mailbox"}, "q0"},
				},
			}
		} else {
			resp = map[string]any{
				"methodResponses": []any{
					[]any{"Email/query", map[string]any{"ids": []any{"e7"}}, "q0"},
				},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}, {"u2", "acc-2"}}
	c := newOperatorTestProxy(t, srv, accts)
	op, _ := NewStalwartEmailOperator(c, c.proxy.cfg.Pool, c.proxy.Logger())
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	got, err := op.QueryEmailsByDate(context.Background(), "t1", "mbx-9", time.Now(), 10)
	if err != nil {
		t.Fatalf("QueryEmailsByDate: %v", err)
	}
	if want := []string{"acc-2:e7"}; !equalStrings(got, want) {
		t.Fatalf("ids = %v, want %v (acc-1 skipped)", got, want)
	}
}

func TestStalwartEmailOperator_QueryEmailsByDate_OtherErrorPropagates(t *testing.T) {
	t.Parallel()

	// A non-invalidArguments method error is a real failure and must
	// abort the sweep even when a mailbox filter is set — we only
	// tolerate the specific "mailbox not in this account" signal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"error", map[string]any{"type": "serverFail", "description": "boom"}, "q0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}}
	c := newOperatorTestProxy(t, srv, accts)
	op, _ := NewStalwartEmailOperator(c, c.proxy.cfg.Pool, c.proxy.Logger())
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	if _, err := op.QueryEmailsByDate(context.Background(), "t1", "mbx-9", time.Now(), 10); err == nil {
		t.Fatal("expected serverFail to propagate, not be skipped")
	}
}

func TestStalwartEmailOperator_DestroyEmails(t *testing.T) {
	t.Parallel()

	destroyedByAccount := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		calls, _ := req["methodCalls"].([]any)
		call, _ := calls[0].([]any)
		args, _ := call[1].(map[string]any)
		accountID, _ := args["accountId"].(string)
		destroy, _ := args["destroy"].([]any)
		for _, v := range destroy {
			destroyedByAccount[accountID] = append(destroyedByAccount[accountID], v.(string))
		}
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/set", map[string]any{"destroyed": destroy}, "d0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}, {"u2", "acc-2"}}
	c := newOperatorTestProxy(t, srv, accts)
	op, _ := NewStalwartEmailOperator(c, c.proxy.cfg.Pool, c.proxy.Logger())
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	err := op.DestroyEmails(context.Background(), "t1", []string{"acc-1:e1", "acc-2:e3", "acc-1:e2"})
	if err != nil {
		t.Fatalf("DestroyEmails: %v", err)
	}
	if want := []string{"e1", "e2"}; !equalStrings(destroyedByAccount["acc-1"], want) {
		t.Errorf("acc-1 destroyed %v, want %v", destroyedByAccount["acc-1"], want)
	}
	if want := []string{"e3"}; !equalStrings(destroyedByAccount["acc-2"], want) {
		t.Errorf("acc-2 destroyed %v, want %v", destroyedByAccount["acc-2"], want)
	}
}

func TestStalwartEmailOperator_DestroyEmails_MalformedID(t *testing.T) {
	t.Parallel()
	op := &StalwartEmailOperator{}
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return nil, nil }
	err := op.DestroyEmails(context.Background(), "t1", []string{"noColon"})
	if err == nil {
		t.Fatal("expected error for malformed qualified id")
	}
}

func TestStalwartEmailOperator_DestroyEmails_HardErrorSurfaces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// notFound is tolerated; forbidden must surface.
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/set", map[string]any{
					"notDestroyed": map[string]any{
						"e1": map[string]any{"type": "notFound"},
						"e2": map[string]any{"type": "forbidden", "description": "no perms"},
					},
				}, "d0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}}
	c := newOperatorTestProxy(t, srv, accts)
	op, _ := NewStalwartEmailOperator(c, c.proxy.cfg.Pool, c.proxy.Logger())
	op.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	err := op.DestroyEmails(context.Background(), "t1", []string{"acc-1:e1", "acc-1:e2"})
	if err == nil {
		t.Fatal("expected hard destroy error to surface")
	}
}

func TestCheckEmailSetDestroy_DeterministicError(t *testing.T) {
	t.Parallel()

	// Two different hard failures in one batch. Map iteration is
	// randomized, so without sorting the surfaced error would vary
	// per run. Sorted id order means "e1" is always selected.
	resp := &JmapResponse{
		MethodResponses: [][]any{
			{"Email/set", map[string]any{
				"notDestroyed": map[string]any{
					"e3": map[string]any{"type": "serverFail", "description": "boom"},
					"e1": map[string]any{"type": "forbidden", "description": "no perms"},
					"e2": map[string]any{"type": "notFound"},
				},
			}, "d0"},
		},
	}
	for i := 0; i < 20; i++ {
		err := checkEmailSetDestroy(resp, "d0")
		if err == nil {
			t.Fatal("expected a hard error to surface")
		}
		if !strings.Contains(err.Error(), "destroy e1 failed: forbidden") {
			t.Fatalf("iteration %d: err = %v, want deterministic e1/forbidden", i, err)
		}
	}
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
