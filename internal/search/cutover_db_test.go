package search

import (
	"context"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestPostgresCutoverStoreLifecycle(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewPostgresCutoverStore(pool)
	ctx := context.Background()
	const target = "shared_opensearch"
	now := time.Now().UTC()

	// UpsertPending creates a pending row.
	job, err := store.UpsertPending(ctx, tenant, target, 1000, 500, now)
	if err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if job.State != "pending" || job.MailboxSize != 1000 {
		t.Fatalf("UpsertPending job=%+v", job)
	}

	// Get returns the same row.
	got, err := store.Get(ctx, tenant, target)
	if err != nil || got == nil || got.State != "pending" {
		t.Fatalf("Get=%+v err=%v", got, err)
	}

	// List returns at least the one row.
	jobs, err := store.List(ctx, tenant)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("List=%+v err=%v", jobs, err)
	}

	// Claim transitions pending → in_progress, returns true once.
	claimed, err := store.Claim(ctx, tenant, target, 1000, 500, now)
	if err != nil || !claimed {
		t.Fatalf("Claim=%v err=%v", claimed, err)
	}
	got, _ = store.Get(ctx, tenant, target)
	if got.State != "in_progress" {
		t.Fatalf("after Claim state=%s want in_progress", got.State)
	}
	if got.CompletedAt != nil {
		t.Errorf("Claim must null completed_at, got %v", got.CompletedAt)
	}

	// MarkCompleted finalises the row.
	if err := store.MarkCompleted(ctx, tenant, target, now); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	got, _ = store.Get(ctx, tenant, target)
	if got.State != "completed" || got.CompletedAt == nil {
		t.Fatalf("after MarkCompleted=%+v", got)
	}
}

func TestPostgresCutoverStoreClaimAndFail(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewPostgresCutoverStore(pool)
	ctx := context.Background()
	const target = "shared_opensearch"
	now := time.Now().UTC()

	// First claim wins; a second claim of an in_progress row loses.
	if ok, err := store.Claim(ctx, tenant, target, 10, 5, now); err != nil || !ok {
		t.Fatalf("first Claim=%v err=%v", ok, err)
	}
	if ok, err := store.Claim(ctx, tenant, target, 10, 5, now); err != nil || ok {
		t.Fatalf("second Claim=%v err=%v want false", ok, err)
	}

	// MarkFailed bumps failure_count and records the reason.
	if err := store.MarkFailed(ctx, tenant, target, "boom", now); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, _ := store.Get(ctx, tenant, target)
	if got.State != "failed" || got.FailureCount != 1 || got.LastError != "boom" {
		t.Fatalf("after MarkFailed=%+v", got)
	}
}

func TestPostgresCutoverStoreCandidatesAndReconcile(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewPostgresCutoverStore(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	// Freshly seeded tenant defaults to search_backend=shared_meilisearch.
	filter := CandidateFilter{
		SourceBackend:    "shared_meilisearch",
		TargetBackend:    "shared_opensearch",
		MaxFailures:      5,
		RetryAfterBefore: now.Add(time.Hour),
	}

	// No job row yet → tenant is a candidate.
	cands, err := store.ListCandidates(ctx, filter)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if !contains(cands, tenant) {
		t.Fatalf("expected %s in candidates %v", tenant, cands)
	}

	// Drive it to in_progress then ReconcileStale (stale window) should
	// reset it back to pending so the worker can re-pick it up.
	if ok, err := store.Claim(ctx, tenant, "shared_opensearch", 10, 5, now.Add(-2*time.Hour)); err != nil || !ok {
		t.Fatalf("Claim=%v err=%v", ok, err)
	}
	n, err := store.ReconcileStale(ctx, "shared_meilisearch", "shared_opensearch", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("ReconcileStale: %v", err)
	}
	if n < 1 {
		t.Errorf("ReconcileStale reset %d rows, want >=1", n)
	}

	// ReconcileCompleted is a no-op-safe call returning a count.
	if _, err := store.ReconcileCompleted(ctx, "shared_opensearch", now.Add(-time.Hour), now); err != nil {
		t.Fatalf("ReconcileCompleted: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
