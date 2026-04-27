// Package dns — DKIM key rotation.
//
// Phase 7 adds a DKIM rotation service so a tenant can rotate
// their signing key without the operator hand-rolling a Stalwart
// JMAP admin call. The new `dkim_keys` table (migration 040)
// stores the per-domain key history; one key is `active` at a
// time, deprecated keys stay around until DNS publishes the
// rotation, and revoked keys are tombstoned for audit.
//
// The wire shape:
//
//   - `GenerateKeyPair` produces a 2048-bit RSA pair (PKCS#8 PEM
//     for the private key, base64-DER for the public key — the
//     selector record format Stalwart expects).
//   - `RotateKey` inserts the new key as `active`, marks the
//     previously active row as `deprecated`, and (when wired)
//     pushes the change to Stalwart through the JMAP admin
//     registry.
//   - `RevokeKey` tombstones a key (e.g. after a private-key
//     leak). The selector stays in DNS until the next active
//     rotation so in-flight signed mail keeps validating.
//
// The DNS wizard (`DnsWizard.tsx`) reads the `pending` flag on
// the active key to show the new selector record once an
// operator has triggered a rotation but the tenant has not yet
// published it.
package dns

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/cmk"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// DKIMKeyStatus enumerates the lifecycle states a DKIM key can be
// in. Stored verbatim in `dkim_keys.status`.
type DKIMKeyStatus string

const (
	DKIMKeyActive     DKIMKeyStatus = "active"
	DKIMKeyDeprecated DKIMKeyStatus = "deprecated"
	DKIMKeyRevoked    DKIMKeyStatus = "revoked"
)

// DKIMKey is the API + DB shape of one row in `dkim_keys`. The
// private-key blob is omitted from the JSON marshal — only the
// admin handlers running with cmk-class authority see it.
type DKIMKey struct {
	ID         string        `json:"id"`
	TenantID   string        `json:"tenant_id"`
	DomainID   string        `json:"domain_id"`
	Selector   string        `json:"selector"`
	PublicKey  string        `json:"public_key"`
	Status     DKIMKeyStatus `json:"status"`
	CreatedAt  time.Time     `json:"created_at"`
	ActivatedAt *time.Time   `json:"activated_at,omitempty"`
	ExpiresAt  *time.Time    `json:"expires_at,omitempty"`
	RevokedAt  *time.Time    `json:"revoked_at,omitempty"`

	privateKeyEncrypted []byte
}

// DKIMKeyPair is a freshly generated RSA pair, returned by
// `GenerateKeyPair`. The selector is auto-derived from
// `time.Now().UTC().Format("20060102")` so two operators rotating
// the same domain on different days get distinct selectors.
type DKIMKeyPair struct {
	Selector   string
	PublicKey  string
	PrivateKey string
}

// PendingRotation is the `pending` view exposed to the DNS wizard:
// the new selector + DNS record that the tenant must publish
// before the rotation can complete.
type PendingRotation struct {
	Selector  string `json:"selector"`
	Record    string `json:"record"`
	NewKeyID  string `json:"new_key_id"`
	StartedAt time.Time `json:"started_at"`
}

// DKIMRotationService manages per-domain DKIM key rotation.
type DKIMRotationService struct {
	pool   *pgxpool.Pool
	logger *log.Logger
	// Pusher is invoked on RotateKey to update Stalwart's JMAP
	// admin registry. Optional — when nil the rotation only
	// touches the local database (the operator publishes the new
	// selector manually).
	Pusher StalwartDKIMPusher
	// envelope wraps the private key before INSERT and unwraps
	// it before handing back to Pusher / external callers. When
	// nil, the service writes plaintext PEM (dev only) and logs
	// a warning at rotate time.
	envelope cmk.SecretsEnvelope
}

// StalwartDKIMPusher is the slice of Stalwart's JMAP admin API
// the rotation service needs. Defining the interface here lets
// tests stub the call without spinning up a real Stalwart.
type StalwartDKIMPusher interface {
	PushDKIMKey(ctx context.Context, domain, selector, privateKeyPEM string) error
}

