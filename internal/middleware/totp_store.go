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
				enabled             = EXCLUDED.enabled
		`, tenantID, userID, encryptedSecret, recoveryHash, enabled, now)
		return err
	})
}

// MarkUsed updates `last_used_at` and clears the brute-force
// lockout state (a successful verification resets the counter and
// any standing lock). Callers invoke this on every successful
// TOTP/recovery verification.
func (s *TOTPStore) MarkUsed(ctx context.Context, tenantID, userID string, now time.Time) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE totp_credentials
			SET last_used_at = $3, failed_attempts = 0, locked_until = NULL
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID, now)
		return err
	})
}

// RegisterFailure records one failed verification for (tenant,
// user) and, when the consecutive-failure count reaches
// maxAttempts, parks the account until now+lockFor. The increment
// and the lock decision happen in a single atomic UPDATE so
// concurrent failed attempts cannot race past the ceiling. When a
// lock is applied the counter resets to 0 so that, after the lock
// elapses, the account starts a fresh window rather than
// re-locking on the very next failure. Returns the resulting
// locked_until (nil when the account is not currently locked).
func (s *TOTPStore) RegisterFailure(ctx context.Context, tenantID, userID string, now time.Time, maxAttempts int, lockFor time.Duration) (*time.Time, error) {
	if s.pool == nil {
		return nil, nil
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	lockUntil := now.Add(lockFor)
	var locked *time.Time
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			UPDATE totp_credentials
			SET failed_attempts = CASE WHEN failed_attempts + 1 >= $3 THEN 0 ELSE failed_attempts + 1 END,
			    locked_until    = CASE WHEN failed_attempts + 1 >= $3 THEN $4::timestamptz ELSE locked_until END
			WHERE tenant_id = $1::uuid AND user_id = $2
			RETURNING locked_until
		`, tenantID, userID, maxAttempts, lockUntil).Scan(&locked)
	})
	if err != nil {
		return nil, err
	}
	return locked, nil
}

// UpdateRecoveryCodes replaces the recovery-codes hash bundle.
func (s *TOTPStore) UpdateRecoveryCodes(ctx context.Context, tenantID, userID, hash string) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE totp_credentials SET recovery_codes_hash = $3
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID, hash)
		return err
	})
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
