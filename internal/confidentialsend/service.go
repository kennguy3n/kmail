// Package confidentialsend — Phase 5 Confidential Send portal.
//
// Confidential Send produces a one-time, externally-shareable
// link backed by an encrypted blob in zk-object-fabric (StrictZK
// mode — the BFF never sees plaintext). The DB row only stores
// the link token, the blob reference, an optional bcrypt
// password hash, expiry, and view-count caps. Public portal
// reads are allowed without auth but are rate-limited (5 attempts
// per token per 15 min) at the handler layer.
package confidentialsend

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// SecureMessage is the public link shape. The password hash is
// never returned over the wire; `HasPassword` indicates whether
// the recipient must supply one.
type SecureMessage struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	SenderID         string    `json:"sender_id"`
	LinkToken        string    `json:"link_token"`
	EncryptedBlobRef string    `json:"encrypted_blob_ref,omitempty"`
	HasPassword      bool      `json:"has_password"`
	ExpiresAt        time.Time `json:"expires_at"`
	MaxViews         int       `json:"max_views"`
	ViewCount        int       `json:"view_count"`
	Revoked          bool      `json:"revoked"`
	CreatedAt        time.Time `json:"created_at"`
}

// CreateRequest is the input to CreateSecureMessage.
type CreateRequest struct {
	TenantID         string        `json:"tenant_id"`
	SenderID         string        `json:"sender_id"`
	EncryptedBlobRef string        `json:"encrypted_blob_ref"`
	Password         string        `json:"password,omitempty"`
	ExpiresIn        time.Duration `json:"expires_in"`
	MaxViews         int           `json:"max_views"`
}

