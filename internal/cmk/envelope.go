// Package cmk — symmetric AEAD envelope wrapper.
//
// This is the kmail-secrets envelope referenced throughout the
// codebase (DKIM private keys, HSM credentials, planned vault
// fields). It is intentionally simple: AES-256-GCM with a
// per-record 12-byte random nonce, prepended to the ciphertext.
// The master key is sourced from the `KMAIL_SECRETS_KEY`
// environment variable (32-byte hex or base64). A 32-byte
// master is required — Wrap returns an error when the envelope
// is unconfigured so callers fail closed instead of silently
// storing plaintext.
//
// Wire shape (little-endian byte stream):
//
//	output = nonce(12) || ciphertext_with_tag
//
// Backwards-compatible read:
//
//	if Unwrap can't decrypt (e.g. legacy plaintext), it returns
//	the input unchanged AND `wasEncrypted=false`. New writes always
//	go through Wrap, so over time the database settles into
//	all-ciphertext.
package cmk

import (
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

// Wrap returns nonce||ciphertext.
func (e *AESGCMEnvelope) Wrap(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("cmk envelope: read nonce: %w", err)
	}
	ct := e.aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Unwrap reverses Wrap. If the blob doesn't decrypt with the
// configured key (e.g. it's legacy plaintext PEM written before
// the envelope landed), it returns the input verbatim with
// wasEncrypted=false.
func (e *AESGCMEnvelope) Unwrap(blob []byte) ([]byte, bool, error) {
	ns := e.aead.NonceSize()
	if len(blob) < ns+e.aead.Overhead() {
		return blob, false, nil
	}
	nonce := blob[:ns]
	ct := blob[ns:]
	pt, err := e.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// Treat as legacy plaintext rather than blowing up — the
		// migration story is "write encrypted, read either".
		return blob, false, nil
	}
	return pt, true, nil
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
