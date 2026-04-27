// Package middleware — WebAuthn credential persistence.
//
// `webauthn_credentials` carries one row per security key per
// (tenant, user). The pattern mirrors the rest of the codebase:
// SetTenantGUC inside a single transaction so RLS holds the line
// even if the SQL is wrong.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebAuthnCredential is the API + DB shape of one row in
// `webauthn_credentials`. The public-key blob is stored as a
// base64url string of the COSE bytes the browser hands us.
type WebAuthnCredential struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	UserID       string    `json:"user_id"`
	CredentialID string    `json:"credential_id"`
	PublicKey    string    `json:"public_key"`
	SignCount    int64     `json:"sign_count"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// WebAuthnStore wraps the pgxpool with the small CRUD surface the
// handlers need.
type WebAuthnStore struct {
	pool *pgxpool.Pool
}

// NewWebAuthnStore returns a store. A nil pool is OK in tests —
// every method short-circuits to an empty result.
func NewWebAuthnStore(pool *pgxpool.Pool) *WebAuthnStore {
	return &WebAuthnStore{pool: pool}
}

// Insert persists a credential. The (tenant_id, credential_id)
// pair is unique; conflicts return ErrDuplicate.
func (s *WebAuthnStore) Insert(ctx context.Context, c WebAuthnCredential) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, c.TenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO webauthn_credentials (
				tenant_id, user_id, credential_id, public_key, name, created_at
			) VALUES (
				$1::uuid, $2, $3, $4, $5, $6
			)`,
			c.TenantID, c.UserID, c.CredentialID, c.PublicKey, c.Name, c.CreatedAt)
		return err
	})
}

// ListByUser returns every credential belonging to (tenant, user)
// ordered by creation date.
func (s *WebAuthnStore) ListByUser(ctx context.Context, tenantID, userID string) ([]WebAuthnCredential, error) {
	if s.pool == nil {
		return nil, nil
	}
	var out []WebAuthnCredential
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, user_id, credential_id, public_key,
			       sign_count, name, created_at, last_used_at
			  FROM webauthn_credentials
			 WHERE tenant_id = $1::uuid AND user_id = $2
			 ORDER BY created_at DESC`,
			tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c WebAuthnCredential
			if err := rows.Scan(&c.ID, &c.TenantID, &c.UserID, &c.CredentialID,
				&c.PublicKey, &c.SignCount, &c.Name, &c.CreatedAt, &c.LastUsedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return out, nil
}

// Get returns one credential by (tenant, credential_id).
func (s *WebAuthnStore) Get(ctx context.Context, tenantID, credentialID string) (*WebAuthnCredential, error) {
	if s.pool == nil {
		return nil, errors.New("webauthn store: no pool")
	}
	var c WebAuthnCredential
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, user_id, credential_id, public_key,
			       sign_count, name, created_at, last_used_at
			  FROM webauthn_credentials
			 WHERE tenant_id = $1::uuid AND credential_id = $2`,
			tenantID, credentialID)
		return row.Scan(&c.ID, &c.TenantID, &c.UserID, &c.CredentialID,
			&c.PublicKey, &c.SignCount, &c.Name, &c.CreatedAt, &c.LastUsedAt)
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// BumpSignCount increments the credential's signature counter and
// stamps last_used_at.
func (s *WebAuthnStore) BumpSignCount(ctx context.Context, tenantID, credentialID string, when time.Time) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE webauthn_credentials
			   SET sign_count = sign_count + 1,
			       last_used_at = $3
			 WHERE tenant_id = $1::uuid AND credential_id = $2`,
			tenantID, credentialID, when)
		return err
	})
}

// Delete removes a credential by id, scoped to (tenant, user).
func (s *WebAuthnStore) Delete(ctx context.Context, tenantID, userID, id string) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM webauthn_credentials
			 WHERE tenant_id = $1::uuid AND user_id = $2 AND id = $3::uuid`,
			tenantID, userID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("credential not found")
		}
		return nil
	})
}

// MemoryChallenger is the dev / test Challenger backed by an
// in-process map. Production wires the Valkey-backed challenger.
type MemoryChallenger struct {
	mu    sync.Mutex
	store map[string]memoryChallengeEntry
}

type memoryChallengeEntry struct {
	value     []byte
	expiresAt time.Time
}

// NewMemoryChallenger returns a MemoryChallenger.
func NewMemoryChallenger() *MemoryChallenger {
	return &MemoryChallenger{store: map[string]memoryChallengeEntry{}}
}

// StoreChallenge persists a challenge with the given TTL.
func (m *MemoryChallenger) StoreChallenge(_ context.Context, key string, challenge []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = memoryChallengeEntry{value: challenge, expiresAt: time.Now().Add(ttl)}
	return nil
}

// LoadChallenge retrieves a challenge if it has not yet expired.
func (m *MemoryChallenger) LoadChallenge(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.store[key]
	if !ok {
		return nil, errors.New("challenge not found")
	}
	if time.Now().After(entry.expiresAt) {
		delete(m.store, key)
		return nil, errors.New("challenge expired")
	}
	return entry.value, nil
}

// DeleteChallenge removes a challenge.
func (m *MemoryChallenger) DeleteChallenge(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, key)
	return nil
}
