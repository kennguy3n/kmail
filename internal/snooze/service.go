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

	// DispatchLeaseInterval is how far `next_retry_at` is
	// pushed forward inside the `claimDue` transaction so the
	// row is ineligible for re-claim by another replica while
	// THIS replica is dispatching. Without this lease,
	// pgx.BeginFunc commits and releases the FOR UPDATE lock the
	// moment the claim transaction ends, and the row — still
	// `status='snoozed'` with `snooze_until <= now()` and
	// `next_retry_at <= now()` — becomes immediately eligible
	// for a second replica's `claimDue` to pick up, producing a
	// duplicate JMAP `Email/set update` patch. Two-or-more
	// concurrent patches with the same payload aren't strictly
	// destructive (the second one is a no-op on the now-final
	// mailbox set), but the second wake also fires the
	// `markUnsnoozed` write that races with the worker's own
	// scheduleRetry / markFailed bookkeeping. Five minutes is
	// long enough to comfortably absorb a slow JMAP round trip
	// + post-Dispatch bookkeeping, and short enough that an
	// actually-crashed worker only delays the row by 5 minutes
	// before a replacement replica re-claims. Mirrors the same
	// constant in `internal/scheduledsend` deliberately so the
	// two workers share the same operational profile.
	DispatchLeaseInterval = 5 * time.Minute
)