// NewDKIMRotationService returns a service bound to the pool.
func NewDKIMRotationService(pool *pgxpool.Pool, logger *log.Logger) *DKIMRotationService {
	if logger == nil {
		logger = log.Default()
	}
	return &DKIMRotationService{pool: pool, logger: logger}
}

// WithPusher attaches a Stalwart JMAP admin pusher.
func (s *DKIMRotationService) WithPusher(p StalwartDKIMPusher) *DKIMRotationService {
	s.Pusher = p
	return s
}

// WithEnvelope attaches the kmail-secrets AEAD envelope used to
// wrap private keys at rest. Production callers MUST inject one;
// without it, the service logs a loud warning and stores raw PEM.
func (s *DKIMRotationService) WithEnvelope(env cmk.SecretsEnvelope) *DKIMRotationService {
	s.envelope = env
	return s
}

// GenerateKeyPair returns a fresh 2048-bit RSA key pair encoded
// in the format Stalwart expects: PKCS#8 PEM for the private key,
// base64-DER for the public key. The selector is derived from the
// current UTC timestamp (e.g. "20260427T153012") so two rotations
// of the same domain on the same day produce distinct selectors —
// the `dkim_keys` table has a UNIQUE (domain_id, selector) constraint
// that would otherwise reject the second rotation.
func (s *DKIMRotationService) GenerateKeyPair() (DKIMKeyPair, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return DKIMKeyPair{}, fmt.Errorf("rsa generate: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return DKIMKeyPair{}, fmt.Errorf("marshal pkcs8: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return DKIMKeyPair{}, fmt.Errorf("marshal pub: %w", err)
	}
	return DKIMKeyPair{
		Selector:   time.Now().UTC().Format("20060102T150405"),
		PublicKey:  base64.StdEncoding.EncodeToString(pubDER),
		PrivateKey: string(privPEM),
	}, nil
}

