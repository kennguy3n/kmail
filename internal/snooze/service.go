// Package snooze implements the Email Snooze feature (WS5).
//
// A snooze hides an already-delivered email in a per-user
// "Snoozed" mailbox until `snooze_until` falls into the past;
// then the worker moves the email back to its original
// mailboxes via JMAP `Email/set update`, marks the row
// `unsnoozed`, and (optionally) clears `$seen` so the message
// surfaces as new.
//
// Compare with `internal/scheduledsend`:
//
//   - Scheduled Send holds a *future* `EmailSubmission/set`
//     payload; the worker submits it to Stalwart at send-time.
//   - Snooze holds *already-delivered* state — a mailbox-id
//     diff — and at wake-time the worker patches the
//     existing Email object via `Email/set update` instead of
//     creating a new submission.
//
// Both are durable Postgres queues with the same lifecycle
// shape (claim → dispatch → mark-terminal / schedule-retry),
// the same RLS-by-tenant pattern, and the same opt-in BFF
// role-bypass for cross-tenant worker scans.
package snooze

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

// Lifecycle statuses for a snooze row.
const (
	StatusSnoozed   = "snoozed"
	StatusUnsnoozed = "unsnoozed"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

// Defaults — override via constructor.
const (
	// DefaultMaxAttempts caps transient-failure retries before
	// the worker flips the row to `failed`. Matches
	// `scheduledsend.DefaultMaxAttempts`.
	DefaultMaxAttempts = 5

	// MinSnoozeHorizon is the floor on `snooze_until - now`.
	// Anything below this is almost always a fat-finger
	// ("snooze for 5 seconds") and we reject it.
	MinSnoozeHorizon = 1 * time.Minute

	// MaxSnoozeHorizon caps how far ahead a snooze can land.
	// 365 days mirrors scheduledsend; longer than that is
	// almost certainly a timezone/year bug.
	MaxSnoozeHorizon = 365 * 24 * time.Hour
)

// Errors surfaced to the HTTP and worker layers.
var (
	ErrNotFound         = errors.New("snooze: not found")
	ErrAlreadyAwake     = errors.New("snooze: already awake")
	ErrAlreadyCancelled = errors.New("snooze: already cancelled")
	ErrInvalidSnooze    = errors.New("snooze: invalid snooze_until")
	ErrTenantMismatch   = errors.New("snooze: tenant mismatch")
	ErrAlreadySnoozed   = errors.New("snooze: email is already snoozed")
)

// Snooze mirrors one row in `snoozed_emails`. The
// `OriginalMailboxIDs` JSONB column carries the mailbox-ids
// patch the worker must apply at wake-time to restore the
// email's pre-snooze location.
type Snooze struct {
	ID                 string          `json:"id"`
	TenantID           string          `json:"tenant_id"`
	KChatUserID        string          `json:"kchat_user_id"`
	StalwartAccountID  string          `json:"stalwart_account_id"`
	EmailID            string          `json:"email_id"`
	OriginalMailboxIDs json.RawMessage `json:"original_mailbox_ids"`
	SnoozedMailboxID   string          `json:"snoozed_mailbox_id"`
	SnoozeUntil        time.Time       `json:"snooze_until"`
	MarkUnreadOnWake   bool            `json:"mark_unread_on_wake"`
	Status             string          `json:"status"`
	Attempts           int             `json:"attempts"`
	LastError          string          `json:"last_error,omitempty"`
	NextRetryAt        time.Time       `json:"next_retry_at"`
	WokenAt            *time.Time      `json:"woken_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// Service is the data-access surface over `snoozed_emails`.
// Construct via NewService; the zero value is unusable.
type Service struct {
	pool   *pgxpool.Pool
	logger *log.Logger
	now    func() time.Time
}

// Config configures Service. `Pool` is mandatory; everything
// else defaults via NewService.
type Config struct {
	Pool    *pgxpool.Pool
	Logger  *log.Logger
	NowFunc func() time.Time
}

// NewService validates the config and constructs a Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("snooze.NewService: Pool is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := cfg.NowFunc
	if now == nil {
		now = time.Now
	}
	return &Service{pool: cfg.Pool, logger: logger, now: now}, nil
}

// SnoozeInput carries everything needed to persist a snooze row.
type SnoozeInput struct {
	TenantID           string
	KChatUserID        string
	StalwartAccountID  string
	EmailID            string
	OriginalMailboxIDs json.RawMessage
	SnoozedMailboxID   string
	SnoozeUntil        time.Time
	MarkUnreadOnWake   bool
}

// Snooze persists a new snooze row.
//
// Validation rejects empty fields and out-of-bounds horizons
// before touching the pool, so test code can exercise the
// validator on a nil-pool Service (same pattern as
// `scheduledsend.Service.validateSchedule`).
//
// The unique partial index `snoozed_emails_active_unique`
// enforces "one active snooze per email" — a duplicate INSERT
// surfaces here as ErrAlreadySnoozed.
func (s *Service) Snooze(ctx context.Context, in SnoozeInput) (*Snooze, error) {
	if err := s.validateSnooze(in); err != nil {
		return nil, err
	}
	until := in.SnoozeUntil.UTC()
	var row Snooze
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, in.TenantID); err != nil {
			return err
		}
		r := tx.QueryRow(ctx, `
			INSERT INTO snoozed_emails (
				tenant_id, kchat_user_id, stalwart_account_id,
				email_id, original_mailbox_ids,
				snoozed_mailbox_id, snooze_until,
				mark_unread_on_wake, next_retry_at
			)
			VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6, $7, $8, $7)
			RETURNING id::text, tenant_id::text, kchat_user_id,
			          stalwart_account_id, email_id,
			          original_mailbox_ids, snoozed_mailbox_id,
			          snooze_until, mark_unread_on_wake,
			          status, attempts, last_error,
			          next_retry_at, woken_at,
			          created_at, updated_at
		`,
			in.TenantID, in.KChatUserID, in.StalwartAccountID,
			in.EmailID, []byte(in.OriginalMailboxIDs),
			in.SnoozedMailboxID, until, in.MarkUnreadOnWake,
		)
		return scanRow(r, &row)
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadySnoozed
		}
		return nil, fmt.Errorf("snooze.Snooze: %w", err)
	}
	return &row, nil
}

func (s *Service) validateSnooze(in SnoozeInput) error {
	if strings.TrimSpace(in.TenantID) == "" {
		return errors.New("snooze.Snooze: TenantID is required")
	}
	if strings.TrimSpace(in.KChatUserID) == "" {
		return errors.New("snooze.Snooze: KChatUserID is required")
	}
	if strings.TrimSpace(in.StalwartAccountID) == "" {
		return errors.New("snooze.Snooze: StalwartAccountID is required")
	}
	if strings.TrimSpace(in.EmailID) == "" {
		return errors.New("snooze.Snooze: EmailID is required")
	}
	if len(in.OriginalMailboxIDs) == 0 {
		return errors.New("snooze.Snooze: OriginalMailboxIDs is required")
	}
	// Sanity-check the JSON shape: must decode to a non-empty
	// map of string→bool (the JMAP mailboxIds shape). The
	// worker depends on this at wake-time and a malformed
	// blob would surface as a Stalwart 4xx instead of an
	// earlier user-facing error.
	var probe map[string]bool
	if err := json.Unmarshal(in.OriginalMailboxIDs, &probe); err != nil {
		return fmt.Errorf("snooze.Snooze: OriginalMailboxIDs must be JSON object of mailboxId→bool: %w", err)
	}
	if len(probe) == 0 {
		return errors.New("snooze.Snooze: OriginalMailboxIDs must contain at least one mailbox")
	}
	if strings.TrimSpace(in.SnoozedMailboxID) == "" {
		return errors.New("snooze.Snooze: SnoozedMailboxID is required")
	}
	if in.SnoozeUntil.IsZero() {
		return fmt.Errorf("%w: snooze_until is zero", ErrInvalidSnooze)
	}
	now := s.now().UTC()
	horizon := in.SnoozeUntil.Sub(now)
	if horizon < MinSnoozeHorizon {
		return fmt.Errorf("%w: snooze_until must be at least %s in the future", ErrInvalidSnooze, MinSnoozeHorizon)
	}
	if horizon > MaxSnoozeHorizon {
		return fmt.Errorf("%w: snooze_until must be within %s", ErrInvalidSnooze, MaxSnoozeHorizon)
	}
	return nil
}

// Get reads a row by id with tenant scoping.
//
// Returns ErrTenantMismatch when the row exists but belongs to a
// different tenant; the handler collapses that into 404.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Snooze, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("snooze.Get: tenantID is required")
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	var row Snooze
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		r := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id,
			       original_mailbox_ids, snoozed_mailbox_id,
			       snooze_until, mark_unread_on_wake,
			       status, attempts, last_error,
			       next_retry_at, woken_at,
			       created_at, updated_at
			FROM snoozed_emails
			WHERE id = $1::uuid
		`, id)
		return scanRow(r, &row)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("snooze.Get: %w", err)
	}
	if row.TenantID != tenantID {
		return nil, ErrTenantMismatch
	}
	return &row, nil
}