// Errors surfaced to the HTTP and worker layers.
var (
	ErrNotFound         = errors.New("snooze: not found")
	ErrAlreadyAwake     = errors.New("snooze: already awake")
	ErrAlreadyCancelled = errors.New("snooze: already cancelled")
	ErrInvalidSnooze    = errors.New("snooze: invalid snooze_until")
	ErrTenantMismatch   = errors.New("snooze: tenant mismatch")
	ErrAlreadySnoozed   = errors.New("snooze: email is already snoozed")
	// ErrSnoozeFailed signals a row in the terminal `failed`
	// state, where the worker exhausted retries trying to wake
	// the email and the email is still in the user's Snoozed
	// folder. Semantically distinct from ErrAlreadyAwake (the
	// email is NOT awake — the row just stopped trying) so the
	// handler doesn't surface 200 to a caller whose intent was
	// "move this email back" but whose email is still stuck.
	ErrSnoozeFailed = errors.New("snooze: row in failed state, email still in snoozed folder")
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
// Returns:
//   - ErrNotFound: row missing or scoped to a different user
//   - ErrTenantMismatch: row belongs to a different tenant (the
//     handler collapses both into 404 to avoid leaking the
//     existence of cross-tenant ids)
//
// `kchatUserID` is required because the snooze authz model is
// per-user, NOT per-tenant: a snooze row is private to the user
// who created it, and an inadvertent fall-through to tenant-only
// scoping would let any user in the tenant read another user's
// snoozed-email metadata (mailbox-id sets, EmailIDs) by guessing
// UUIDs.
func (s *Service) Get(ctx context.Context, tenantID, kchatUserID, id string) (*Snooze, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("snooze.Get: tenantID is required")
	}
	if strings.TrimSpace(kchatUserID) == "" {
		return nil, errors.New("snooze.Get: kchatUserID is required")
	}
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	var row Snooze
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// The (tenant_id, kchat_user_id) belt on the SELECT is the
		// actual authz fence. RLS would also exclude cross-tenant
		// rows except the BFF role is exempt from forced RLS (see
		// migration 052 + package doc); without this explicit
		// predicate, the cross-USER hole stays open even with RLS
		// healthy because RLS only enforces tenant.
		r := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email_id,
			       original_mailbox_ids, snoozed_mailbox_id,
			       snooze_until, mark_unread_on_wake,
			       status, attempts, last_error,
			       next_retry_at, woken_at,
			       created_at, updated_at
			FROM snoozed_emails
			WHERE id = $1::uuid AND tenant_id = $2::uuid AND kchat_user_id = $3
		`, id, tenantID, kchatUserID)
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
//
// `kchatUserID` is required because the snooze authz model is
// per-user (see Get above). Without this scoping, any user in
// the tenant could cancel another user's snooze by guessing UUIDs.
func (s *Service) Cancel(ctx context.Context, tenantID, kchatUserID, id string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("snooze.Cancel: tenantID is required")
	}
	if strings.TrimSpace(kchatUserID) == "" {
		return errors.New("snooze.Cancel: kchatUserID is required")
	}
	if strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Optimistic guarded UPDATE. The `AND status = 'snoozed'`
		// clause closes the TOCTOU window vs. the worker's claim:
		// in READ COMMITTED, a prior SELECT could read
		// `status='snoozed'` while the worker simultaneously commits
		// `markUnsnoozed` → `status='unsnoozed'`, and a subsequent
		// unguarded `UPDATE ... WHERE id=$1` would clobber the
		// already-unsnoozed state with `cancelled` — producing a
		// row that says `cancelled` for an email that's already
		// back in the user's inbox. Postgres blocks the guarded
		// UPDATE behind the worker's FOR UPDATE lock, then
		// re-evaluates the WHERE clause against the post-commit
		// snapshot, so we either claim the row (1 row affected) or
		// no-op (0 rows affected) and disambiguate the precise
		// reason below. The `tenant_id` AND `kchat_user_id` belts
		// together form the authz fence so a cross-user cancel is a
		// no-op that re-reads as "not found" rather than silently
		// clobbering a peer's row.
		// status IN ('snoozed', 'failed'): a user-initiated wake
		// is allowed to take over from a row the worker has given
		// up on. The wakeNow handler retries the JMAP move BEFORE
		// calling Cancel, so by the time we land here the email
		// is either back in its originals (success) or the
		// applyMove already returned 502 and we never get called.
		// Flipping `failed` → `cancelled` here records the
		// user-driven recovery in the audit trail and stops the
		// row from showing up in any future failed-state UI.
		result, err := tx.Exec(ctx, `
			UPDATE snoozed_emails
			SET status = 'cancelled'
			WHERE id = $1::uuid
			  AND tenant_id = $2::uuid
			  AND kchat_user_id = $3
			  AND status IN ('snoozed', 'failed')
		`, id, tenantID, kchatUserID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 1 {
			return nil
		}
		// 0 rows: re-read to map (missing | tenant-mismatch |
		// user-mismatch | unsnoozed | cancelled | failed) to the
		// precise sentinel error the handler surfaces to the
		// client. The re-read is scoped to id only because we
		// need to disambiguate user-mismatch from
		// already-unsnoozed and the latter requires reading the row.
		var status, t, owner string
		row := tx.QueryRow(ctx, `
			SELECT status, tenant_id::text, kchat_user_id FROM snoozed_emails WHERE id = $1::uuid
		`, id)
		if err := row.Scan(&status, &t, &owner); err != nil {
			return err
		}
		if t != tenantID {
			return ErrTenantMismatch
		}
		if owner != kchatUserID {
			// Cross-user — surface as not-found to avoid leaking
			// the existence of another user's snooze row.
			return ErrNotFound
		}
		switch status {
		case StatusUnsnoozed:
			return ErrAlreadyAwake
		case StatusCancelled:
			return ErrAlreadyCancelled
		case StatusFailed:
			// Defence-in-depth: should be unreachable because the
			// guarded UPDATE above accepts ('snoozed', 'failed').
			// A surprise here means a third concurrent writer
			// flipped the row out from under us — surface as a
			// distinct sentinel so the handler doesn't fold the
			// failure into a misleading "already awake" response.
			return ErrSnoozeFailed
		default:
			// Shouldn't happen — the guarded UPDATE only no-ops
			// when status is outside ('snoozed', 'failed'). A
			// surprise here is a schema-drift bug worth surfacing
			// loudly.
			return fmt.Errorf("snooze.Cancel: guarded UPDATE no-op with unexpected status=%q", status)
		}
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// claimDue is the worker's hot path: pick the next due row that
// no other replica is already processing, bump `attempts`, and
// return the row to the caller. Returns (nil, ErrNotFound) when
// the queue is drained.
//
// `FOR UPDATE OF s SKIP LOCKED` makes the claim safe across N
// worker replicas: each transaction sees a different unlocked row
// while the claim transaction is open. But the lock dies when the
// transaction commits — and the dispatch + bookkeeping happen
// OUTSIDE this transaction (see `DispatchWorker.process`). To
// keep the row off the menu while THIS replica is dispatching,
// the same UPDATE that bumps `attempts` also pushes
// `next_retry_at` forward by `DispatchLeaseInterval`. The
// re-eligibility WHERE clause above (`next_retry_at <= now()`)
// then excludes this row for the full lease duration. If the
// worker crashes mid-dispatch the row sits idle for one lease
// interval before another replica re-claims (acceptable under
// at-least-once delivery); on success `markUnsnoozed` flips
// `status='unsnoozed'` and the row drops out of the partial
// index; on failure `scheduleRetry` overwrites `next_retry_at`
// with the real backoff value, so the lease is a strict floor
// and never extends a retry. Without this push, BeginFunc commits,
// the lock disappears, and a second replica's `claimDue` picks
// up the same row a few milliseconds later — duplicate JMAP
// patch dispatch.
//
// `row.Attempts` is set to the POST-increment value (worker's
// handleErr compares it against maxAttempts; an off-by-one here
// would cause the worker to retry one more time than intended).
// Mirrors `internal/scheduledsend/service.go`.
func (s *Service) claimDue(ctx context.Context) (*Snooze, error) {
	var row Snooze
	leaseUntil := s.now().UTC().Add(DispatchLeaseInterval)
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
		if _, err := tx.Exec(ctx, `
			UPDATE snoozed_emails
			SET attempts = attempts + 1,
			    next_retry_at = $2
			WHERE id = $1::uuid
		`, row.ID, leaseUntil); err != nil {
			return err
		}
		row.Attempts++
		row.NextRetryAt = leaseUntil
		return nil
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
//
// A 0-row update here is operationally interesting: it means
// either (a) a user cancelled the row between this worker's
// claimDue and markUnsnoozed (Cancel won the race), or (b) a
// second worker replica already finished the wake. Both are
// safe — the guarded `AND status = 'snoozed'` clause prevents
// invalid state transitions — but operators want a log line to
// correlate the rare race scenario. Mirrors
// `scheduledsend.Service.markDispatched`.
func (s *Service) markUnsnoozed(ctx context.Context, id string, wokenAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE snoozed_emails
		SET status = 'unsnoozed', woken_at = $2, last_error = ''
		WHERE id = $1::uuid AND status = 'snoozed'
	`, id, wokenAt.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		s.logger.Printf("snooze: markUnsnoozed no-op (likely cancelled or already woken by peer): id=%s", id)
	}
	return nil
}

