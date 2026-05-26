// Package scheduledsend implements the Scheduled Send feature.
//
// A scheduled send is a user-authored email submission that is
// held in Postgres until `send_at`, then dispatched to Stalwart
// via the JMAP `InternalClient`. The user-visible surface is two
// REST endpoints (`POST /api/v1/scheduled-sends` to schedule,
// `GET` to list, `DELETE` to cancel) plus an opt-in JMAP proxy
// hook that intercepts the existing Compose flow when the client
// sets the `X-KMail-Schedule-At` header.
//
// Compare with `internal/undosend`:
//
//   - Undo Send holds for <30s and is Valkey-backed (in-memory
//     sorted set, deadline-keyed). Losing a hold across a Valkey
//     restart is acceptable — the user just doesn't get the
//     cancel window — and the cancel UX is a transient banner.
//
//   - Scheduled Send holds for minutes → weeks. The user expects
//     the message to survive a BFF restart, replication failover,
//     and a deploy of the worker. The store is therefore Postgres
//     (durable, RLS-isolated per tenant), the worker claims rows
//     with `SELECT ... FOR UPDATE SKIP LOCKED` (one row at a time,
//     multi-replica safe), and the user-facing "Scheduled" list
//     view is a normal indexed SELECT instead of a Valkey ZSCAN.
//
// Per `migrations/051_scheduled_sends.sql`, the table is RLS-
// enabled and isolated by `app.tenant_id`. The worker runs across
// tenants without setting the GUC and relies on the BFF role
// being exempt from forced RLS — the same pattern used by
// `webhook_deliveries` and `alias_stalwart_sync_queue`.
package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Lifecycle statuses for a scheduled send.
const (
	StatusPending   = "pending"
	StatusSent      = "sent"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

// Defaults — override via constructor.
const (
	// DefaultMaxAttempts is the cap on transient-failure retries
	// before the worker flips the row to `failed` and leaves it
	// for operator inspection. Matches `webhooks.MaxAttempts`.
	DefaultMaxAttempts = 5

	// MinScheduleHorizon is the floor on `send_at - now`. The
	// proxy hook rejects schedules below this window because
	// "schedule for 5 seconds from now" is really Undo Send and
	// shouldn't go through this surface (it would still work
	// but the user gets a worse UX — no countdown, no inline
	// cancel banner).
	MinScheduleHorizon = 1 * time.Minute

	// MaxScheduleHorizon caps how far ahead a single submission
	// may be scheduled. Anything further is almost always a bug
	// (timezone math, accidental year selection) so we reject it
	// at the boundary instead of silently holding the row for a
	// decade. One year is the documented platform ceiling.
	MaxScheduleHorizon = 365 * 24 * time.Hour
)

// Errors surfaced to the HTTP and proxy-hook layers.
var (
	ErrNotFound        = errors.New("scheduledsend: not found")
	ErrAlreadyDispatched = errors.New("scheduledsend: already dispatched")
	ErrAlreadyCancelled  = errors.New("scheduledsend: already cancelled")
	ErrInvalidSchedule = errors.New("scheduledsend: invalid send_at")
	ErrTenantMismatch  = errors.New("scheduledsend: tenant mismatch")
)

// ScheduledSend is the in-memory mirror of one row in
// `scheduled_sends`. `SubmissionPayload` carries the raw
// `EmailSubmission/set` *create* invocation arguments so the
// worker can replay the original submission verbatim.
type ScheduledSend struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	KChatUserID       string          `json:"kchat_user_id"`
	StalwartAccountID string          `json:"stalwart_account_id"`
	EmailID           string          `json:"email_id"`
	IdentityID        string          `json:"identity_id"`
	SubmissionPayload json.RawMessage `json:"submission_payload"`
	SendAt            time.Time       `json:"send_at"`
	Status            string          `json:"status"`
	Attempts          int             `json:"attempts"`
	LastError         string          `json:"last_error,omitempty"`
	NextRetryAt       time.Time       `json:"next_retry_at"`
	SentAt            *time.Time      `json:"sent_at,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// Service is the data access surface over `scheduled_sends`.
// Construct via NewService; the zero value is unusable.
type Service struct {
	pool   *pgxpool.Pool
	logger *log.Logger
	now    func() time.Time
}

// Config configures Service. `Pool` is mandatory; everything
// else defaults via NewService.
type Config struct {
	Pool   *pgxpool.Pool
	Logger *log.Logger
	// NowFunc is a test seam. Production passes time.Now.
	NowFunc func() time.Time
}

// NewService validates the config and constructs a Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("scheduledsend.NewService: Pool is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := cfg.NowFunc
	if now == nil {
		now = time.Now
	}
	return &Service{
		pool:   cfg.Pool,
		logger: logger,
		now:    now,
	}, nil
}

// ScheduleInput carries everything the worker needs to dispatch
// the held submission later.
type ScheduleInput struct {
	TenantID          string
	KChatUserID       string
	StalwartAccountID string
	EmailID           string
	IdentityID        string
	SubmissionPayload json.RawMessage
	SendAt            time.Time
}

// Schedule persists a new row and returns the resulting record.
// `SendAt` is normalised to UTC before insertion.
func (s *Service) Schedule(ctx context.Context, in ScheduleInput) (*ScheduledSend, error) {
	if err := s.validateSchedule(in); err != nil {
		return nil, err
	}
	sendAt := in.SendAt.UTC()
	var ss ScheduledSend
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, in.TenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO scheduled_sends (
				tenant_id, kchat_user_id, stalwart_account_id,
				email_id, identity_id, submission, send_at,
				next_retry_at
			)
			VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7, $7)
			RETURNING id::text, tenant_id::text, kchat_user_id,
			          stalwart_account_id, email_id, identity_id,
			          submission, send_at, status, attempts,
			          last_error, next_retry_at, sent_at,
			          created_at, updated_at
		`,
			in.TenantID, in.KChatUserID, in.StalwartAccountID,
			in.EmailID, in.IdentityID, []byte(in.SubmissionPayload),
			sendAt,
		)
		return scanRow(row, &ss)
	})
	if err != nil {
		return nil, fmt.Errorf("scheduledsend.Schedule: %w", err)
	}
	return &ss, nil
}

