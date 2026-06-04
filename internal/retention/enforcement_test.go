package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/kmail/internal/audit"
)

// fakeOperator is an in-memory jmap.EmailOperator. It models the
// real paging contract: DestroyEmails removes IDs from the window so
// the next QueryEmailsByDate returns the following batch.
type fakeOperator struct {
	mu sync.Mutex

	remaining   []string // IDs still inside the cutoff window, oldest first
	destroyNoOp bool     // when true, destroy "succeeds" but removes nothing
	queryErr    error
	destroyErr  error

	queryCalls   int
	queryLimits  []int
	queryMailbox []string
	destroyCalls int
	destroySizes []int
}

func (f *fakeOperator) QueryEmailsByDate(_ context.Context, _, mailboxID string, _ time.Time, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryCalls++
	f.queryLimits = append(f.queryLimits, limit)
	f.queryMailbox = append(f.queryMailbox, mailboxID)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if limit <= 0 || limit > len(f.remaining) {
		limit = len(f.remaining)
	}
	out := make([]string, limit)
	copy(out, f.remaining[:limit])
	return out, nil
}

func (f *fakeOperator) DestroyEmails(_ context.Context, _ string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalls++
	f.destroySizes = append(f.destroySizes, len(ids))
	if f.destroyErr != nil {
		return f.destroyErr
	}
	if f.destroyNoOp {
		return nil
	}
	gone := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		gone[id] = struct{}{}
	}
	kept := make([]string, 0, len(f.remaining))
	for _, id := range f.remaining {
		if _, ok := gone[id]; !ok {
			kept = append(kept, id)
		}
	}
	f.remaining = kept
	return nil
}

// fakeColdMover records the batches it was asked to move.
type fakeColdMover struct {
	mu    sync.Mutex
	moved [][]string
	err   error
}

func (f *fakeColdMover) MoveToCold(_ context.Context, _ string, ids []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), ids...)
	f.moved = append(f.moved, cp)
	if f.err != nil {
		return 0, f.err
	}
	return len(ids), nil
}

func (f *fakeColdMover) batches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.moved)
}

// fakeAudit records audit entries.
type fakeAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
	err     error
}

func (f *fakeAudit) Log(_ context.Context, e audit.Entry) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	if f.err != nil {
		return nil, f.err
	}
	return &e, nil
}

func (f *fakeAudit) snapshot() []audit.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Entry(nil), f.entries...)
}

func genIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("acct:msg-%05d", i)
	}
	return out
}

func deletePolicy() Policy {
	return Policy{ID: "11111111-1111-1111-1111-111111111111", PolicyType: "delete", RetentionDays: 30, AppliesTo: "all"}
}

func TestEnforcePolicy_DeleteRecordsCalls(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(150)}
	m := NewMetrics(nil)
	enf := NewEnforcer(op, nil, nil).WithMetrics(m)

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	if run.EmailsProcessed != 150 || run.EmailsDeleted != 150 || run.EmailsArchived != 0 {
		t.Fatalf("counts: processed=%d deleted=%d archived=%d", run.EmailsProcessed, run.EmailsDeleted, run.EmailsArchived)
	}
	if run.CompletedAt == nil {
		t.Fatalf("CompletedAt not stamped")
	}
	// 150 IDs in one 500-page → two destroy chunks (100 + 50).
	if op.destroyCalls != 2 {
		t.Fatalf("destroyCalls = %d, want 2", op.destroyCalls)
	}
	for _, sz := range op.destroySizes {
		if sz > destroyChunk {
			t.Fatalf("destroy batch %d exceeds chunk %d", sz, destroyChunk)
		}
	}
	if got := testutil.ToFloat64(m.EmailsDeleted); got != 150 {
		t.Fatalf("EmailsDeleted metric = %v, want 150", got)
	}
}

func TestEnforcePolicy_DryRunSideEffectFree(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(1200)}
	m := NewMetrics(nil)
	enf := NewEnforcer(op, nil, nil).WithMetrics(m).WithDryRun(true)

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	if run.EmailsDeleted != 0 || run.EmailsArchived != 0 {
		t.Fatalf("dry-run mutated: deleted=%d archived=%d", run.EmailsDeleted, run.EmailsArchived)
	}
	if op.destroyCalls != 0 {
		t.Fatalf("dry-run issued %d destroy calls, want 0", op.destroyCalls)
	}
	if len(op.remaining) != 1200 {
		t.Fatalf("dry-run removed messages: %d remain, want 1200", len(op.remaining))
	}
	// One page is read so the run reports a non-zero lower bound.
	if run.EmailsProcessed == 0 {
		t.Fatalf("dry-run processed 0; expected the first page to be read")
	}
	if !strings.Contains(run.Notes, "dry_run=true") {
		t.Fatalf("dry-run notes = %q", run.Notes)
	}
	if got := testutil.ToFloat64(m.EmailsDeleted); got != 0 {
		t.Fatalf("dry-run incremented deleted metric: %v", got)
	}
}

