package cmk

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// HSMProviderType identifies the wire protocol the appliance
// speaks. Phase 6 supports KMIP-over-TLS and a local
// PKCS#11-loaded module; both are stubbed at the connection layer
// (validate config + persist), real cryptographic operations land
// in a follow-up.
type HSMProviderType string

const (
	HSMKMIP   HSMProviderType = "kmip"
	HSMPKCS11 HSMProviderType = "pkcs11"
)

// HSMConfig is the public projection of `cmk_hsm_configs`.
type HSMConfig struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenant_id"`
	Provider      HSMProviderType `json:"provider_type"`
	Endpoint      string          `json:"endpoint"`
	SlotID        string          `json:"slot_id,omitempty"`
	Status        string          `json:"status"`
	LastTestAt    *time.Time      `json:"last_test_at,omitempty"`
	LastTestError string          `json:"last_test_error,omitempty"`
	LastUsedAt    *time.Time      `json:"last_used_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// HSMRegistration is the input shape for `RegisterHSMKey`.
type HSMRegistration struct {
	Provider    HSMProviderType `json:"provider_type"`
	Endpoint    string          `json:"endpoint"`
	SlotID      string          `json:"slot_id,omitempty"`
	Credentials string          `json:"credentials"`
}

// HSMKeyProvider is the abstraction over a configured HSM. Phase
// 6 ships stubs that validate the connection params and persist
// the row; the real KMIP / PKCS#11 wire integration lands later.
type HSMKeyProvider interface {
	// Validate runs a handshake against the appliance; success
	// flips the row to `active`. The implementation may consult
	// `cfg.Endpoint`, `cfg.SlotID`, and the encrypted credentials
	// the service supplies separately.
	Validate(ctx context.Context, cfg HSMConfig, credentials string) error
}

// KMIPProvider is a stub KMIP HSM provider. Validation enforces
// the endpoint shape (`kmip[s]://host:port`) and any non-empty
// credentials buffer.
type KMIPProvider struct{}

// Validate checks the connection params shape.
func (KMIPProvider) Validate(ctx context.Context, cfg HSMConfig, credentials string) error {
	if cfg.Provider != HSMKMIP {
		return fmt.Errorf("cmk.kmip: wrong provider %q", cfg.Provider)
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return errors.New("cmk.kmip: endpoint required")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "kmip" && u.Scheme != "kmips") {
		return errors.New("cmk.kmip: endpoint must use kmip:// or kmips:// scheme")
	}
	if u.Host == "" {
		return errors.New("cmk.kmip: endpoint must include host:port")
	}
	if strings.TrimSpace(credentials) == "" {
		return errors.New("cmk.kmip: credentials required")
	}
	return nil
}

// PKCS11Provider is a stub PKCS#11 HSM provider. Validation
// enforces the endpoint shape (file path to the .so module) and a
// non-empty slot ID.
type PKCS11Provider struct{}

// Validate checks the connection params shape.
func (PKCS11Provider) Validate(ctx context.Context, cfg HSMConfig, credentials string) error {
	if cfg.Provider != HSMPKCS11 {
		return fmt.Errorf("cmk.pkcs11: wrong provider %q", cfg.Provider)
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return errors.New("cmk.pkcs11: endpoint (path to .so module) required")
	}
	if !strings.HasPrefix(cfg.Endpoint, "/") {
		return errors.New("cmk.pkcs11: endpoint must be an absolute path")
	}
	if strings.TrimSpace(cfg.SlotID) == "" {
		return errors.New("cmk.pkcs11: slot_id required")
	}
	if strings.TrimSpace(credentials) == "" {
		return errors.New("cmk.pkcs11: credentials (PIN) required")
	}
	return nil
}

// providers is the registry the service consults.
var providers = map[HSMProviderType]HSMKeyProvider{
	HSMKMIP:   KMIPProvider{},
	HSMPKCS11: PKCS11Provider{},
}

// ErrEnvelopeNotConfigured signals that a write touched the HSM
// credentials store without a configured kmail-secrets envelope.
// The service refuses to fall back to plaintext storage: an
// operator must wire `KMAIL_SECRETS_KEY` and rebuild the service
// with `NewCMKServiceWithEnvelope` (or `SetEnvelope`).
var ErrEnvelopeNotConfigured = errors.New("cmk: kmail-secrets envelope not configured (set KMAIL_SECRETS_KEY)")

