package retention

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
