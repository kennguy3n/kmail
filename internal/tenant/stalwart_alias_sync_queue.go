// Package tenant — alias-Stalwart sync queue helpers.
//
// Reads and writes to `alias_stalwart_sync_queue` (migration 049).
// Shared between the inline sync attempt in `aliases.go` and the
// background `AliasStalwartSyncWorker`. Lives in its own file so
// the worker can be tested without dragging in the alias CRUD
// surface.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// aliasSyncOp matches the CHECK constraint on
// `alias_stalwart_sync_queue.operation`.
type aliasSyncOp string

const (
	aliasSyncOpAdd    aliasSyncOp = "add"
	aliasSyncOpRemove aliasSyncOp = "remove"
)

// aliasSyncStatus matches the CHECK constraint on
// `alias_stalwart_sync_queue.status`.
const (
	aliasSyncStatusPending = "pending"
	aliasSyncStatusSynced  = "synced"
	aliasSyncStatusFailed  = "failed"
)

// AliasSyncMaxAttempts caps how many times the background worker
// retries before marking a row `failed`. Aligned with
// `webhooks.MaxAttempts` so operators only need to remember one
// retry policy across the BFF.
const AliasSyncMaxAttempts = 5

// nextAliasSyncBackoff returns the delay between an attempt that
// just failed and the next retry. `attempt` is the 1-indexed
// number of the attempt that *just failed* — so the first call
// after the inline-from-the-handler attempt fails passes
// `attempt=1`, the next worker call after the first
// worker-driven retry fails passes `attempt=2`, and so on. The
// schedule mirrors the webhooks worker tiers: 30s, 2m, 10m,
// 30m, 1h. Values past the explicit schedule fall through to
// the 1h default — the worker's `AliasSyncMaxAttempts` guard
// gives up before that tier ever runs.
func nextAliasSyncBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	case 3:
		return 10 * time.Minute
	case 4:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

// enqueueAliasSyncTx inserts a `pending` row into
// `alias_stalwart_sync_queue` inside an already-open transaction so
// the queue intent commits atomically with the alias write. Returns
// the new row's UUID for the inline attempt to refer back to.
//
// `next_retry_at` is `now()` so the worker sees the row immediately
// if the inline attempt is skipped or fails before it can mark the
// row.
func enqueueAliasSyncTx(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	op aliasSyncOp,
	stalwartAccountID, aliasEmail string,
) (string, error) {
	if tenantID == "" || stalwartAccountID == "" || aliasEmail == "" {
		return "", fmt.Errorf("%w: tenant id, stalwart account id, and alias email are required", ErrInvalidInput)
	}
	if op != aliasSyncOpAdd && op != aliasSyncOpRemove {
		return "", fmt.Errorf("%w: alias sync operation must be 'add' or 'remove'", ErrInvalidInput)
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO alias_stalwart_sync_queue
		    (tenant_id, operation, stalwart_account_id, alias_email)
		VALUES ($1::uuid, $2, $3, $4)
		RETURNING id::text
	`, tenantID, string(op), stalwartAccountID, aliasEmail).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// markAliasSyncSynced flips a queue row to `synced` and stamps
// `synced_at = now()`. Called by the inline attempt after a
// successful Stalwart push so the worker doesn't re-do the same
// sync.
func markAliasSyncSynced(ctx context.Context, pool *pgxpool.Pool, id string) error {
	if pool == nil {
		return errors.New("nil pool")
	}
	cmd, err := pool.Exec(ctx, `
		UPDATE alias_stalwart_sync_queue
		SET status = 'synced',
		    synced_at = now(),
		    last_error = ''
		WHERE id = $1::uuid AND status = 'pending'
	`, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		// Row was already marked synced (worker won the race) or
		// the queue row no longer exists. Both are no-ops from
		// the inline-attempt caller's perspective.
		return nil
	}
	return nil
}

// recordAliasSyncFailure bumps `attempts`, sets `last_error`, and
// schedules the next retry. Called by both the inline attempt and
// the background worker. The row stays `pending` until the worker
// gives up (`attempts >= AliasSyncMaxAttempts`), at which point the
// worker flips it to `failed` via markAliasSyncFailed.
func recordAliasSyncFailure(
	ctx context.Context,
	pool *pgxpool.Pool,
	id, errMsg string,
	backoff time.Duration,
) error {
	if pool == nil {
		return errors.New("nil pool")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE alias_stalwart_sync_queue
		SET attempts      = attempts + 1,
		    last_error    = $2,
		    next_retry_at = now() + $3::interval
		WHERE id = $1::uuid AND status = 'pending'
	`, id, errMsg, fmt.Sprintf("%d seconds", int(backoff.Seconds()))); err != nil {
		return err
	}
	return nil
}

// markAliasSyncFailed flips a queue row to `failed` after the
// worker has exhausted its retry budget. Operators inspect failed
// rows via the admin endpoint and either fix Stalwart (then re-
// enqueue manually) or accept the divergence.
func markAliasSyncFailed(ctx context.Context, pool *pgxpool.Pool, id, errMsg string) error {
	if pool == nil {
		return errors.New("nil pool")
	}
	_, err := pool.Exec(ctx, `
		UPDATE alias_stalwart_sync_queue
		SET status     = 'failed',
		    last_error = $2
		WHERE id = $1::uuid AND status = 'pending'
	`, id, errMsg)
	return err
}
