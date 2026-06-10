package tenant

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// stubAliasSync is a configurable StalwartAliasSync for worker tests.
type stubAliasSync struct {
	mu        sync.Mutex
	addErr    error
	removeErr error
	adds      int
	removes   int
}

func (s *stubAliasSync) AddAlias(context.Context, string, string, string) error {
	s.mu.Lock()
	s.adds++
	s.mu.Unlock()
	return s.addErr
}

func (s *stubAliasSync) RemoveAlias(context.Context, string, string, string) error {
	s.mu.Lock()
	s.removes++
	s.mu.Unlock()
	return s.removeErr
}

func seedAliasSyncRow(t *testing.T, tenant, op, account, email string, attempts int) string {
	t.Helper()
	pool := testsupport.Pool(t)
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO alias_stalwart_sync_queue
			(tenant_id, operation, stalwart_account_id, alias_email, attempts, next_retry_at)
		VALUES ($1::uuid, $2, $3, $4, $5, now() - interval '1 minute')
		RETURNING id::text
	`, tenant, op, account, email, attempts).Scan(&id); err != nil {
		t.Fatalf("seed alias sync row: %v", err)
	}
	return id
}

func aliasSyncStatus(t *testing.T, id string) (status string, attempts int) {
	t.Helper()
	pool := testsupport.Pool(t)
	if err := pool.QueryRow(context.Background(), `
		SELECT status, attempts FROM alias_stalwart_sync_queue WHERE id = $1::uuid
	`, id).Scan(&status, &attempts); err != nil {
		t.Fatalf("read alias sync row: %v", err)
	}
	return status, attempts
}

func TestAliasSyncWorkerProcessNextDB(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	// Clear stale due rows so the worker drains only our seeds.
	if _, err := pool.Exec(ctx, `DELETE FROM alias_stalwart_sync_queue WHERE status = 'pending' AND next_retry_at <= now()`); err != nil {
		t.Fatalf("clear pending: %v", err)
	}

	t.Run("add success marks synced", func(t *testing.T) {
		tn := testsupport.SeedTenant(t, pool, "pro", "active")
		id := seedAliasSyncRow(t, tn, "add", "sw-1", "a@example.com", 0)
		w := NewAliasStalwartSyncWorker(pool, &stubAliasSync{}, log.New(io.Discard, "", 0))
		ok, err := w.processNext(ctx)
		if !ok || err != nil {
			t.Fatalf("processNext=(%v,%v) want (true,nil)", ok, err)
		}
		if st, _ := aliasSyncStatus(t, id); st != "synced" {
			t.Errorf("status=%q want synced", st)
		}
	})

	t.Run("remove failure reschedules and increments attempts", func(t *testing.T) {
		tn := testsupport.SeedTenant(t, pool, "pro", "active")
		id := seedAliasSyncRow(t, tn, "remove", "sw-2", "b@example.com", 0)
		w := NewAliasStalwartSyncWorker(pool, &stubAliasSync{removeErr: context.DeadlineExceeded}, log.New(io.Discard, "", 0))
		ok, err := w.processNext(ctx)
		if !ok || err != nil {
			t.Fatalf("processNext=(%v,%v) want (true,nil)", ok, err)
		}
		st, attempts := aliasSyncStatus(t, id)
		if st != "pending" || attempts != 1 {
			t.Errorf("status=%q attempts=%d want pending,1", st, attempts)
		}
	})

	t.Run("remove failure at max attempts gives up", func(t *testing.T) {
		tn := testsupport.SeedTenant(t, pool, "pro", "active")
		// attempts=4 → nextAttempt=5 == AliasSyncMaxAttempts → failed.
		id := seedAliasSyncRow(t, tn, "remove", "sw-3", "c@example.com", AliasSyncMaxAttempts-1)
		w := NewAliasStalwartSyncWorker(pool, &stubAliasSync{removeErr: context.DeadlineExceeded}, log.New(io.Discard, "", 0))
		ok, err := w.processNext(ctx)
		if !ok || err != nil {
			t.Fatalf("processNext=(%v,%v) want (true,nil)", ok, err)
		}
		if st, _ := aliasSyncStatus(t, id); st != "failed" {
			t.Errorf("status=%q want failed", st)
		}
	})

	t.Run("empty queue returns false", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM alias_stalwart_sync_queue WHERE status = 'pending' AND next_retry_at <= now()`); err != nil {
			t.Fatalf("clear pending: %v", err)
		}
		w := NewAliasStalwartSyncWorker(pool, &stubAliasSync{}, log.New(io.Discard, "", 0))
		ok, err := w.processNext(ctx)
		if ok || err != nil {
			t.Fatalf("processNext on empty=(%v,%v) want (false,nil)", ok, err)
		}
	})
}

func TestAliasSyncWorkerTickAndRunDB(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM alias_stalwart_sync_queue WHERE status = 'pending' AND next_retry_at <= now()`); err != nil {
		t.Fatalf("clear pending: %v", err)
	}

	tn := testsupport.SeedTenant(t, pool, "pro", "active")
	id1 := seedAliasSyncRow(t, tn, "add", "sw-a", "t1@example.com", 0)
	id2 := seedAliasSyncRow(t, tn, "add", "sw-b", "t2@example.com", 0)

	stub := &stubAliasSync{}
	w := NewAliasStalwartSyncWorker(pool, stub, log.New(io.Discard, "", 0)).
		WithInterval(10 * time.Millisecond).
		WithBatchCap(50)

	// tick drains both due rows in one pass.
	w.tick(ctx)
	for _, id := range []string{id1, id2} {
		if st, _ := aliasSyncStatus(t, id); st != "synced" {
			t.Errorf("row %s status=%q want synced", id, st)
		}
	}

	// Run loops on the ticker and exits on context cancel.
	runCtx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker Run did not return after cancel")
	}

	// Run no-ops on nil pool / sync.
	NewAliasStalwartSyncWorker(nil, stub, log.New(io.Discard, "", 0)).Run(context.Background())
	NewAliasStalwartSyncWorker(pool, nil, log.New(io.Discard, "", 0)).Run(context.Background())
}
