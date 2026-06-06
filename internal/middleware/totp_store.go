// Package middleware — TOTP credential persistence.
//
// One row per (tenant_id, user_id) in `totp_credentials`
// (see `migrations/001_baseline.sql`). All reads and writes set the tenant GUC inside
// a single transaction so RLS holds even on accidentally-broad
// SQL.
package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TOTPCredential is the on-the-wire + DB shape.
type TOTPCredential struct {
	TenantID          string
	UserID            string
	EncryptedSecret   []byte
	RecoveryCodesHash string
	Enabled           bool
	CreatedAt         time.Time
	LastUsedAt        *time.Time
	// FailedAttempts counts consecutive failed verifications since
	// the last success or lockout. LockedUntil, when non-nil and in
	// the future, parks the account: verification is refused until
	// it elapses. Both are reset on a successful verification.
	FailedAttempts int
	LockedUntil    *time.Time
}

// TOTPStore wraps the pool with the small CRUD surface the
// handlers need.
type TOTPStore struct {
	pool *pgxpool.Pool
}

// NewTOTPStore returns a store. A nil pool short-circuits to
// in-memory no-ops so handlers stay testable.
func NewTOTPStore(pool *pgxpool.Pool) *TOTPStore {
	return &TOTPStore{pool: pool}
}

// ErrTOTPNotFound is returned when no credential row exists.
var ErrTOTPNotFound = errors.New("totp: not found")

// Get returns the credential row for (tenant, user). When the
// pool is nil the helper returns ErrTOTPNotFound — useful for
// keeping handler control flow simple in tests.
func (s *TOTPStore) Get(ctx context.Context, tenantID, userID string) (*TOTPCredential, error) {
	if s.pool == nil {
		return nil, ErrTOTPNotFound
	}
	var c TOTPCredential
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT tenant_id::text, user_id, encrypted_secret,
			       recovery_codes_hash, enabled, created_at, last_used_at,
			       failed_attempts, locked_until
			FROM totp_credentials
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID).Scan(
			&c.TenantID, &c.UserID, &c.EncryptedSecret,
			&c.RecoveryCodesHash, &c.Enabled, &c.CreatedAt, &c.LastUsedAt,
			&c.FailedAttempts, &c.LockedUntil,
		)
	})
	if err != nil {
		return nil, ErrTOTPNotFound
	}
	return &c, nil
}