// markFailed transitions a row to `failed` after the retry cap
// has been hit. Operators inspect the dead-letter rows via the
// admin console.
//
// A 0-row update here is operationally interesting (same
// reasoning as markUnsnoozed) — a user cancel can race the
// worker's terminal failure. Mirrors
// `scheduledsend.Service.markFailed`.
func (s *Service) markFailed(ctx context.Context, id, lastErr string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE snoozed_emails
		SET status = 'failed', last_error = $2
		WHERE id = $1::uuid AND status = 'snoozed'
	`, id, lastErr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		s.logger.Printf("snooze: markFailed no-op (likely cancelled by user between claim and terminal failure): id=%s", id)
	}
	return nil
}

// scheduleRetry pushes `next_retry_at` into the future without
// moving the row out of `snoozed`. Used by the worker after a
// transient Stalwart failure.
//
// A 0-row update means the worker's retry write lost to a Cancel
// commit; the row is correctly in `cancelled` and the worker
// won't re-claim it (the partial index excludes non-`snoozed`).
func (s *Service) scheduleRetry(ctx context.Context, id string, nextRetryAt time.Time, lastErr string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE snoozed_emails
		SET next_retry_at = $2, last_error = $3
		WHERE id = $1::uuid AND status = 'snoozed'
	`, id, nextRetryAt.UTC(), lastErr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		s.logger.Printf("snooze: scheduleRetry no-op (likely cancelled): id=%s", id)
	}
	return nil
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
