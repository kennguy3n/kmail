package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kennguy3n/kmail/internal/cmk"
)

// RotatingEnvelope enables zero-downtime rotation of the
// kmail-secrets master key (KMAIL_SECRETS_KEY).
//
// The problem: cmk.AESGCMEnvelope is keyed by a single master key.
// Rotating that key in place makes every previously-wrapped row
// (DKIM private keys, TOTP secrets, per-tenant S3 creds, HSM creds)
// fail Unwrap with ErrEnvelopeCorrupted, because they were sealed
// under the old key. The only safe rotation today is "stop the
// world, re-wrap every row, restart" — i.e. downtime.
//
// RotatingEnvelope removes the downtime by holding an ordered
// keyring:
//
//	primary  := envelope for the NEW key  (KMAIL_SECRETS_KEY)
//	retired  := envelopes for OLD keys     (KMAIL_SECRETS_KEY_RETIRED, comma-separated)
//
// Wrap always seals under the primary, so new writes immediately
// use the new key. Unwrap tries the primary first, then each
// retired key in turn, so rows still sealed under an old key keep
// decrypting. A background (or lazy-on-read) re-wrap can then
// migrate rows to the new key at leisure; once every row is
// confirmed re-wrapped, the operator drops the retired key from
// the env and the old key is fully decommissioned — all without a
// single failed read.
//
// It implements cmk.SecretsEnvelope, so it is a drop-in for every
// existing consumer (cmk.LoadEnvelope's callers) with no call-site
// change beyond the one in cmd/kmail-api/main.go.
type RotatingEnvelope struct {
	primary cmk.SecretsEnvelope
	retired []cmk.SecretsEnvelope
}

var _ cmk.SecretsEnvelope = (*RotatingEnvelope)(nil)

// NewRotatingEnvelope builds a keyring from a primary key and zero
// or more retired keys, each in the hex/base64 form
// cmk.NewAESGCMEnvelopeFromKeyMaterial accepts. The primary is
// required; retired keys are optional (none == behaves exactly like
// a single-key envelope).
func NewRotatingEnvelope(primaryKey string, retiredKeys ...string) (*RotatingEnvelope, error) {
	primary, err := cmk.NewAESGCMEnvelopeFromKeyMaterial(primaryKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: primary master key: %w", err)
	}
	re := &RotatingEnvelope{primary: primary}
	for i, rk := range retiredKeys {
		rk = strings.TrimSpace(rk)
		if rk == "" {
			continue
		}
		env, err := cmk.NewAESGCMEnvelopeFromKeyMaterial(rk)
		if err != nil {
			return nil, fmt.Errorf("secrets: retired master key #%d: %w", i, err)
		}
		re.retired = append(re.retired, env)
	}
	return re, nil
}

// Wrap seals plaintext under the primary (newest) key.
func (r *RotatingEnvelope) Wrap(plaintext []byte) ([]byte, error) {
	return r.primary.Wrap(plaintext)
}

// Unwrap tries the primary key, then each retired key. A blob that
// authenticates under any key is returned. Only when every key
// fails to authenticate a magic-prefixed blob do we surface
// cmk.ErrEnvelopeCorrupted — at that point the blob really is
// corrupt or was sealed under a key no longer on the ring.
//
// Legacy (magic-absent) plaintext is handled by the underlying
// AESGCMEnvelope on the primary attempt: it returns
// (blob, false, nil), which we pass straight through. We only fall
// through to retired keys when the primary reports
// ErrEnvelopeCorrupted (magic present, auth failed) — a legacy
// plaintext result is terminal and must not be re-interpreted.
func (r *RotatingEnvelope) Unwrap(blob []byte) ([]byte, bool, error) {
	pt, wasEncrypted, err := r.primary.Unwrap(blob)
	if err == nil {
		return pt, wasEncrypted, nil
	}
	if !errors.Is(err, cmk.ErrEnvelopeCorrupted) {
		// A non-corruption error (e.g. malformed) is not something
		// a different key can fix.
		return nil, false, err
	}
	for _, env := range r.retired {
		pt, wasEncrypted, rerr := env.Unwrap(blob)
		if rerr == nil {
			return pt, wasEncrypted, nil
		}
	}
	// Exhausted the ring: report the primary's corruption error so
	// the operator sees the canonical message.
	return nil, false, err
}

// RetiredKeyCount reports how many retired keys are on the ring.
// Exposed for startup logging / health output so an operator can
// confirm a rotation is in the "both keys live" window.
func (r *RotatingEnvelope) RetiredKeyCount() int { return len(r.retired) }

// LoadEnvelope is the rotation-aware replacement for
// cmk.LoadEnvelope. It resolves the primary master key from the
// supplied Provider (reference "SECRETS_KEY", i.e. KMAIL_SECRETS_KEY
// on the env backend) and any retired keys from
// "SECRETS_KEY_RETIRED" (comma-separated).
//
// When the primary is unset it returns the same kind of error
// cmk.LoadEnvelope does, so main.go's existing dev-fallback
// (log + run unwrapped) keeps working unchanged.
func LoadEnvelope(ctx context.Context, p Provider) (cmk.SecretsEnvelope, error) {
	if p == nil {
		p = EnvProvider{}
	}
	primary, err := p.Resolve(ctx, "SECRETS_KEY")
	if err != nil {
		return nil, fmt.Errorf("cmk: KMAIL_SECRETS_KEY not set: %w", err)
	}
	// Retired keys are optional: a deployment not mid-rotation has
	// none. But only ErrSecretNotFound means "legitimately unset" —
	// any other error (e.g. a Vault transport failure) must NOT be
	// swallowed, or the BFF could boot with a partial keyring and
	// silently fail to decrypt rows still sealed under a retired key.
	var retired []string
	raw, rerr := p.Resolve(ctx, "SECRETS_KEY_RETIRED")
	switch {
	case rerr == nil:
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				retired = append(retired, k)
			}
		}
	case errors.Is(rerr, ErrSecretNotFound):
		// No retired keys configured — fine, single-key envelope.
	default:
		return nil, fmt.Errorf("cmk: resolve KMAIL_SECRETS_KEY_RETIRED: %w", rerr)
	}
	return NewRotatingEnvelope(primary, retired...)
}
