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

	// MLS fields. Populated only when the message was wrapped
	// under an MLS-derived key (i.e. MLS is configured AND the
	// caller supplied sender-leaf + recipient material). For a
	// plain link-portal message these are zero-valued and
	// MLSWrapped reports false.
	//
	// MLSWrappingKey is for server-side bookkeeping (rekey/rotation)
	// only — GetSecureMessage strips it before returning over the
	// public portal so it is never handed to a link-token holder.
	MLSWrapped     bool   `json:"mls_wrapped"`
	MLSWrappingKey string `json:"mls_wrapping_key,omitempty"`
	MLSEpoch       int    `json:"mls_epoch"`
}

// CreateRequest is the input to CreateSecureMessage.
type CreateRequest struct {
	TenantID         string        `json:"tenant_id"`
	SenderID         string        `json:"sender_id"`
	EncryptedBlobRef string        `json:"encrypted_blob_ref"`
	Password         string        `json:"password,omitempty"`
	ExpiresIn        time.Duration `json:"expires_in"`
	MaxViews         int           `json:"max_views"`

	// MLS wrapping inputs. When MLS is configured and BOTH of
	// these are supplied, CreateSecureMessage derives a
	// per-recipient wrapping key from the KChat MLS service and
	// persists it on the link. When MLS is disabled, or the
	// caller omits both, the message degrades to the link-portal
	// flow. Supplying exactly one is a request error — it almost
	// always signals a client that thinks it is sending MLS but
	// isn't.
	SenderLeafKey string   `json:"sender_leaf_key,omitempty"`
	Recipients    []string `json:"recipients,omitempty"`
}

// Service is the implementation.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
	mls  MLSKeyDeriver
}

// NewService returns a service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// WithMLS plugs an MLS key deriver into the service. Pass nil to
// disable MLS — the surrounding flow falls back to the link-based
// portal flow (current behaviour).
func (s *Service) WithMLS(d MLSKeyDeriver) *Service {
	s.mls = d
	return s
}

// MLSEnabled reports whether the wired MLS deriver is configured.
// Frontend code calls a small endpoint to flip the Compose UI
// between MLS and link flows; see handlers.go.
func (s *Service) MLSEnabled() bool {
	if s.mls == nil {
		return false
	}
	if d, ok := s.mls.(*HTTPKeyDeriver); ok {
		return d.Enabled()
	}
	return true
}

// DeriveMLSWrappingKey delegates to the wired deriver. Callers
// receive `ErrMLSDisabled` when MLS is not configured and should
// fall back to CreateSecureMessage.
func (s *Service) DeriveMLSWrappingKey(ctx context.Context, senderLeafKey, recipientCredential string) (string, error) {
	if !s.MLSEnabled() {
		return "", ErrMLSDisabled
	}
	return s.mls.DeriveWrappingKey(ctx, senderLeafKey, recipientCredential)
}