// Upsert creates or updates a credential row.
func (s *TOTPStore) Upsert(ctx context.Context, tenantID, userID string, encryptedSecret []byte, recoveryHash string, enabled bool, now time.Time) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO totp_credentials (
				tenant_id, user_id, encrypted_secret, recovery_codes_hash, enabled, created_at
			) VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, user_id) DO UPDATE SET
				encrypted_secret    = EXCLUDED.encrypted_secret,
				recovery_codes_hash = EXCLUDED.recovery_codes_hash,
				enabled             = EXCLUDED.enabled,
				-- (Re)enrollment is a fresh credential lifecycle, so it
				-- must clear any brute-force lockout left over on the
				-- existing row. Without this, failed attempts recorded
				-- during enrollment confirmation (verify) would persist
				-- into the login (check) phase and the account could be
				-- locked out on its first wrong code there. New INSERTs
				-- default these columns to 0/NULL already.
				failed_attempts     = 0,
				locked_until        = NULL
		`, tenantID, userID, encryptedSecret, recoveryHash, enabled, now)
		return err
	})
}

// TOTPVerification is the verifier's decision about a submitted
// code, evaluated against the credential row while it is locked
// FOR UPDATE inside EvaluateAttempt.
type TOTPVerification struct {
	// OK reports whether the submitted code was accepted.
	OK bool
	// Method labels the accepted factor ("totp" | "recovery").
	Method string
	// SetRecoveryHash, when non-nil, is written to
	// recovery_codes_hash on success — used to persist a consumed
	// recovery bundle, or a freshly minted bundle on enrollment.
	// Ignored on failure.
	SetRecoveryHash *string
	// SetEnabled, when non-nil, is written to enabled on success —
	// used by enrollment confirmation to flip the credential live.
	// Ignored on failure.
	SetEnabled *bool
	// Err, when non-nil, aborts the attempt without spending one
	// (e.g. a secret-envelope failure). The transaction rolls back
	// and the error is surfaced to the caller; no counter changes.
	Err error
}

// TOTPAttemptResult is the outcome of EvaluateAttempt.
type TOTPAttemptResult struct {
	// NotEnabled is set (only when requireEnabled was requested) if
	// the credential exists but is not yet enabled.
	NotEnabled bool
	// Locked is set when the account is currently within its lockout
	// window; RetryAfter is the time remaining. No attempt is spent.
	Locked     bool
	RetryAfter time.Duration
	// Verified reports whether the submitted code was accepted, and
	// Method labels the factor used.
	Verified bool
	Method   string
}

// EvaluateAttempt runs a brute-force-guarded verification
// atomically. It locks the credential row (SELECT ... FOR UPDATE)
// so that all concurrent attempts for a single account are fully
// serialized — closing the check-then-act race where a burst of
// guesses could be evaluated (or a recovery code double-spent)
// before the incremented counter / lock became visible to peers.
//
// Everything happens inside one transaction:
//  1. lock + read the row (ErrTOTPNotFound when absent);
//  2. when requireEnabled and the row is not enabled, return NotEnabled;
//  3. when locked_until is still in the future, return Locked (no
//     attempt spent);
//  4. run verify() against the locked row;
//  5. on success: reset failed_attempts/locked_until, bump
//     last_used_at, and apply any SetRecoveryHash / SetEnabled —
//     all in the same statement;
//  6. on failure: increment failed_attempts and apply the lockout
//     once the ceiling is reached (counter resets to 0 at lock time
//     so the post-cooldown window starts fresh).
func (s *TOTPStore) EvaluateAttempt(
	ctx context.Context,
	tenantID, userID string,
	now time.Time,
	maxAttempts int,
	lockFor time.Duration,
	requireEnabled bool,
	verify func(cred *TOTPCredential) TOTPVerification,
) (TOTPAttemptResult, error) {
	if s.pool == nil {
		return TOTPAttemptResult{}, ErrTOTPNotFound
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var res TOTPAttemptResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var c TOTPCredential
		if err := tx.QueryRow(ctx, `
			SELECT tenant_id::text, user_id, encrypted_secret,
			       recovery_codes_hash, enabled, created_at, last_used_at,
			       failed_attempts, locked_until
			FROM totp_credentials
			WHERE tenant_id = $1::uuid AND user_id = $2
			FOR UPDATE
		`, tenantID, userID).Scan(
			&c.TenantID, &c.UserID, &c.EncryptedSecret,
			&c.RecoveryCodesHash, &c.Enabled, &c.CreatedAt, &c.LastUsedAt,
			&c.FailedAttempts, &c.LockedUntil,
		); err != nil {
			// Only a genuinely absent row is "not enrolled". A
			// transient DB error (connection drop, context
			// cancellation, etc.) must surface as a real error so the
			// handler returns 500 rather than masking it as a 401 —
			// otherwise infrastructure blips look like auth rejections.
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTOTPNotFound
			}
			return err
		}

		if requireEnabled && !c.Enabled {
			res.NotEnabled = true
			return nil
		}
		if c.LockedUntil != nil {
			if remaining := c.LockedUntil.Sub(now); remaining > 0 {
				res.Locked = true
				res.RetryAfter = remaining
				return nil
			}
		}

		v := verify(&c)
		if v.Err != nil {
			return v.Err
		}
		if !v.OK {
			_, err := tx.Exec(ctx, `
				UPDATE totp_credentials
				SET failed_attempts = CASE WHEN failed_attempts + 1 >= $3 THEN 0 ELSE failed_attempts + 1 END,
				    locked_until    = CASE WHEN failed_attempts + 1 >= $3 THEN $4::timestamptz ELSE locked_until END
				WHERE tenant_id = $1::uuid AND user_id = $2
			`, tenantID, userID, maxAttempts, now.Add(lockFor))
			return err
		}

		res.Verified = true
		res.Method = v.Method
		_, err := tx.Exec(ctx, `
			UPDATE totp_credentials
			SET last_used_at        = $3,
			    failed_attempts     = 0,
			    locked_until        = NULL,
			    recovery_codes_hash = COALESCE($4::text, recovery_codes_hash),
			    enabled             = COALESCE($5::boolean, enabled)
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID, now, v.SetRecoveryHash, v.SetEnabled)
		return err
	})
	if err != nil {
		return TOTPAttemptResult{}, err
	}
	return res, nil
}

// Delete removes a credential row.
func (s *TOTPStore) Delete(ctx context.Context, tenantID, userID string) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM totp_credentials
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID)
		return err
	})
}
