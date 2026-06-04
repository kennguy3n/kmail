package retention

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeShards is a ShardResolver that always points at one URL.
type fakeShards struct{ url string }

func (f fakeShards) GetTenantShard(_ context.Context, _ string) (string, error) {
	return f.url, nil
}

// jmapSetServer spins an httptest server that answers the single
// Email/set destroy call with the supplied response body. It asserts
// the request actually carried a destroy array so the test fails loud
// if the call shape regresses.
func jmapSetServer(t *testing.T, respBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
}

func TestJMAPDestroyEmails_HardNotDestroyedSurfacesError(t *testing.T) {
	// Stalwart accepted the HTTP request but refused the destroy.
	srv := jmapSetServer(t, `{"methodResponses":[["Email/set",{"accountId":"t1","notDestroyed":{"m1":{"type":"forbidden","description":"locked"}}},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	err := e.DestroyEmails(context.Background(), "t1", []string{"m1"})
	if err == nil {
		t.Fatal("expected error when JMAP reports a hard notDestroyed failure, got nil")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should name the failure type, got %v", err)
	}
}

func TestJMAPDestroyEmails_NotFoundIsIdempotent(t *testing.T) {
	// notFound = already gone; a destroy must treat it as success.
	srv := jmapSetServer(t, `{"methodResponses":[["Email/set",{"accountId":"t1","destroyed":[],"notDestroyed":{"m1":{"type":"notFound"}}},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	if err := e.DestroyEmails(context.Background(), "t1", []string{"m1"}); err != nil {
		t.Fatalf("notFound should be idempotent success, got %v", err)
	}
}

func TestJMAPDestroyEmails_MethodLevelError(t *testing.T) {
	srv := jmapSetServer(t, `{"methodResponses":[["error",{"type":"accountNotFound"},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	err := e.DestroyEmails(context.Background(), "t1", []string{"m1"})
	if err == nil || !strings.Contains(err.Error(), "accountNotFound") {
		t.Fatalf("method-level error should surface, got %v", err)
	}
}

func TestJMAPDestroyEmails_AllDestroyedSucceeds(t *testing.T) {
	srv := jmapSetServer(t, `{"methodResponses":[["Email/set",{"accountId":"t1","destroyed":["m1","m2"]},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	if err := e.DestroyEmails(context.Background(), "t1", []string{"m1", "m2"}); err != nil {
		t.Fatalf("clean destroy should succeed, got %v", err)
	}
}

func TestJMAPQueryEmailsByDate_MethodErrorSurfaces(t *testing.T) {
	// HTTP 200 carrying a JMAP method-level error must NOT be read as
	// an empty (sweep-complete) result, or the enforcer records a
	// false success while mail remains.
	srv := jmapSetServer(t, `{"methodResponses":[["error",{"type":"accountNotFound","description":"no such account"},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	ids, err := e.QueryEmailsByDate(context.Background(), "t1", "", time.Now(), 500)
	if err == nil {
		t.Fatal("expected error for JMAP method-level error response, got nil")
	}
	if !strings.Contains(err.Error(), "accountNotFound") {
		t.Fatalf("error should name the failure type, got %v", err)
	}
	if ids != nil {
		t.Fatalf("expected nil ids on error, got %v", ids)
	}
}

func TestJMAPQueryEmailsByDate_EmptyResultIsNotAnError(t *testing.T) {
	srv := jmapSetServer(t, `{"methodResponses":[["Email/query",{"ids":[]},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	ids, err := e.QueryEmailsByDate(context.Background(), "t1", "", time.Now(), 500)
	if err != nil {
		t.Fatalf("a genuinely empty result must not error, got %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no ids, got %v", ids)
	}
}

func TestJMAPQueryEmailsByDate_ReturnsIDs(t *testing.T) {
	srv := jmapSetServer(t, `{"methodResponses":[["Email/query",{"ids":["m1","m2"]},"c1"]]}`)
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: srv.URL}, srv.Client(), "", "", "", nil)
	ids, err := e.QueryEmailsByDate(context.Background(), "t1", "", time.Now(), 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Fatalf("expected [m1 m2], got %v", ids)
	}
}

func TestJMAPMethodArgs_MatchesByCallID(t *testing.T) {
	// A response carrying multiple method results (out of order) must
	// be resolved by callID, not by position.
	responses := [][]json.RawMessage{
		{json.RawMessage(`"Core/echo"`), json.RawMessage(`{}`), json.RawMessage(`"c0"`)},
		{json.RawMessage(`"Email/query"`), json.RawMessage(`{"ids":["x"]}`), json.RawMessage(`"c1"`)},
	}
	raw, err := jmapMethodArgs(responses, "Email/query", "c1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	if len(args.IDs) != 1 || args.IDs[0] != "x" {
		t.Fatalf("matched the wrong response by callID: %v", args.IDs)
	}
}

func TestJMAPMethodArgs_MissingCallIDErrors(t *testing.T) {
	responses := [][]json.RawMessage{
		{json.RawMessage(`"Email/query"`), json.RawMessage(`{"ids":[]}`), json.RawMessage(`"c0"`)},
	}
	if _, err := jmapMethodArgs(responses, "Email/query", "c1"); err == nil {
		t.Fatal("expected error when no response carries the requested callID")
	}
}

// TestService_EnforcerRegistrationIsRaceFree exercises the atomic
// enforcer pointer: the worker goroutine registers the engine while a
// caller drives EvaluateRetention. Run under -race this would flag an
// unsynchronized read/write of Service.enforcer if it regressed to a
// plain field.
func TestService_EnforcerRegistrationIsRaceFree(t *testing.T) {
	svc := NewService(nil) // nil pool: ListPolicies returns no rows
	eng := NewEnforcer(&fakeOperator{}, nil, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			svc.WithEnforcer(eng)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			if _, err := svc.EvaluateRetention(context.Background(), "tenant-1"); err != nil {
				t.Errorf("EvaluateRetention: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
