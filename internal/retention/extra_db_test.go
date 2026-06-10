package retention

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestEvaluateRetentionEnforcedDB covers Service.EvaluateRetention's
// enforced branch: with an Enforcer registered it drives EnforcePolicy
// for each enabled policy and returns the count that completed.
func TestEvaluateRetentionEnforcedDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool)

	p, err := svc.CreatePolicy(context.Background(), Policy{
		TenantID: tenant, PolicyType: "delete", RetentionDays: 30,
		AppliesTo: "all", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeletePolicy(context.Background(), tenant, p.ID) })

	op := &fakeOperator{remaining: []string{"a", "b"}}
	eng := NewEnforcer(op, pool, log.New(io.Discard, "", 0)).
		WithDryRun(false).
		withClock(func() time.Time { return time.Now().UTC() })
	svc.WithEnforcer(eng)

	n, err := svc.EvaluateRetention(context.Background(), tenant)
	if err != nil {
		t.Fatalf("EvaluateRetention enforced: %v", err)
	}
	if n != 1 {
		t.Errorf("EvaluateRetention enforced count=%d want 1", n)
	}
	if len(op.remaining) != 0 {
		t.Errorf("expected all emails destroyed, %d remain", len(op.remaining))
	}
}

// TestJMAPMoveToCold covers the cold-tier placement path of the
// production enforcer: a 2xx fabric response counts the batch, a
// missing URL errors, and a non-2xx surfaces the HTTP status.
func TestJMAPMoveToCold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/placements/move" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewJMAPEnforcer(fakeShards{url: "x"}, srv.Client(), "", srv.URL, "bearer tok", nil)
	n, err := e.MoveToCold(context.Background(), "t1", []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("MoveToCold: %v", err)
	}
	if n != 3 {
		t.Errorf("MoveToCold count=%d want 3", n)
	}

	// No fabric URL → error.
	noURL := NewJMAPEnforcer(fakeShards{url: "x"}, srv.Client(), "", "", "", nil)
	if _, err := noURL.MoveToCold(context.Background(), "t1", []string{"m1"}); err == nil {
		t.Error("MoveToCold without fabric url should error")
	}

	// 5xx fabric → error with status.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	e5 := NewJMAPEnforcer(fakeShards{url: "x"}, bad.Client(), "", bad.URL, "", nil)
	if _, err := e5.MoveToCold(context.Background(), "t1", []string{"m1"}); err == nil {
		t.Error("MoveToCold on 5xx fabric should error")
	}
}

// TestJMAPQueryShardError covers shardURL's not-configured branch.
func TestJMAPQueryShardError(t *testing.T) {
	e := NewJMAPEnforcer(nil, http.DefaultClient, "", "", "", nil)
	if _, err := e.QueryEmailsByDate(context.Background(), "t1", "", time.Now(), 10); err == nil {
		t.Error("QueryEmailsByDate without shards should error")
	}
}