func (s *Service) validateSchedule(in ScheduleInput) error {
	if strings.TrimSpace(in.TenantID) == "" {
		return errors.New("scheduledsend.Schedule: TenantID is required")
	}
	if strings.TrimSpace(in.KChatUserID) == "" {
		return errors.New("scheduledsend.Schedule: KChatUserID is required")
	}
	if strings.TrimSpace(in.StalwartAccountID) == "" {
		return errors.New("scheduledsend.Schedule: StalwartAccountID is required")
	}
	if strings.TrimSpace(in.EmailID) == "" {
		return errors.New("scheduledsend.Schedule: EmailID is required")
	}
	if strings.TrimSpace(in.IdentityID) == "" {
		return errors.New("scheduledsend.Schedule: IdentityID is required")
	}
	if len(in.SubmissionPayload) == 0 {
		return errors.New("scheduledsend.Schedule: SubmissionPayload is required")
	}
	if in.SendAt.IsZero() {
		return fmt.Errorf("%w: send_at is zero", ErrInvalidSchedule)
	}
	now := s.now().UTC()
	horizon := in.SendAt.Sub(now)
	if horizon < MinScheduleHorizon {
		return fmt.Errorf("%w: send_at must be at least %s in the future", ErrInvalidSchedule, MinScheduleHorizon)
	}
	if horizon > MaxScheduleHorizon {
		return fmt.Errorf("%w: send_at must be within %s", ErrInvalidSchedule, MaxScheduleHorizon)
	}
	return nil
}

