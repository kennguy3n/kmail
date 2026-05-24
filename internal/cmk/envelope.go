// Package cmk — symmetric AEAD envelope wrapper.
//
// This is the kmail-secrets envelope referenced throughout the
// codebase (DKIM private keys, HSM credentials, planned vault
// fields). It is intentionally simple: AES-256-GCM with a
// per-record 12-byte random nonce, prepended to the ciphertext
// and tagged with a 16-byte magic prefix so a wrapped blob is
// unambiguously identifiable on read. The master key is sourced
// from the `KMAIL_SECRETS_KEY` environment variable (32-byte hex
// or base64). A 32-byte master is required — Wrap returns an
// error when the envelope is unconfigured so callers fail closed
// instead of silently storing plaintext.
//
// Wire shape (little-endian byte stream):
//
//	output = magic(16) || nonce(12) || ciphertext_with_tag
//
//	magic = "kmail-cmk-v1\x00\x00\x00\x00" (constant, 16 bytes)
//
// Backwards-compatible read:
//
//	Unwrap is a four-state state machine. The magic prefix
//	disambiguates the two unambiguous cases; AEAD auth on the
//	old layout disambiguates the remaining two:
//
//	• Magic present + GCM auth OK   → plaintext, wasEncrypted=true.
//	• Magic present + GCM auth fails → ERROR (corruption / wrong
//	  key / tampered row). Callers must NOT silently fall through
//	  to plaintext, which would surface raw ciphertext as if it
//	  were a legacy plaintext credential.
//	• Magic absent  + old-format GCM auth OK → plaintext,
//	  wasEncrypted=true. The blob was written by an EARLIER
//	  revision of this envelope (no magic, layout = nonce||ct).
//	  This preserves data continuity for any deployment that
//	  encrypted DKIM keys or TOTP secrets before the magic
//	  prefix was introduced.
//	• Magic absent  + old-format GCM auth fails → legacy
//	  plaintext written before the envelope landed; return as-is
//	  with wasEncrypted=false. Callers WARN once per (tenant,
//	  config) via warnLegacyPlaintextHSM so the migration is
//	  visible.
//
// Note: the "magic-absent + auth-fails" case loses the
// key-rotation safety guarantee that the magic prefix provides
// for new-format blobs (because we cannot distinguish "wrong key
// on old-format ciphertext" from "genuine legacy plaintext").
// The window during which this matters is bounded: it ends as
// soon as every old-format row has been read once and re-written
// by Wrap, which produces a new-format blob. For deployments
// that have NEVER written old-format ciphertext (i.e. they never
// ran a pre-magic-prefix revision of this code), every read is
// a new-format read and the key-rotation guarantee is exact.
//
// New writes always go through Wrap, so over time the database
// settles into all-ciphertext with magic prefix.
package cmk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// SecretsEnvelope is the small interface DKIM (and any future
// caller) needs.
type SecretsEnvelope interface {
	Wrap(plaintext []byte) ([]byte, error)
	Unwrap(blob []byte) (plaintext []byte, wasEncrypted bool, err error)
}

// envelopeMagic identifies a wrapped blob unambiguously so Unwrap
// can distinguish corruption (magic + auth fail) from a legacy
// plaintext credential (no magic). 16 bytes is small enough not to
// bloat per-record storage and large enough that the probability
// of an organic 16-byte collision in legacy plaintext is
// negligible.
var envelopeMagic = [16]byte{
	'k', 'm', 'a', 'i', 'l', '-', 'c', 'm', 'k', '-', 'v', '1',
	0x00, 0x00, 0x00, 0x00,
}

// ErrEnvelopeCorrupted is returned by Unwrap when a blob carries
// the kmail-cmk-v1 magic prefix but fails AEAD authentication. The
// likely causes are (a) the master key was rotated and this row
// is from the previous epoch, (b) the row was tampered with at
// rest, or (c) the ciphertext was truncated. Callers MUST surface
// this error rather than treating the blob as legacy plaintext.
var ErrEnvelopeCorrupted = errors.New("cmk envelope: wrapped blob failed AEAD authentication (key rotation or corruption)")

// AESGCMEnvelope is the production implementation.
type AESGCMEnvelope struct {
	aead cipher.AEAD
}

