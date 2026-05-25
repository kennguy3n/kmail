// Package tenant — alias-Stalwart sync background worker.
//
// Drains `alias_stalwart_sync_queue` rows whose inline sync attempt
// failed. Polls every `Interval` (default 30s), claims one pending
// row per iteration via `FOR UPDATE SKIP LOCKED`, retries the
// Stalwart push, and either flips the row to `synced` or bumps its
// retry counter with exponential backoff. After
// `AliasSyncMaxAttempts` failures the row is flipped to `failed`
// for operator inspection.
//
// Pattern mirrors `internal/webhooks/worker.go` (migration 032)
// so an operator who already understands the webhook backlog can
// reason about alias-sync backlog the same way.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AliasStalwartSyncWorker drains the alias-Stalwart sync queue.
type AliasStalwartSyncWorker struct {
	pool     *pgxpool.Pool
	sync     StalwartAliasSync
	logger   *log.Logger
	interval time.Duration
}

// NewAliasStalwartSyncWorker constructs a worker. `sync` must be
// the same `StalwartAliasSync` wired to the Tenant Service so the
// worker and the inline attempt target the same Stalwart cluster.
func NewAliasStalwartSyncWorker(pool *pgxpool.Pool, sync StalwartAliasSync, logger *log.Logger) *AliasStalwartSyncWorker {
	if logger == nil {
		logger = log.Default()
	}
	return &AliasStalwartSyncWorker{
		pool:     pool,
		sync:     sync,
		logger:   logger,
		interval: 30 * time.Second,
	}
}

// WithInterval overrides the poll interval. Used by tests to drive
// the worker without waiting 30s between ticks.
func (w *AliasStalwartSyncWorker) WithInterval(d time.Duration) *AliasStalwartSyncWorker {
	w.interval = d
	return w
}

// Run loops until ctx is cancelled. Safe to call on a nil receiver
// or with a nil pool / sync (no-op), which matches how the Tenant
// Service handles unwired Stalwart sync.
func (w *AliasStalwartSyncWorker) Run(ctx context.Context) {
	if w == nil || w.pool == nil || w.sync == nil {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick drains all currently due rows. Calling tick() once per
// poll interval keeps the worker latency-bounded even if the
// backlog spikes briefly.
func (w *AliasStalwartSyncWorker) tick(ctx context.Context) {
	for {
		ok, err := w.processNext(ctx)
		if err != nil {
			w.logger.Printf("alias_sync.worker: %v", err)
			return
		}
		if !ok {
			return
		}
	}
}

// processNext claims one pending row and attempts the Stalwart
// push. Returns (true, nil) if a row was processed (caller should
// loop), (false, nil) if the queue is empty, or (false, err) on a
// terminal store error.
func (w *AliasStalwartSyncWorker) processNext(ctx context.Context) (bool, error) {
	var (
		id, tenantID, op, stalwartAccountID, aliasEmail string
		attempts                                        int
	)
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, operation, stalwart_account_id, alias_email, attempts
			FROM alias_stalwart_sync_queue
			WHERE status = 'pending' AND next_retry_at <= now()
			ORDER BY next_retry_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		`)
		if err := row.Scan(&id, &tenantID, &op, &stalwartAccountID, &aliasEmail, &attempts); err != nil {
			return err
		}
		// Push the next retry timestamp out by one
		// `AliasSyncMaxAttempts`-worth of backoff inside the
		// claim transaction. That gives this worker a lease on
		// the row: a concurrent worker won't see it again until
		// either we mark it synced/failed or the lease expires.
		// The lease is generous because Stalwart can stall for
		// minutes during a rebuild; AliasSyncMaxAttempts steps
		// of backoff is the right ceiling.
		_, err := tx.Exec(ctx, `
			UPDATE alias_stalwart_sync_queue
			SET next_retry_at = now() + $2::interval
			WHERE id = $1::uuid
		`, id, fmt.Sprintf("%d seconds", int(nextAliasSyncBackoff(AliasSyncMaxAttempts).Seconds())))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var syncErr error
	switch aliasSyncOp(op) {
	case aliasSyncOpAdd:
		syncErr = w.sync.AddAlias(ctx, tenantID, stalwartAccountID, aliasEmail)
	case aliasSyncOpRemove:
		syncErr = w.sync.RemoveAlias(ctx, tenantID, stalwartAccountID, aliasEmail)
	default:
		// Row violates the CHECK constraint somehow. Flip it to
		// failed so the worker doesn't keep retrying garbage.
		return true, markAliasSyncFailed(ctx, w.pool, id, fmt.Sprintf("unknown operation %q", op))
	}
	if syncErr == nil {
		return true, markAliasSyncSynced(ctx, w.pool, id)
	}
	// `attempts` is the DB column value BEFORE this worker call.
	// `nextAttempt` is the 1-indexed number of the attempt the
	// worker just executed (e.g. attempts=1 means the inline
	// attempt failed already, so this is attempt #2). After this
	// attempt fails, the schedule entry at index `nextAttempt`
	// gives the delay before the *next* try — see the contract
	// on `nextAliasSyncBackoff`. The previous implementation
	// passed `nextAttempt+1` here, which skipped the 2-minute
	// tier of the schedule entirely (30s → 10m → 30m → 1h
	// instead of the documented 30s → 2m → 10m → 30m).
	nextAttempt := attempts + 1
	if nextAttempt >= AliasSyncMaxAttempts {
		w.logger.Printf("alias_sync.worker: giving up after %d attempts for tenant=%s alias=%s op=%s: %v", nextAttempt, tenantID, aliasEmail, op, syncErr)
		return true, markAliasSyncFailed(ctx, w.pool, id, syncErr.Error())
	}
	if err := recordAliasSyncFailure(ctx, w.pool, id, syncErr.Error(), nextAliasSyncBackoff(nextAttempt)); err != nil {
		return true, err
	}
	w.logger.Printf("alias_sync.worker: attempt %d failed for tenant=%s alias=%s op=%s: %v (will retry)", nextAttempt, tenantID, aliasEmail, op, syncErr)
	return true, nil
}