// RegisterHSMKey validates the connection params, enforces
// privacy-plan gating (same as PEM CMK), and persists a row in
// `cmk_hsm_configs` with the credentials AES-256-GCM wrapped by
// the kmail-secrets envelope (`internal/cmk/envelope.go`). The
// service refuses to insert plaintext: if no envelope is wired
// the write returns ErrEnvelopeNotConfigured.
func (s *CMKService) RegisterHSMKey(ctx context.Context, tenantID, plan string, reg HSMRegistration) (*HSMConfig, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("cmk: tenantID required")
	}
	if plan != PrivacyPlan {
		return nil, ErrPlanNotEligible
	}
	if s.pool == nil {
		return nil, errors.New("cmk: pool not configured")
	}
	// Snapshot the envelope once so a concurrent SetEnvelope
	// cannot interleave the nil-check and the Wrap call below.
	envelope := s.getEnvelope()
	if envelope == nil {
		return nil, ErrEnvelopeNotConfigured
	}
	provider, ok := providers[reg.Provider]
	if !ok {
		return nil, fmt.Errorf("cmk: unsupported provider_type %q", reg.Provider)
	}
	cfg := HSMConfig{
		Provider: reg.Provider,
		Endpoint: reg.Endpoint,
		SlotID:   reg.SlotID,
	}
	if err := provider.Validate(ctx, cfg, reg.Credentials); err != nil {
		return nil, err
	}
	wrapped, err := envelope.Wrap([]byte(reg.Credentials))
	if err != nil {
		return nil, fmt.Errorf("cmk: wrap HSM credentials: %w", err)
	}
	var out HSMConfig
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO cmk_hsm_configs
			    (tenant_id, provider_type, endpoint, slot_id, credentials_encrypted, status)
			VALUES ($1::uuid, $2, $3, $4, $5, 'pending')
			RETURNING id::text, tenant_id::text, provider_type, endpoint, slot_id,
			          status, last_test_at, last_test_error, created_at, updated_at
		`, tenantID, string(reg.Provider), reg.Endpoint, reg.SlotID, wrapped).Scan(
			&out.ID, &out.TenantID, &out.Provider, &out.Endpoint, &out.SlotID,
			&out.Status, &out.LastTestAt, &out.LastTestError, &out.CreatedAt, &out.UpdatedAt,
		)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListHSMConfigs returns every config row for a tenant.
func (s *CMKService) ListHSMConfigs(ctx context.Context, tenantID string) ([]HSMConfig, error) {
	if s.pool == nil || tenantID == "" {
		return nil, nil
	}
	var out []HSMConfig
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, provider_type, endpoint, slot_id,
			       status, last_test_at, last_test_error, last_used_at,
			       created_at, updated_at
			FROM cmk_hsm_configs
			WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c HSMConfig
			if err := rows.Scan(&c.ID, &c.TenantID, &c.Provider, &c.Endpoint, &c.SlotID,
				&c.Status, &c.LastTestAt, &c.LastTestError, &c.LastUsedAt,
				&c.CreatedAt, &c.UpdatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// EncryptDEK runs the operation-specific KMIP / PKCS#11 wire
// path (Phase 8) for the configured HSM. The KMIP path connects
// over TLS and exchanges Encrypt; the PKCS#11 path requires the
// `pkcs11` build tag (see pkcs11.go for the build-tag matrix).
// Updates `last_used_at` on success.
func (s *CMKService) EncryptDEK(ctx context.Context, tenantID, configID, keyLabel string, plaintext []byte) (ciphertext, iv []byte, err error) {
	cfg, creds, err := s.loadHSMConfig(ctx, tenantID, configID)
	if err != nil {
		return nil, nil, err
	}
	// Best-effort PIN scrubbing. Go's GC may have already copied
	// the underlying bytes elsewhere by the time the wire call
	// returns (and the `string(creds)` conversion on the KMIP path
	// produces an unreachable heap copy that cannot be zeroed at
	// all), so this is a defense-in-depth measure rather than a
	// hard guarantee. It still narrows the in-process exposure
	// window from "until next GC + N other allocations" down to
	// "until function return". See pkcs11Encrypt doc for the full
	// lifetime-contract rationale.
	defer clear(creds)
	switch cfg.Provider {
	case HSMKMIP:
		client := NewKMIPClient(strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "kmips://"), "kmip://"), nil)
		client.Username = ""
		client.Password = string(creds)
		ciphertext, iv, err = client.Encrypt(keyLabel, plaintext)
	case HSMPKCS11:
		// `creds` carries the unwrapped PKCS#11 PIN — the cgo
		// build needs it for `C_Login`. The no-cgo shim ignores
		// the slice and returns errPKCS11NotBuilt.
		ciphertext, iv, err = pkcs11Encrypt(ctx, *cfg, creds, keyLabel, plaintext)
	default:
		return nil, nil, fmt.Errorf("cmk: unsupported provider_type %q", cfg.Provider)
	}
	if err != nil {
		return nil, nil, err
	}
	if updErr := s.markHSMUsed(ctx, tenantID, configID); updErr != nil {
		// Non-fatal: the envelope succeeded.
		_ = updErr
	}
	return ciphertext, iv, nil
}

// DecryptDEK is the symmetric counterpart of EncryptDEK.
func (s *CMKService) DecryptDEK(ctx context.Context, tenantID, configID, keyLabel string, ciphertext, iv []byte) ([]byte, error) {
	cfg, creds, err := s.loadHSMConfig(ctx, tenantID, configID)
	if err != nil {
		return nil, err
	}
	// See EncryptDEK for the lifetime rationale.
	defer clear(creds)
	var out []byte
	switch cfg.Provider {
	case HSMKMIP:
		client := NewKMIPClient(strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "kmips://"), "kmip://"), nil)
		client.Password = string(creds)
		out, err = client.Decrypt(keyLabel, ciphertext, iv)
	case HSMPKCS11:
		out, err = pkcs11Decrypt(ctx, *cfg, creds, keyLabel, ciphertext, iv)
	default:
		return nil, fmt.Errorf("cmk: unsupported provider_type %q", cfg.Provider)
	}
	if err != nil {
		return nil, err
	}
	_ = s.markHSMUsed(ctx, tenantID, configID)
	return out, nil
}

// loadHSMConfig pulls the row + decrypted credentials for use by
// EncryptDEK / DecryptDEK. The stored bytes are unwrapped through
// the kmail-secrets envelope; legacy plaintext rows (written
// before the envelope landed) are returned verbatim so the
// migration story is "new writes go through Wrap, reads accept
// either".
func (s *CMKService) loadHSMConfig(ctx context.Context, tenantID, configID string) (*HSMConfig, []byte, error) {
	if s.pool == nil {
		return nil, nil, errors.New("cmk: pool not configured")
	}
	var cfg HSMConfig
	var creds []byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, provider_type, endpoint, slot_id,
			       credentials_encrypted, status, last_test_at, last_test_error,
			       last_used_at, created_at, updated_at
			FROM cmk_hsm_configs
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, configID, tenantID).Scan(
			&cfg.ID, &cfg.TenantID, &cfg.Provider, &cfg.Endpoint, &cfg.SlotID,
			&creds, &cfg.Status, &cfg.LastTestAt, &cfg.LastTestError,
			&cfg.LastUsedAt, &cfg.CreatedAt, &cfg.UpdatedAt,
		)
	})
	if err != nil {
		return nil, nil, err
	}
	envelope := s.getEnvelope()
	plain, wasEncrypted, err := s.unwrapHSMCredentialsWith(envelope, creds)
	if err != nil {
		return nil, nil, err
	}
	if !wasEncrypted && envelope != nil && len(creds) > 0 {
		s.warnLegacyPlaintextHSM(tenantID, configID)
	}
	return &cfg, plain, nil
}

// unwrapHSMCredentials decrypts the stored credential blob with
// the configured envelope and returns the plaintext bytes plus a
// flag indicating whether the blob was AEAD-protected. The
// envelope's magic-prefix check distinguishes three states:
//
//   - Blob carries the magic prefix AND auth succeeds →
//     plaintext, wasEncrypted=true.
//   - Blob carries the magic prefix AND auth fails →
//     ErrEnvelopeCorrupted bubbles up. This is the case the bot
//     called out: a master-key rotation leaves the previous
//     epoch's rows un-decryptable, and surfacing this as an error
//     prevents the worse failure of silently returning ciphertext
//     as if it were a legacy plaintext credential.
//   - Blob has NO magic prefix → legacy plaintext written before
//     the envelope landed; returned as-is with wasEncrypted=false
//     so warnLegacyPlaintextHSM can log a one-time migration
//     signal at the call site.
//
// When no envelope is configured the blob is returned as-is for
// read compatibility, but writes still require the envelope — see
// RegisterHSMKey.
func (s *CMKService) unwrapHSMCredentials(blob []byte) ([]byte, bool, error) {
	return s.unwrapHSMCredentialsWith(s.getEnvelope(), blob)
}

// unwrapHSMCredentialsWith is the same as unwrapHSMCredentials
// but takes a pre-snapshotted envelope so a caller that has
// already pulled the envelope at function entry doesn't acquire
// the read lock a second time and can be sure the same envelope
// instance is used for both the nil-check at the call site and
// the Unwrap call here.
func (s *CMKService) unwrapHSMCredentialsWith(envelope SecretsEnvelope, blob []byte) ([]byte, bool, error) {
	if envelope == nil {
		return blob, false, nil
	}
	plain, wasEncrypted, err := envelope.Unwrap(blob)
	if err != nil {
		return nil, false, fmt.Errorf("cmk: unwrap HSM credentials: %w", err)
	}
	return plain, wasEncrypted, nil
}

// warnLegacyPlaintextHSM logs a single warning the first time we
// observe a legacy-plaintext credential blob for a given
// (tenant, config) pair. Operators are expected to re-register
// affected HSM configs through the API so the next write goes
// through the envelope.
func (s *CMKService) warnLegacyPlaintextHSM(tenantID, configID string) {
	key := tenantID + "/" + configID
	if _, loaded := s.legacyPlaintextSeen.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if s.logger != nil {
		s.logger.Printf(
			"cmk: legacy-plaintext HSM credentials detected tenant=%s config=%s — "+
				"re-register this HSM config through the API to wrap with the envelope; "+
				"this warning will only fire once per (tenant, config) per process",
			tenantID, configID,
		)
	}
}

// markHSMUsed bumps `last_used_at` to now() (Phase 8 column).
func (s *CMKService) markHSMUsed(ctx context.Context, tenantID, configID string) error {
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE cmk_hsm_configs SET last_used_at = now()
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, configID, tenantID)
		return err
	})
}

// TestHSMConnection re-runs the provider's Validate against a
// stored row and updates `status`, `last_test_at`,
// `last_test_error` accordingly. The persisted credential blob is
// unwrapped through the kmail-secrets envelope before it is fed
// to the provider.
func (s *CMKService) TestHSMConnection(ctx context.Context, tenantID, configID string) (*HSMConfig, error) {
	if s.pool == nil {
		return nil, errors.New("cmk: pool not configured")
	}
	var out HSMConfig
	var creds []byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, provider_type, endpoint, slot_id,
			       credentials_encrypted, status, last_test_at, last_test_error,
			       last_used_at, created_at, updated_at
			FROM cmk_hsm_configs
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, configID, tenantID).Scan(
			&out.ID, &out.TenantID, &out.Provider, &out.Endpoint, &out.SlotID,
			&creds, &out.Status, &out.LastTestAt, &out.LastTestError,
			&out.LastUsedAt, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			return err
		}
		provider, ok := providers[out.Provider]
		if !ok {
			return fmt.Errorf("cmk: unsupported provider_type %q", out.Provider)
		}
		envelope := s.getEnvelope()
		plain, wasEncrypted, err := s.unwrapHSMCredentialsWith(envelope, creds)
		if err != nil {
			return err
		}
		if !wasEncrypted && envelope != nil && len(creds) > 0 {
			s.warnLegacyPlaintextHSM(tenantID, configID)
		}
		validateErr := provider.Validate(ctx, out, string(plain))
		newStatus := "active"
		errMsg := ""
		if validateErr != nil {
			newStatus = "failed"
			errMsg = validateErr.Error()
		}
		_, err = tx.Exec(ctx, `
			UPDATE cmk_hsm_configs
			SET status = $3, last_test_at = now(), last_test_error = $4
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, configID, tenantID, newStatus, errMsg)
		if err != nil {
			return err
		}
		out.Status = newStatus
		out.LastTestError = errMsg
		now := time.Now().UTC()
		out.LastTestAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