// Get reads a row by id with tenant scoping.
//
// Returns ErrTenantMismatch when the row exists but belongs to a
// different tenant — the handler maps that to 404 to avoid
// leaking the existence of cross-tenant ids.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*ScheduledSend, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("scheduledsend.Get: tenantID is required")
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	var ss ScheduledSend
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id, identity_id,
			       submission, send_at, status, attempts,
			       last_error, next_retry_at, sent_at,
			       created_at, updated_at
			FROM scheduled_sends
			WHERE id = $1::uuid
		`, id)
		return scanRow(row, &ss)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scheduledsend.Get: %w", err)
	}
	if ss.TenantID != tenantID {
		// RLS should have masked this, but defence in depth: if
		// the BFF role bypasses RLS (it does), confirm the
		// tenant matches before returning.
		return nil, ErrTenantMismatch
	}
	return &ss, nil
}

// ListByUser returns every row owned by (tenant, user) ordered by
// creation time DESC. Used by the React "Scheduled" page.
func (s *Service) ListByUser(ctx context.Context, tenantID, kchatUserID string) ([]ScheduledSend, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("scheduledsend.ListByUser: tenantID is required")
	}
	if strings.TrimSpace(kchatUserID) == "" {
		return nil, errors.New("scheduledsend.ListByUser: kchatUserID is required")
	}
	var out []ScheduledSend
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id, identity_id,
			       submission, send_at, status, attempts,
			       last_error, next_retry_at, sent_at,
			       created_at, updated_at
			FROM scheduled_sends
			WHERE tenant_id = $1::uuid AND kchat_user_id = $2
			ORDER BY created_at DESC
			LIMIT 500
		`, tenantID, kchatUserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ss ScheduledSend
			if err := scanRow(rows, &ss); err != nil {
				return err
			}
			out = append(out, ss)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("scheduledsend.ListByUser: %w", err)
	}
	return out, nil
}

// Cancel marks a still-pending row as cancelled. Idempotent — a
// double-click returns ErrAlreadyCancelled instead of throwing.
func (s *Service) Cancel(ctx context.Context, tenantID, id string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("scheduledsend.Cancel: tenantID is required")
	}
	if strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Optimistic guarded UPDATE. The `AND status = 'pending'`
		// clause closes the TOCTOU window vs. the worker's claim:
		// in READ COMMITTED, a pre-UPDATE SELECT can read
		// `status='pending'` while the worker simultaneously
		// commits `status='sent'`, and a subsequent unguarded
		// `UPDATE ... WHERE id=$1` would clobber the sent state
		// with `cancelled` — silently producing a row that says
		// `cancelled` for a mail that already shipped. Postgres
		// blocks the guarded UPDATE behind the worker's FOR UPDATE
		// lock, then re-evaluates the WHERE clause against the
		// post-commit snapshot, so we either claim the row (1 row
		// affected) or no-op (0 rows affected) and disambiguate
		// the precise reason below. The redundant
		// `tenant_id = $2::uuid` belt protects against a wrong-
		// tenant clobber even if the GUC is somehow misconfigured.
		result, err := tx.Exec(ctx, `
			UPDATE scheduled_sends
			SET status = 'cancelled'
			WHERE id = $1::uuid AND tenant_id = $2::uuid AND status = 'pending'
		`, id, tenantID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 1 {
			return nil
		}
		// 0 rows: re-read to map (missing | tenant-mismatch |
		// sent | cancelled | failed) to the precise sentinel
		// error the handler surfaces to the client.
		var status, t string
		row := tx.QueryRow(ctx, `
			SELECT status, tenant_id::text FROM scheduled_sends WHERE id = $1::uuid
		`, id)
		if err := row.Scan(&status, &t); err != nil {
			return err
		}
		if t != tenantID {
			return ErrTenantMismatch
		}
		switch status {
		case StatusCancelled:
			return ErrAlreadyCancelled
		case StatusSent, StatusFailed:
			return ErrAlreadyDispatched
		default:
			// Shouldn't happen — the guarded UPDATE only no-ops
			// when status != 'pending'. A surprise here is a
			// schema-drift bug worth surfacing loudly.
			return fmt.Errorf("scheduledsend.Cancel: guarded UPDATE no-op with unexpected status=%q", status)
		}
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return nil
}