func TestEnforcePolicy_PagesThroughMoreThan500(t *testing.T) {
	t.Parallel()
	const total = 1200
	op := &fakeOperator{remaining: genIDs(total)}
	enf := NewEnforcer(op, nil, nil)

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	if run.EmailsProcessed != total || run.EmailsDeleted != total {
		t.Fatalf("counts: processed=%d deleted=%d want %d", run.EmailsProcessed, run.EmailsDeleted, total)
	}
	if len(op.remaining) != 0 {
		t.Fatalf("%d messages left undeleted", len(op.remaining))
	}
	// 1200 / 500 → pages of 500, 500, 200, then an empty page.
	if op.queryCalls != 4 {
		t.Fatalf("queryCalls = %d, want 4", op.queryCalls)
	}
	for _, lim := range op.queryLimits {
		if lim != queryPageSize {
			t.Fatalf("query limit = %d, want %d", lim, queryPageSize)
		}
	}
	sum := 0
	for _, sz := range op.destroySizes {
		if sz > destroyChunk {
			t.Fatalf("destroy batch %d exceeds chunk %d", sz, destroyChunk)
		}
		sum += sz
	}
	if sum != total {
		t.Fatalf("destroyed %d, want %d", sum, total)
	}
}

func TestEnforcePolicy_ArchiveCallsPlacementThenDestroy(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(250)}
	cold := &fakeColdMover{}
	m := NewMetrics(nil)
	enf := NewEnforcer(op, nil, nil).WithMetrics(m).WithColdMover(cold)

	policy := Policy{ID: "22222222-2222-2222-2222-222222222222", PolicyType: "archive", RetentionDays: 7, AppliesTo: "all"}
	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", policy)
	if err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	if run.EmailsArchived != 250 || run.EmailsDeleted != 0 {
		t.Fatalf("counts: archived=%d deleted=%d", run.EmailsArchived, run.EmailsDeleted)
	}
	// 250 IDs → chunks 100, 100, 50 moved to cold then destroyed.
	if cold.batches() != 3 {
		t.Fatalf("cold mover batches = %d, want 3", cold.batches())
	}
	if op.destroyCalls != 3 {
		t.Fatalf("destroyCalls = %d, want 3", op.destroyCalls)
	}
	if len(op.remaining) != 0 {
		t.Fatalf("%d messages left after archive", len(op.remaining))
	}
	if got := testutil.ToFloat64(m.EmailsArchived); got != 250 {
		t.Fatalf("EmailsArchived metric = %v, want 250", got)
	}
}

func TestEnforcePolicy_ArchiveWithoutColdMoverFails(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(10)}
	enf := NewEnforcer(op, nil, nil)
	policy := Policy{ID: "33333333-3333-3333-3333-333333333333", PolicyType: "archive", RetentionDays: 7, AppliesTo: "all"}

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", policy)
	if err == nil {
		t.Fatalf("expected error without cold mover")
	}
	if !strings.Contains(err.Error(), "cold-tier mover") {
		t.Fatalf("error = %v", err)
	}
	if run == nil || run.Error == "" {
		t.Fatalf("run should carry the error")
	}
	if op.destroyCalls != 0 {
		t.Fatalf("archive without mover destroyed %d", op.destroyCalls)
	}
}

func TestEnforcePolicy_MailboxScoping(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(5)}
	enf := NewEnforcer(op, nil, nil)
	policy := Policy{ID: "44444444-4444-4444-4444-444444444444", PolicyType: "delete", RetentionDays: 30, AppliesTo: "mailbox", TargetRef: "mbox-42"}

	if _, err := enf.EnforcePolicy(context.Background(), "tenant-a", policy); err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	if len(op.queryMailbox) == 0 || op.queryMailbox[0] != "mbox-42" {
		t.Fatalf("mailbox not forwarded: %v", op.queryMailbox)
	}
}