// LoadEnvelope reads the master key from the environment and
// returns an AES-GCM envelope. When the env var is unset, it
// returns nil and a non-nil error so the caller can decide
// whether to fall back to a no-op envelope (dev) or refuse to
// boot (production).
func LoadEnvelope() (SecretsEnvelope, error) {
	raw := strings.TrimSpace(os.Getenv("KMAIL_SECRETS_KEY"))
	if raw == "" {
		return nil, errors.New("cmk: KMAIL_SECRETS_KEY not set")
	}
	return NewAESGCMEnvelopeFromKeyMaterial(raw)
}

// NewAESGCMEnvelopeFromKeyMaterial accepts hex (64 chars) or
// base64 (any padding) encoding of a 32-byte master key.
func NewAESGCMEnvelopeFromKeyMaterial(material string) (SecretsEnvelope, error) {
	key, err := decodeKeyMaterial(material)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cmk envelope: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cmk envelope: cipher.NewGCM: %w", err)
	}
	return &AESGCMEnvelope{aead: aead}, nil
}

// Wrap returns magic||nonce||ciphertext.
func (e *AESGCMEnvelope) Wrap(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("cmk envelope: read nonce: %w", err)
	}
	ct := e.aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(envelopeMagic)+len(nonce)+len(ct))
	out = append(out, envelopeMagic[:]...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Unwrap reverses Wrap. It is a four-state state machine (see
// package doc); the magic prefix disambiguates the two
// unambiguous cases, and AEAD auth on the old layout
// disambiguates the remaining two:
//
//   - Magic present + GCM auth OK     → returns plaintext, true, nil.
//   - Magic present + GCM auth fails  → returns nil, false,
//     ErrEnvelopeCorrupted. The likely cause is key rotation
//     pointed at the previous epoch's rows; callers MUST surface
//     this rather than silently returning ciphertext-as-plaintext.
//   - Magic absent  + old-format auth OK → returns plaintext,
//     true, nil. This is the data-continuity path for blobs
//     written by an earlier revision of this envelope (no magic,
//     layout = nonce||ct).
//   - Magic absent  + old-format auth fails → returns blob, false,
//     nil. Treated as legacy plaintext (written before the
//     envelope landed) so migration callers can warn once and
//     continue.
func (e *AESGCMEnvelope) Unwrap(blob []byte) ([]byte, bool, error) {
	ns := e.aead.NonceSize()
	ovh := e.aead.Overhead()

	if len(blob) >= len(envelopeMagic) && bytes.Equal(blob[:len(envelopeMagic)], envelopeMagic[:]) {
		// New format: magic || nonce || ciphertext.
		body := blob[len(envelopeMagic):]
		if len(body) < ns+ovh {
			return nil, false, ErrEnvelopeCorrupted
		}
		nonce := body[:ns]
		ct := body[ns:]
		pt, err := e.aead.Open(nil, nonce, ct, nil)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %v", ErrEnvelopeCorrupted, err)
		}
		return pt, true, nil
	}

	// Magic absent. Could be (a) pre-magic-prefix ciphertext
	// written by an earlier revision of this envelope, or (b)
	// genuine legacy plaintext that pre-dates the envelope.
	// Attempt AEAD decrypt with the old layout (nonce at offset
	// 0): on success it is (a) and we return the plaintext with
	// wasEncrypted=true; on failure it is (b) and we return the
	// blob as-is. The probability that random plaintext bytes
	// pass GCM authentication is 2^-128, so the disambiguation
	// is reliable.
	if len(blob) >= ns+ovh {
		nonce := blob[:ns]
		ct := blob[ns:]
		if pt, err := e.aead.Open(nil, nonce, ct, nil); err == nil {
			return pt, true, nil
		}
	}
	return blob, false, nil
}

// NoopEnvelope is the dev fallback: wraps and unwraps as identity
// transforms. Callers are responsible for deciding whether using
// this in production is acceptable (it isn't, for DKIM private
// keys).
type NoopEnvelope struct{}

func (NoopEnvelope) Wrap(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil }
func (NoopEnvelope) Unwrap(b []byte) ([]byte, bool, error) {
	return append([]byte(nil), b...), false, nil
}

func decodeKeyMaterial(s string) ([]byte, error) {
	if len(s) == 64 {
		if k, err := hex.DecodeString(s); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := base64.URLEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	if k, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(k) == 32 {
		return k, nil
	}
	return nil, errors.New("cmk envelope: master key must be 32 bytes (hex or base64)")
}