// ListByUser returns every snooze owned by (tenant, user)
// ordered by newest first. Used by the React Snoozed view.
func (s *Service) ListByUser(ctx context.Context, tenantID, kchatUserID string) ([]Snooze, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("snooze.ListByUser: tenantID is required")
	}
	if strings.TrimSpace(kchatUserID) == "" {
		return nil, errors.New("snooze.ListByUser: kchatUserID is required")
	}
	var out []Snooze
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id,
			       original_mailbox_ids, snoozed_mailbox_id,
			       snooze_until, mark_unread_on_wake,
			       status, attempts, last_error,
			       next_retry_at, woken_at,
			       created_at, updated_at
			FROM snoozed_emails
			WHERE tenant_id = $1::uuid AND kchat_user_id = $2
			ORDER BY created_at DESC
			LIMIT 500
		`, tenantID, kchatUserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s2 Snooze
			if err := scanRow(rows, &s2); err != nil {
				return err
			}
			out = append(out, s2)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("snooze.ListByUser: %w", err)
	}
	return out, nil
}

// Cancel marks a still-snoozed row as cancelled WITHOUT moving
// the email back. The caller is responsible for triggering the
// JMAP mailbox patch (the worker / unsnoose-now handler does
// it). Idempotent — a double-click returns ErrAlreadyCancelled.
func (s *Service) Cancel(ctx context.Context, tenantID, id string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("snooze.Cancel: tenantID is required")
	}
	if strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var status, t string
		row := tx.QueryRow(ctx, `
			SELECT status, tenant_id::text FROM snoozed_emails WHERE id = $1::uuid
		`, id)
		if err := row.Scan(&status, &t); err != nil {
			return err
		}
		if t != tenantID {
			return ErrTenantMismatch
		}
		switch status {
		case StatusUnsnoozed:
			return ErrAlreadyAwake
		case StatusCancelled:
			return ErrAlreadyCancelled
		case StatusFailed:
			return ErrAlreadyAwake
		}
		_, err := tx.Exec(ctx, `
			UPDATE snoozed_emails SET status = 'cancelled' WHERE id = $1::uuid
		`, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// claimDue is the worker's hot path: pick the next due row that
// no other replica is already processing, bump `attempts`, and
// return the row.
//
// Returns (nil, ErrNotFound) when the queue is drained — same
// shape as scheduledsend.Service.claimDue, so the worker logic
// is symmetric across the two packages.
func (s *Service) claimDue(ctx context.Context) (*Snooze, error) {
	var row Snooze
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id,
			       original_mailbox_ids, snoozed_mailbox_id,
			       snooze_until, mark_unread_on_wake,
			       status, attempts, last_error,
			       next_retry_at, woken_at,
			       created_at, updated_at
			FROM snoozed_emails s
			WHERE status = 'snoozed'
			  AND snooze_until <= now()
			  AND next_retry_at <= now()
			ORDER BY snooze_until
			FOR UPDATE OF s SKIP LOCKED
			LIMIT 1
		`)
		if err := scanRow(r, &row); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE snoozed_emails SET attempts = attempts + 1 WHERE id = $1::uuid
		`, row.ID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// markUnsnoozed transitions a row to `unsnoozed` after a
// successful wake. Subsequent ticks skip the row because the
// partial index excludes non-`snoozed` rows.
func (s *Service) markUnsnoozed(ctx context.Context, id string, wokenAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE snoozed_emails
		SET status = 'unsnoozed', woken_at = $2, last_error = ''
		WHERE id = $1::uuid AND status = 'snoozed'
	`, id, wokenAt.UTC())
	return err
}