func TestEnforcePolicy_LabelScopeRejected(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(1000)}
	enf := NewEnforcer(op, nil, nil)
	policy := Policy{ID: "55555555-5555-5555-5555-555555555555", PolicyType: "delete", RetentionDays: 30, AppliesTo: "label", TargetRef: "Promotions"}

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", policy)
	if err == nil || !strings.Contains(err.Error(), "label-scoped") {
		t.Fatalf("expected label rejection, got %v", err)
	}
	// Critically: it must NOT fall back to an unscoped sweep.
	if op.queryCalls != 0 || op.destroyCalls != 0 {
		t.Fatalf("label policy touched mail: queries=%d destroys=%d", op.queryCalls, op.destroyCalls)
	}
	if run == nil || run.EmailsProcessed != 0 {
		t.Fatalf("label policy processed %d", run.EmailsProcessed)
	}
}

func TestEnforcePolicy_MailboxMissingTargetRef(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(5)}
	enf := NewEnforcer(op, nil, nil)
	policy := Policy{ID: "66666666-6666-6666-6666-666666666666", PolicyType: "delete", RetentionDays: 30, AppliesTo: "mailbox"}

	if _, err := enf.EnforcePolicy(context.Background(), "tenant-a", policy); err == nil {
		t.Fatalf("expected error for mailbox policy without target_ref")
	}
	if op.queryCalls != 0 {
		t.Fatalf("queried despite invalid scope")
	}
}

func TestEnforcePolicy_QueryErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("shard offline")
	op := &fakeOperator{queryErr: sentinel}
	enf := NewEnforcer(op, nil, nil)

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrap of %v", err, sentinel)
	}
	if run.Error == "" {
		t.Fatalf("run.Error not populated")
	}
}

func TestEnforcePolicy_DestroyErrorRecordsPartial(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("forbidden")
	op := &fakeOperator{remaining: genIDs(150), destroyErr: sentinel}
	m := NewMetrics(nil)
	enf := NewEnforcer(op, nil, nil).WithMetrics(m)

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrap of %v", err, sentinel)
	}
	if run.EmailsProcessed != 150 {
		t.Fatalf("processed = %d, want 150", run.EmailsProcessed)
	}
	if run.EmailsDeleted != 0 {
		t.Fatalf("deleted = %d on failure, want 0", run.EmailsDeleted)
	}
}

func TestEnforcePolicy_NoProgressGuard(t *testing.T) {
	t.Parallel()
	// destroy reports success but removes nothing → the next query
	// returns the identical batch. The enforcer must bail instead of
	// looping forever.
	op := &fakeOperator{remaining: genIDs(3), destroyNoOp: true}
	enf := NewEnforcer(op, nil, nil)

	_, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if !errors.Is(err, errNoProgress) {
		t.Fatalf("error = %v, want %v", err, errNoProgress)
	}
}

func TestEnforcePolicy_WritesAudit(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: genIDs(40)}
	aud := &fakeAudit{}
	enf := NewEnforcer(op, nil, nil).WithAudit(aud)

	if _, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy()); err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	entries := aud.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != "retention.enforce" || e.ActorType != audit.ActorSystem {
		t.Fatalf("audit entry = %+v", e)
	}
	if e.TenantID != "tenant-a" || e.ResourceType != "retention_policy" {
		t.Fatalf("audit scoping wrong: %+v", e)
	}
	if got, _ := e.Metadata["emails_deleted"].(int); got != 40 {
		t.Fatalf("audit emails_deleted = %v, want 40", e.Metadata["emails_deleted"])
	}
	if dr, _ := e.Metadata["dry_run"].(bool); dr {
		t.Fatalf("audit dry_run should be false for live run")
	}
}

func TestEnforcePolicy_EmptyWindowNoDestroy(t *testing.T) {
	t.Parallel()
	op := &fakeOperator{remaining: nil}
	enf := NewEnforcer(op, nil, nil)

	run, err := enf.EnforcePolicy(context.Background(), "tenant-a", deletePolicy())
	if err != nil {
		t.Fatalf("EnforcePolicy: %v", err)
	}
	if run.EmailsProcessed != 0 || op.destroyCalls != 0 {
		t.Fatalf("empty window processed=%d destroys=%d", run.EmailsProcessed, op.destroyCalls)
	}
}

func TestEnforcer_RequiresOperatorAndTenant(t *testing.T) {
	t.Parallel()
	if _, err := NewEnforcer(nil, nil, nil).EnforcePolicy(context.Background(), "t", deletePolicy()); err == nil {
		t.Fatalf("expected error with nil operator")
	}
	op := &fakeOperator{}
	if _, err := NewEnforcer(op, nil, nil).EnforcePolicy(context.Background(), "  ", deletePolicy()); err == nil {
		t.Fatalf("expected error with blank tenant")
	}
}
