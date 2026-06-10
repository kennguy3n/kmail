package retention

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestWorkerTickEnforcesDB drives a full worker tick against a live
// DB: seed an active tenant + an enabled delete policy, wire a fake
// operator holding three in-window emails, then assert the live-mode
// tick destroys them, stamps the enforcement-log row, and updates the
// admin snapshot counters.
func TestWorkerTickEnforcesDB(t *testing.T) {
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

	op := &fakeOperator{remaining: []string{"m1", "m2", "m3"}}
	w := NewWorker(svc, log.New(io.Discard, "", 0)).
		WithEnforcer(op).
		WithDryRun(false).
		WithMetrics(NewMetrics(nil)).
		WithInterval(time.Hour)

	if w.DryRun() {
		t.Fatal("DryRun() should be false after WithDryRun(false)")
	}

	if err := w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	snap := w.Snapshot()
	if snap.EmailsDeleted != 3 {
		t.Errorf("snapshot EmailsDeleted=%d want 3", snap.EmailsDeleted)
	}
	if snap.LastEvaluated == nil {
		t.Error("LastEvaluated should be set after a tick")
	}
	if op.destroyCalls == 0 {
		t.Error("operator DestroyEmails was never called")
	}

	// The enforcement run should have been logged for the tenant.
	runs, err := svc.RecentEnforcementRuns(context.Background(), tenant, 10)
	if err != nil {
		t.Fatalf("RecentEnforcementRuns: %v", err)
	}
	if len(runs) == 0 {
		t.Error("expected at least one enforcement-log row after live tick")
	}
}

// TestWorkerTickNoOperator covers the engineFor nil path: a worker
// without a wired operator logs and returns nil rather than panicking.
func TestWorkerTickNoOperator(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	w := NewWorker(svc, log.New(io.Discard, "", 0))
	if err := w.tick(context.Background()); err != nil {
		t.Fatalf("tick without operator should be a no-op, got %v", err)
	}
	if w.engineFor() != nil {
		t.Error("engineFor must stay nil when no operator is wired")
	}
}

// TestWorkerRunCancels exercises the Run loop with a short interval
// and a cancelled context so the ticker fires at least once and the
// select returns on ctx.Done.
func TestWorkerRunCancels(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewService(pool)
	op := &fakeOperator{}
	w := NewWorker(svc, log.New(io.Discard, "", 0)).
		WithEnforcer(op).
		WithInterval(5 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestWorkerRunNilGuards covers the nil-worker / nil-service early
// return in Run.
func TestWorkerRunNilGuards(t *testing.T) {
	var nilWorker *Worker
	nilWorker.Run(context.Background()) // must not panic

	w := NewWorker(nil, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx) // svc==nil → immediate return
}