// Service is the implementation.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewService returns a service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// CreateSecureMessage mints a token, hashes the password (if
// provided), and inserts a row.
func (s *Service) CreateSecureMessage(ctx context.Context, req CreateRequest) (*SecureMessage, error) {
	if strings.TrimSpace(req.TenantID) == "" {
		return nil, errors.New("confidentialsend: tenant_id required")
	}
	if strings.TrimSpace(req.SenderID) == "" {
		return nil, errors.New("confidentialsend: sender_id required")
	}
	if strings.TrimSpace(req.EncryptedBlobRef) == "" {
		return nil, errors.New("confidentialsend: encrypted_blob_ref required")
	}
	if req.ExpiresIn <= 0 {
		req.ExpiresIn = 24 * time.Hour
	}
	if req.ExpiresIn > 30*24*time.Hour {
		return nil, errors.New("confidentialsend: expires_in cannot exceed 30 days")
	}
	if req.MaxViews < 0 {
		return nil, errors.New("confidentialsend: max_views must be >= 0 (0 = unlimited)")
	}
	if s.pool == nil {
		return nil, errors.New("confidentialsend: pool not configured")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	var passwordHash string
	if req.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		passwordHash = string(hash)
	}
	expiresAt := s.now().Add(req.ExpiresIn)

	var m SecureMessage
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, req.TenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO confidential_send_links (
				tenant_id, sender_id, link_token, encrypted_blob_ref,
				password_hash, expires_at, max_views
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
			RETURNING id::text, tenant_id::text, sender_id, link_token,
			          encrypted_blob_ref, expires_at, max_views, view_count,
			          revoked, created_at
		`, req.TenantID, req.SenderID, token, req.EncryptedBlobRef,
			passwordHash, expiresAt, req.MaxViews,
		).Scan(
			&m.ID, &m.TenantID, &m.SenderID, &m.LinkToken,
			&m.EncryptedBlobRef, &m.ExpiresAt, &m.MaxViews, &m.ViewCount,
			&m.Revoked, &m.CreatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("create secure message: %w", err)
	}
	m.HasPassword = passwordHash != ""
	return &m, nil
}

// ErrLinkExpired is returned when the token has expired.
var ErrLinkExpired = errors.New("confidentialsend: link expired")

// ErrLinkRevoked is returned for revoked links.
var ErrLinkRevoked = errors.New("confidentialsend: link revoked")

// ErrViewsExceeded is returned when max_views has been reached.
var ErrViewsExceeded = errors.New("confidentialsend: max views exceeded")

// ErrInvalidPassword is returned for an incorrect password.
var ErrInvalidPassword = errors.New("confidentialsend: invalid password")

// ErrLinkNotFound is returned for an unknown token.
var ErrLinkNotFound = errors.New("confidentialsend: link not found")

// GetSecureMessage validates the link token + password and (on
// success) atomically increments the view counter. Returns the
// blob reference so the caller can hand it to the client portal.
func (s *Service) GetSecureMessage(ctx context.Context, token, password string) (*SecureMessage, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrLinkNotFound
	}
	if s.pool == nil {
		return nil, errors.New("confidentialsend: pool not configured")
	}
	var (
		m            SecureMessage
		passwordHash string
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Public-portal lookups bypass tenant scope: the unique
		// token is the gating identity. RLS still allows the read
		// because the policy admits rows when the GUC is unset.
		err := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, sender_id, link_token,
			       encrypted_blob_ref, password_hash, expires_at,
			       max_views, view_count, revoked, created_at
			FROM confidential_send_links
			WHERE link_token = $1
			FOR UPDATE
		`, token).Scan(
			&m.ID, &m.TenantID, &m.SenderID, &m.LinkToken,
			&m.EncryptedBlobRef, &passwordHash, &m.ExpiresAt,
			&m.MaxViews, &m.ViewCount, &m.Revoked, &m.CreatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLinkNotFound
		}
		if err != nil {
			return err
		}
		if m.Revoked {
			return ErrLinkRevoked
		}
		if s.now().After(m.ExpiresAt) {
			return ErrLinkExpired
		}
		if m.MaxViews > 0 && m.ViewCount >= m.MaxViews {
			return ErrViewsExceeded
		}
		if passwordHash != "" {
			if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
				return ErrInvalidPassword
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE confidential_send_links
			SET view_count = view_count + 1
			WHERE id = $1::uuid
		`, m.ID)
		if err != nil {
			return err
		}
		m.ViewCount++
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.HasPassword = passwordHash != ""
	return &m, nil
}

// RevokeLink marks a link revoked.
func (s *Service) RevokeLink(ctx context.Context, tenantID, linkID string) error {
	if tenantID == "" || linkID == "" {
		return errors.New("confidentialsend: tenantID and linkID required")
	}
	if s.pool == nil {
		return errors.New("confidentialsend: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE confidential_send_links
			SET revoked = true
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, linkID)
		return err
	})
}

// ListSentSecureMessages returns the most recent links for a
// tenant, optionally scoped to a sender. Strips password hashes
// and blob refs.
func (s *Service) ListSentSecureMessages(ctx context.Context, tenantID, senderID string) ([]SecureMessage, error) {
	if tenantID == "" {
		return nil, errors.New("confidentialsend: tenantID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []SecureMessage
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
			rows pgx.Rows
			err  error
		)
		if senderID == "" {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, sender_id, link_token,
				       password_hash <> '' AS has_password, expires_at,
				       max_views, view_count, revoked, created_at
				FROM confidential_send_links
				WHERE tenant_id = $1::uuid
				ORDER BY created_at DESC
				LIMIT 200
			`, tenantID)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, sender_id, link_token,
				       password_hash <> '' AS has_password, expires_at,
				       max_views, view_count, revoked, created_at
				FROM confidential_send_links
				WHERE tenant_id = $1::uuid AND sender_id = $2
				ORDER BY created_at DESC
				LIMIT 200
			`, tenantID, senderID)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m SecureMessage
			if err := rows.Scan(
				&m.ID, &m.TenantID, &m.SenderID, &m.LinkToken,
				&m.HasPassword, &m.ExpiresAt, &m.MaxViews, &m.ViewCount,
				&m.Revoked, &m.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// newToken returns a 32-byte URL-safe random token.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