// claimDue is the worker's hot path: pick the next due row that
// no other replica is already processing, bump `attempts`, and
// return the row to the caller. Returns (nil, ErrNotFound) when
// the queue is drained.
//
// `FOR UPDATE OF s SKIP LOCKED` makes the claim safe across N
// worker replicas: each transaction sees a different unlocked row
// and the UPDATE within the same transaction holds the lock until
// commit. If the worker crashes mid-dispatch, the lock dies with
// the connection and another replica picks the row up on the
// next tick.
func (s *Service) claimDue(ctx context.Context) (*ScheduledSend, error) {
	var ss ScheduledSend
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id, identity_id,
			       submission, send_at, status, attempts,
			       last_error, next_retry_at, sent_at,
			       created_at, updated_at
			FROM scheduled_sends s
			WHERE status = 'pending'
			  AND send_at <= now()
			  AND next_retry_at <= now()
			ORDER BY send_at
			FOR UPDATE OF s SKIP LOCKED
			LIMIT 1
		`)
		if err := scanRow(row, &ss); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE scheduled_sends SET attempts = attempts + 1 WHERE id = $1::uuid
		`, ss.ID); err != nil {
			return err
		}
		// scanRow captured the PRE-increment value above; the UPDATE
		// has now bumped the DB row but the returned struct must
		// reflect the same post-increment value so the worker's
		// `handleErr` (which compares `ss.Attempts >= maxAttempts`
		// to decide retry vs. dead-letter, and uses `ss.Attempts` as
		// the backoff index) sees a value consistent with the DB.
		// Without this, the worker would retry one extra time AND
		// use the wrong backoff slot — both off-by-one bugs.
		ss.Attempts++
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

// markDispatched transitions a row to `sent` after a successful
// Stalwart submission. The worker calls this after the dispatch
// succeeds; subsequent ticks skip the row because the index is
// partial on `status = 'pending'`.
//
// A 0-row UPDATE here means a concurrent Cancel won the race
// between `claimDue` and the post-Dispatch bookkeeping. The mail
// shipped successfully, but the row says `cancelled`. From the
// user's point of view the cancel wins (they clicked cancel, they
// got the cancel confirmation, but the mail had already left the
// building). This is the correct outcome, but we log it so
// operators can correlate the rare "I cancelled but the recipient
// got it" support tickets with a real Stalwart submission.
func (s *Service) markDispatched(ctx context.Context, id string, sentAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_sends
		SET status = 'sent', sent_at = $2, last_error = ''
		WHERE id = $1::uuid AND status = 'pending'
	`, id, sentAt.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		s.logger.Printf("scheduledsend.markDispatched: id=%s no-op (cancelled racing dispatch); mail was sent but row says cancelled", id)
	}
	return nil
}

// markFailed transitions a row to `failed` after the retry cap
// has been hit. The row is kept for operator inspection.
//
// A 0-row UPDATE here means a concurrent Cancel won the race
// between `claimDue` and the post-Dispatch bookkeeping after the
// retry budget was exhausted. The cancel sticks; the row stays
// `cancelled`. We log so operators can spot the rare case where
// the worker exhausted retries but the user's cancel masked the
// failure.
func (s *Service) markFailed(ctx context.Context, id, lastErr string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_sends
		SET status = 'failed', last_error = $2
		WHERE id = $1::uuid AND status = 'pending'
	`, id, lastErr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		s.logger.Printf("scheduledsend.markFailed: id=%s no-op (cancelled racing post-retry bookkeeping); last_error=%q", id, lastErr)
	}
	return nil
}

// scheduleRetry pushes `next_retry_at` into the future without
// moving the row out of `pending`. Used by the worker after a
// transient Stalwart failure when there are retries left.
func (s *Service) scheduleRetry(ctx context.Context, id string, nextRetryAt time.Time, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE scheduled_sends
		SET next_retry_at = $2, last_error = $3
		WHERE id = $1::uuid AND status = 'pending'
	`, id, nextRetryAt.UTC(), lastErr)
	return err
}

// rowScanner unifies pgx.Row (single-row) and pgx.Rows (iterator)
// for the shared `scanRow` helper.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRow(r rowScanner, ss *ScheduledSend) error {
	var (
		submission  []byte
		sentAt      *time.Time
	)
	err := r.Scan(
		&ss.ID, &ss.TenantID, &ss.KChatUserID,
		&ss.StalwartAccountID, &ss.EmailID, &ss.IdentityID,
		&submission, &ss.SendAt, &ss.Status, &ss.Attempts,
		&ss.LastError, &ss.NextRetryAt, &sentAt,
		&ss.CreatedAt, &ss.UpdatedAt,
	)
	if err != nil {
		return err
	}
	ss.SubmissionPayload = json.RawMessage(submission)
	ss.SentAt = sentAt
	return nil
}
