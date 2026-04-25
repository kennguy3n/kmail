// Package cmk — Phase 5 Customer-managed keys.
//
// Tenants on the privacy plan can register a public key that
// wraps the KMail-side data encryption keys. The public key (PEM)
// is the only thing this service ever sees: rotation deprecates
// the previous active key but keeps it readable for re-wrap
// during the migration window; revocation marks the key
// terminally unusable so the next CMK-backed operation refuses
// to proceed.
//
// Plan gating happens at the handler edge — `RegisterKey`
// requires the caller to pass an already-validated plan; the
// service refuses to register for non-`privacy` tenants.
package cmk

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Status mirrors the SQL CHECK constraint.
type Status string

const (
	StatusActive     Status = "active"
	StatusDeprecated Status = "deprecated"
	StatusRevoked    Status = "revoked"
)

// PrivacyPlan is the only tenant plan that may register a CMK.
const PrivacyPlan = "privacy"

// Key is the public CMK shape.
type Key struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	KeyFingerprint string    `json:"key_fingerprint"`
	PublicKeyPEM   string    `json:"public_key_pem"`
	Status         Status    `json:"status"`
	Algorithm      string    `json:"algorithm"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CMKService is the service implementation.
type CMKService struct {
	pool *pgxpool.Pool
}

// NewCMKService returns a service.
func NewCMKService(pool *pgxpool.Pool) *CMKService {
	return &CMKService{pool: pool}
}

// ErrPlanNotEligible is returned when a non-privacy tenant tries
// to register a CMK.
var ErrPlanNotEligible = errors.New("cmk: tenant plan not eligible (privacy plan required)")

// RegisterKey validates the PEM, computes a fingerprint, and
// inserts a new active key. The caller must pass the tenant's
// plan; only `privacy` tenants are allowed to register.
func (s *CMKService) RegisterKey(ctx context.Context, tenantID, plan, publicKeyPEM, algorithm string) (*Key, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("cmk: tenantID required")
	}
	if plan != PrivacyPlan {
		return nil, ErrPlanNotEligible
	}
	pem := strings.TrimSpace(publicKeyPEM)
	if pem == "" {
		return nil, errors.New("cmk: public_key_pem required")
	}
	fp, err := fingerprintPEM(pem)
	if err != nil {
		return nil, err
	}
	if algorithm == "" {
		algorithm = "RSA-OAEP-256"
	}
	if s.pool == nil {
		return nil, errors.New("cmk: pool not configured")
	}
	var k Key
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO customer_managed_keys (
				tenant_id, key_fingerprint, public_key_pem, status, algorithm
			) VALUES ($1::uuid, $2, $3, 'active', $4)
			RETURNING id::text, tenant_id::text, key_fingerprint, public_key_pem,
			          status, algorithm, created_at, updated_at
		`, tenantID, fp, pem, algorithm).Scan(
			&k.ID, &k.TenantID, &k.KeyFingerprint, &k.PublicKeyPEM,
			&k.Status, &k.Algorithm, &k.CreatedAt, &k.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("register cmk key: %w", err)
	}
	return &k, nil
}

// RotateKey deprecates the current active key (and any other
// non-revoked rows) and registers a new active one. Atomic: either
// the deprecation + insert both land or neither does.
func (s *CMKService) RotateKey(ctx context.Context, tenantID, plan, publicKeyPEM, algorithm string) (*Key, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("cmk: tenantID required")
	}
	if plan != PrivacyPlan {
		return nil, ErrPlanNotEligible
	}
	pem := strings.TrimSpace(publicKeyPEM)
	if pem == "" {
		return nil, errors.New("cmk: public_key_pem required")
	}
	fp, err := fingerprintPEM(pem)
	if err != nil {
		return nil, err
	}
	if algorithm == "" {
		algorithm = "RSA-OAEP-256"
	}
	if s.pool == nil {
		return nil, errors.New("cmk: pool not configured")
	}
	var k Key
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE customer_managed_keys
			SET status = 'deprecated'
			WHERE tenant_id = $1::uuid AND status = 'active'
		`, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO customer_managed_keys (
				tenant_id, key_fingerprint, public_key_pem, status, algorithm
			) VALUES ($1::uuid, $2, $3, 'active', $4)
			RETURNING id::text, tenant_id::text, key_fingerprint, public_key_pem,
			          status, algorithm, created_at, updated_at
		`, tenantID, fp, pem, algorithm).Scan(
			&k.ID, &k.TenantID, &k.KeyFingerprint, &k.PublicKeyPEM,
			&k.Status, &k.Algorithm, &k.CreatedAt, &k.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("rotate cmk key: %w", err)
	}
	return &k, nil
}

// RevokeKey marks a specific key revoked. Idempotent.
func (s *CMKService) RevokeKey(ctx context.Context, tenantID, keyID string) error {
	if tenantID == "" || keyID == "" {
		return errors.New("cmk: tenantID and keyID required")
	}
	if s.pool == nil {
		return errors.New("cmk: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE customer_managed_keys
			SET status = 'revoked'
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, keyID)
		return err
	})
}

// GetActiveKey returns the current active key, or `(nil, nil)`
// when none is registered.
func (s *CMKService) GetActiveKey(ctx context.Context, tenantID string) (*Key, error) {
	if tenantID == "" {
		return nil, errors.New("cmk: tenantID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var k Key
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, key_fingerprint, public_key_pem,
			       status, algorithm, created_at, updated_at
			FROM customer_managed_keys
			WHERE tenant_id = $1::uuid AND status = 'active'
			ORDER BY created_at DESC
			LIMIT 1
		`, tenantID).Scan(
			&k.ID, &k.TenantID, &k.KeyFingerprint, &k.PublicKeyPEM,
			&k.Status, &k.Algorithm, &k.CreatedAt, &k.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoActive
		}
		return err
	})
	if errors.Is(err, errNoActive) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// ListKeys returns every key for a tenant, newest first.
func (s *CMKService) ListKeys(ctx context.Context, tenantID string) ([]Key, error) {
	if tenantID == "" {
		return nil, errors.New("cmk: tenantID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []Key
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, key_fingerprint, public_key_pem,
			       status, algorithm, created_at, updated_at
			FROM customer_managed_keys
			WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k Key
			if err := rows.Scan(
				&k.ID, &k.TenantID, &k.KeyFingerprint, &k.PublicKeyPEM,
				&k.Status, &k.Algorithm, &k.CreatedAt, &k.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// errNoActive is a sentinel that lets GetActiveKey distinguish
// the "no rows" case from real query errors without leaking the
// pgx-internal error type to callers.
var errNoActive = errors.New("cmk: no active key")

// fingerprintPEM validates the PEM block and returns the SHA-256
// fingerprint of the DER-encoded public key (lowercase hex).
func fingerprintPEM(pemStr string) (string, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return "", errors.New("cmk: invalid PEM (no decodable block)")
	}
	if !strings.Contains(strings.ToUpper(block.Type), "PUBLIC KEY") {
		return "", fmt.Errorf("cmk: PEM block must be a public key (got %q)", block.Type)
	}
	if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		// Fall back to PKCS1 in case the customer hands us a bare
		// RSA public key — both are commonly produced by HSM
		// exports.
		if _, err2 := x509.ParsePKCS1PublicKey(block.Bytes); err2 != nil {
			return "", fmt.Errorf("cmk: parse public key: %w", err)
		}
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}