// ListKeys returns every DKIM key for a (tenant, domain) tuple,
// newest first.
func (s *DKIMRotationService) ListKeys(ctx context.Context, tenantID, domainID string) ([]DKIMKey, error) {
	if tenantID == "" || domainID == "" {
		return nil, fmt.Errorf("%w: tenantID and domainID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []DKIMKey
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, domain_id::text,
			       selector, public_key, status,
			       created_at, activated_at, expires_at, revoked_at
			FROM dkim_keys
			WHERE tenant_id = $1::uuid AND domain_id = $2::uuid
			ORDER BY created_at DESC`, tenantID, domainID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k DKIMKey
			if err := rows.Scan(&k.ID, &k.TenantID, &k.DomainID, &k.Selector,
				&k.PublicKey, &k.Status, &k.CreatedAt,
				&k.ActivatedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list dkim: %w", err)
	}
	return out, nil
}

// RotateKey generates a new key, marks the previously active key
// as deprecated, persists the new key as active, and (when a
// Pusher is wired) pushes the new private key into Stalwart.
func (s *DKIMRotationService) RotateKey(ctx context.Context, tenantID, domainID string) (DKIMKey, error) {
	if tenantID == "" || domainID == "" {
		return DKIMKey{}, fmt.Errorf("%w: tenantID and domainID required", ErrInvalidInput)
	}
	pair, err := s.GenerateKeyPair()
	if err != nil {
		return DKIMKey{}, err
	}
	wrappedPriv, err := s.wrapPrivateKey(pair.PrivateKey)
	if err != nil {
		return DKIMKey{}, fmt.Errorf("wrap private key: %w", err)
	}
	var domainName string
	var newKey DKIMKey
	if s.pool != nil {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT name FROM domains WHERE tenant_id = $1::uuid AND id = $2::uuid`,
				tenantID, domainID).Scan(&domainName); err != nil {
				return fmt.Errorf("lookup domain: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE dkim_keys
				   SET status = 'deprecated'
				 WHERE tenant_id = $1::uuid AND domain_id = $2::uuid AND status = 'active'`,
				tenantID, domainID); err != nil {
				return err
			}
			row := tx.QueryRow(ctx, `
				INSERT INTO dkim_keys (
					tenant_id, domain_id, selector, public_key,
					private_key_encrypted, status, activated_at
				) VALUES (
					$1::uuid, $2::uuid, $3, $4, $5::bytea, 'active', now()
				)
				RETURNING id::text, created_at, activated_at`,
				tenantID, domainID, pair.Selector, pair.PublicKey, wrappedPriv)
			return row.Scan(&newKey.ID, &newKey.CreatedAt, &newKey.ActivatedAt)
		})
		if err != nil {
			return DKIMKey{}, fmt.Errorf("rotate: %w", err)
		}
	}
	newKey.TenantID = tenantID
	newKey.DomainID = domainID
	newKey.Selector = pair.Selector
	newKey.PublicKey = pair.PublicKey
	newKey.Status = DKIMKeyActive
	if s.Pusher != nil && domainName != "" {
		if err := s.Pusher.PushDKIMKey(ctx, domainName, pair.Selector, pair.PrivateKey); err != nil {
			s.logger.Printf("dkim push to stalwart failed for %s: %v", domainName, err)
		}
	}
	return newKey, nil
}

// RevokeKey tombstones a key. Historic mail signed by the key
// keeps validating until DNS rolls forward.
func (s *DKIMRotationService) RevokeKey(ctx context.Context, tenantID, domainID, keyID string) error {
	if tenantID == "" || domainID == "" || keyID == "" {
		return fmt.Errorf("%w: tenantID, domainID, keyID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE dkim_keys
			   SET status = 'revoked', revoked_at = now()
			 WHERE tenant_id = $1::uuid AND domain_id = $2::uuid AND id = $3::uuid`,
			tenantID, domainID, keyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// PendingRotation returns the rotation the DNS wizard should
// surface — i.e. the active key whose selector hasn't yet been
// published in DNS. Callers that need only the active key can
// scan ListKeys directly; this convenience exists so the wizard
// does not have to filter by status itself.
func (s *DKIMRotationService) PendingRotation(ctx context.Context, tenantID, domainID, defaultRecord string) (*PendingRotation, error) {
	keys, err := s.ListKeys(ctx, tenantID, domainID)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.Status != DKIMKeyActive {
			continue
		}
		started := k.CreatedAt
		return &PendingRotation{
			Selector:  k.Selector,
			Record:    fmt.Sprintf("v=DKIM1; k=rsa; p=%s", k.PublicKey),
			NewKeyID:  k.ID,
			StartedAt: started,
		}, nil
	}
	if defaultRecord != "" {
		return &PendingRotation{Selector: "kmail", Record: defaultRecord}, nil
	}
	return nil, errors.New("no rotations pending")
}

// wrapPrivateKey runs the configured envelope over the raw PEM
// bytes. When no envelope is configured, it logs a loud warning
// and returns the plaintext — production callers MUST inject
// `WithEnvelope(...)` to avoid the warning.
func (s *DKIMRotationService) wrapPrivateKey(privPEM string) ([]byte, error) {
	if s.envelope == nil {
		s.logger.Printf("dkim: WARNING: no SecretsEnvelope configured; storing PRIVATE KEY as plaintext (set KMAIL_SECRETS_KEY)")
		return []byte(privPEM), nil
	}
	return s.envelope.Wrap([]byte(privPEM))
}

// LoadPrivateKey fetches the encrypted private key for a given
// (tenant, domain, keyID) and unwraps it through the configured
// envelope. Used by callers that need to push the key out of
// band (e.g. backfill against a fresh Stalwart instance).
func (s *DKIMRotationService) LoadPrivateKey(ctx context.Context, tenantID, domainID, keyID string) (string, error) {
	if tenantID == "" || domainID == "" || keyID == "" {
		return "", fmt.Errorf("%w: tenantID, domainID, keyID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return "", errors.New("dkim: pool not configured")
	}
	var blob []byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT private_key_encrypted
			  FROM dkim_keys
			 WHERE tenant_id = $1::uuid AND domain_id = $2::uuid AND id = $3::uuid`,
			tenantID, domainID, keyID).Scan(&blob)
	})
	if err != nil {
		return "", err
	}
	if s.envelope == nil {
		return string(blob), nil
	}
	pt, _, err := s.envelope.Unwrap(blob)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