// RekeyConfidentialMessage triggers an MLS rekey when the
// participant set on a confidential link changes (a participant is
// added or removed). It asks the MLS service for a fresh wrapping
// key, then atomically persists the new key, the updated
// participant set, and a bumped epoch on the link row. The new
// wrapping key is returned so the caller can re-wrap the DEK.
//
// The link is loaded tenant-scoped and must be an MLS-wrapped link
// (created with sender-leaf material); rekeying a plain link-portal
// message is rejected with ErrLinkNotFound rather than silently
// upgrading it.
func (s *Service) RekeyConfidentialMessage(ctx context.Context, tenantID, linkID string, newParticipants []string) (string, error) {
	if !s.MLSEnabled() {
		return "", ErrMLSDisabled
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(linkID) == "" {
		return "", errors.New("confidentialsend: tenantID and linkID required")
	}
	if len(newParticipants) == 0 {
		return "", errors.New("confidentialsend: rekey requires at least one participant")
	}
	if s.pool == nil {
		return "", errors.New("confidentialsend: pool not configured")
	}
	newKey, err := s.mls.RekeyConfidentialMessage(ctx, linkID, newParticipants)
	if err != nil {
		return "", fmt.Errorf("confidentialsend: MLS rekey: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE confidential_send_links
			SET mls_wrapping_key = $3,
			    mls_participants  = $4,
			    mls_epoch         = mls_epoch + 1
			WHERE tenant_id = $1::uuid
			  AND id = $2::uuid
			  AND mls_sender_leaf_key <> ''
		`, tenantID, linkID, newKey, newParticipants)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLinkNotFound
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newKey, nil
}

// ErrMLSPartialRequest is returned when a create request supplies
// only one of sender_leaf_key / recipients. MLS wrapping needs
// both; receiving one strongly suggests a client that believes it
// is sending an MLS-wrapped message but is silently falling back to
// the link portal — surfacing an error is safer than degrading
// without telling the caller.
var ErrMLSPartialRequest = errors.New("confidentialsend: MLS send requires both sender_leaf_key and recipients")

// wrapResult carries the MLS material resolved for a new link.
type wrapResult struct {
	wrapped      bool
	wrappingKey  string
	senderLeaf   string
	participants []string
}

// resolveCreateWrapping decides whether a create request is an MLS
// send and, if so, derives the per-recipient wrapping key from the
// MLS service. It is intentionally pool-free so the MLS decision is
// unit-testable without a database.
//
//   - MLS not configured            -> link-only (no error)
//   - MLS configured, no MLS inputs -> link-only (no error)
//   - exactly one MLS input         -> ErrMLSPartialRequest
//   - both MLS inputs               -> derive against the service
func (s *Service) resolveCreateWrapping(ctx context.Context, req CreateRequest) (wrapResult, error) {
	hasLeaf := strings.TrimSpace(req.SenderLeafKey) != ""
	hasRecipients := len(req.Recipients) > 0
	if !hasLeaf && !hasRecipients {
		return wrapResult{}, nil
	}
	if hasLeaf != hasRecipients {
		return wrapResult{}, ErrMLSPartialRequest
	}
	if !s.MLSEnabled() {
		// The caller asked for MLS but the deployment has no MLS
		// endpoint wired. Fail loudly rather than minting a link
		// that pretends to be MLS-protected.
		return wrapResult{}, ErrMLSDisabled
	}
	// One link addresses one external recipient in the portal
	// model; the first credential is the wrapping target and the
	// full slice is retained as the participant set for rekey.
	key, err := s.mls.DeriveWrappingKey(ctx, req.SenderLeafKey, req.Recipients[0])
	if err != nil {
		return wrapResult{}, fmt.Errorf("confidentialsend: derive MLS wrapping key: %w", err)
	}
	return wrapResult{
		wrapped:      true,
		wrappingKey:  key,
		senderLeaf:   req.SenderLeafKey,
		participants: req.Recipients,
	}, nil
}

// CreateSecureMessage mints a token, hashes the password (if
// provided), derives an MLS wrapping key when requested, and
// inserts a row.
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
	// Resolve MLS wrapping BEFORE touching the database so a
	// failed derivation (or partial request) never leaves a
	// dangling link row.
	wrap, err := s.resolveCreateWrapping(ctx, req)
	if err != nil {
		return nil, err
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
				password_hash, expires_at, max_views,
				mls_wrapping_key, mls_sender_leaf_key, mls_participants
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id::text, tenant_id::text, sender_id, link_token,
			          encrypted_blob_ref, expires_at, max_views, view_count,
			          revoked, created_at, mls_epoch
		`, req.TenantID, req.SenderID, token, req.EncryptedBlobRef,
			passwordHash, expiresAt, req.MaxViews,
			wrap.wrappingKey, wrap.senderLeaf, wrap.participants,
		).Scan(
			&m.ID, &m.TenantID, &m.SenderID, &m.LinkToken,
			&m.EncryptedBlobRef, &m.ExpiresAt, &m.MaxViews, &m.ViewCount,
			&m.Revoked, &m.CreatedAt, &m.MLSEpoch,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("create secure message: %w", err)
	}
	m.HasPassword = passwordHash != ""
	m.MLSWrapped = wrap.wrapped
	m.MLSWrappingKey = wrap.wrappingKey
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
			       max_views, view_count, revoked, created_at,
			       mls_wrapping_key, mls_epoch
			FROM confidential_send_links
			WHERE link_token = $1
			FOR UPDATE
		`, token).Scan(
			&m.ID, &m.TenantID, &m.SenderID, &m.LinkToken,
			&m.EncryptedBlobRef, &passwordHash, &m.ExpiresAt,
			&m.MaxViews, &m.ViewCount, &m.Revoked, &m.CreatedAt,
			&m.MLSWrappingKey, &m.MLSEpoch,
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
	m.MLSWrapped = m.MLSWrappingKey != ""
	// The MLS wrapping key is deliberately NOT served over the public
	// portal. For an MLS-wrapped message the recipient is an MLS group
	// member and re-derives the wrapping key from group state via the
	// auth-gated MLS path; emitting it here would hand the key to any
	// holder of the (unauthenticated) link token and defeat the
	// confidentiality the MLS wrapping is meant to provide — the BFF
	// would effectively distribute the unwrap key to portal visitors.
	// The portal exposes only MLSWrapped + MLSEpoch so the client
	// knows to take the MLS-derivation path at the correct epoch.
	m.MLSWrappingKey = ""
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