// markFailed transitions a row to `failed` after the retry cap
// has been hit. Operators inspect the dead-letter rows via the
// admin console.
func (s *Service) markFailed(ctx context.Context, id, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE snoozed_emails
		SET status = 'failed', last_error = $2
		WHERE id = $1::uuid AND status = 'snoozed'
	`, id, lastErr)
	return err
}

// scheduleRetry pushes `next_retry_at` into the future without
// moving the row out of `snoozed`. Used by the worker after a
// transient Stalwart failure.
func (s *Service) scheduleRetry(ctx context.Context, id string, nextRetryAt time.Time, lastErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE snoozed_emails
		SET next_retry_at = $2, last_error = $3
		WHERE id = $1::uuid AND status = 'snoozed'
	`, id, nextRetryAt.UTC(), lastErr)
	return err
}

// rowScanner is the slice of pgx.Row used by scanRow so the
// helper works against both QueryRow and rows-from-Query
// without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRow(r rowScanner, out *Snooze) error {
	var origMailboxes []byte
	var lastErr string
	if err := r.Scan(
		&out.ID, &out.TenantID, &out.KChatUserID,
		&out.StalwartAccountID, &out.EmailID,
		&origMailboxes, &out.SnoozedMailboxID,
		&out.SnoozeUntil, &out.MarkUnreadOnWake,
		&out.Status, &out.Attempts, &lastErr,
		&out.NextRetryAt, &out.WokenAt,
		&out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return err
	}
	out.LastError = lastErr
	out.OriginalMailboxIDs = append(json.RawMessage(nil), origMailboxes...)
	return nil
}

// isUniqueViolation pulls apart pgx's wrapped pq error to detect
// constraint 23505 (unique violation). We can't import the
// pgconn package symbol directly without bloating the test
// surface, so we string-match the SQLSTATE — the error chain
// always carries it via `err.Error()` in pgx v5.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 23505") || strings.Contains(msg, "duplicate key value violates unique constraint")
}
