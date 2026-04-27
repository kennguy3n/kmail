// Package middleware — TOTP credential persistence.
//
// One row per (tenant_id, user_id) in `totp_credentials`
// (migration 044). All reads and writes set the tenant GUC inside
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
	TenantID            string
	UserID              string
	EncryptedSecret     []byte
	RecoveryCodesHash   string
	Enabled             bool
	CreatedAt           time.Time
	LastUsedAt          *time.Time
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
			       recovery_codes_hash, enabled, created_at, last_used_at
			FROM totp_credentials
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID).Scan(
			&c.TenantID, &c.UserID, &c.EncryptedSecret,
			&c.RecoveryCodesHash, &c.Enabled, &c.CreatedAt, &c.LastUsedAt,
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

// MarkUsed updates `last_used_at`.
func (s *TOTPStore) MarkUsed(ctx context.Context, tenantID, userID string, now time.Time) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE totp_credentials SET last_used_at = $3
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID, now)
		return err
	})
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
