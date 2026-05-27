package dns

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/cmk"
)

// newTestEnvelope returns an AES-256-GCM envelope keyed with 32
// random bytes — sufficient for round-trip and tamper-detection
// tests. The key never leaves the test process.
func newTestEnvelope(t *testing.T) cmk.SecretsEnvelope {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		t.Fatalf("read random key material: %v", err)
	}
	env, err := cmk.NewAESGCMEnvelopeFromKeyMaterial(hex.EncodeToString(buf))
	if err != nil {
		t.Fatalf("NewAESGCMEnvelopeFromKeyMaterial: %v", err)
	}
	return env
}

// newQuietService returns a DKIMRotationService whose logger
// writes to a sink we control, so we can assert on the dev-mode
// WARNING line.
func newQuietService(env cmk.SecretsEnvelope) (*DKIMRotationService, *bytes.Buffer) {
	sink := &bytes.Buffer{}
	logger := log.New(sink, "", 0)
	s := &DKIMRotationService{
		logger:   logger,
		envelope: env,
	}
	return s, sink
}

// TestUnwrapPrivateKey_RoundTrip exercises the happy path:
// plaintext -> wrap -> unwrap returns the original bytes.
func TestUnwrapPrivateKey_RoundTrip(t *testing.T) {
	env := newTestEnvelope(t)
	s, _ := newQuietService(env)

	pem := "-----BEGIN PRIVATE KEY-----\nMIIE...test...padding\n-----END PRIVATE KEY-----\n"
	blob, err := s.wrapPrivateKey(pem)
	if err != nil {
		t.Fatalf("wrapPrivateKey: %v", err)
	}
	got, err := s.unwrapPrivateKey(blob)
	if err != nil {
		t.Fatalf("unwrapPrivateKey: %v", err)
	}
	if got != pem {
		t.Errorf("round-trip mismatch:\n got  %q\n want %q", got, pem)
	}
}

// TestUnwrapPrivateKey_LegacyPlaintextPassthrough pins the
// dev-mode behaviour: a row without the magic prefix is returned
// verbatim so pre-envelope DKIM keys keep working.
func TestUnwrapPrivateKey_LegacyPlaintextPassthrough(t *testing.T) {
	s, _ := newQuietService(nil)

	plain := []byte("-----BEGIN PRIVATE KEY-----\nLEGACY\n-----END PRIVATE KEY-----\n")
	got, err := s.unwrapPrivateKey(plain)
	if err != nil {
		t.Fatalf("unwrapPrivateKey: %v", err)
	}
	if got != string(plain) {
		t.Errorf("legacy passthrough mismatch: got %q want %q", got, plain)
	}
}

// TestUnwrapPrivateKey_NilEnvelopeRefusesWrappedBlob is the
// defense-in-depth case backported from the tenant storage path:
// if an operator removes KMAIL_SECRETS_KEY after writing wrapped
// DKIM keys, unwrapPrivateKey must refuse with
// ErrEnvelopeMissingForWrappedKey rather than handing ciphertext
// to Stalwart's PEM parser (which would either fail opaquely or,
// worse, sign outbound mail with garbage).
func TestUnwrapPrivateKey_NilEnvelopeRefusesWrappedBlob(t *testing.T) {
	env := newTestEnvelope(t)
	sWith, _ := newQuietService(env)
	blob, err := sWith.wrapPrivateKey("-----BEGIN PRIVATE KEY-----\nBEFORE\n-----END PRIVATE KEY-----\n")
	if err != nil {
		t.Fatalf("wrapPrivateKey (with env): %v", err)
	}

	sWithout, _ := newQuietService(nil)
	_, err = sWithout.unwrapPrivateKey(blob)
	if !errors.Is(err, ErrEnvelopeMissingForWrappedKey) {
		t.Fatalf("expected ErrEnvelopeMissingForWrappedKey, got %v", err)
	}
}

// TestUnwrapPrivateKey_TamperedBlobSurfacesCorruption checks that
// AEAD authentication failures bubble up as cmk.ErrEnvelopeCorrupted
// instead of returning ciphertext as plaintext.
func TestUnwrapPrivateKey_TamperedBlobSurfacesCorruption(t *testing.T) {
	env := newTestEnvelope(t)
	s, _ := newQuietService(env)

	blob, err := s.wrapPrivateKey("-----BEGIN PRIVATE KEY-----\nORIG\n-----END PRIVATE KEY-----\n")
	if err != nil {
		t.Fatalf("wrapPrivateKey: %v", err)
	}
	if len(blob) < 32 {
		t.Fatalf("wrapped blob unexpectedly short: %d bytes", len(blob))
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01

	_, err = s.unwrapPrivateKey(tampered)
	if !errors.Is(err, cmk.ErrEnvelopeCorrupted) {
		t.Fatalf("expected ErrEnvelopeCorrupted, got %v", err)
	}
}

// TestWrapPrivateKey_NilEnvelopeFallback pins the dev-mode
// WARNING log so a future refactor cannot silently drop it.
func TestWrapPrivateKey_NilEnvelopeFallback(t *testing.T) {
	s, sink := newQuietService(nil)

	pem := "-----BEGIN PRIVATE KEY-----\nDEV\n-----END PRIVATE KEY-----\n"
	blob, err := s.wrapPrivateKey(pem)
	if err != nil {
		t.Fatalf("wrapPrivateKey (nil env): %v", err)
	}
	if string(blob) != pem {
		t.Errorf("nil-envelope wrap altered bytes: got %q want %q", blob, pem)
	}
	if !strings.Contains(sink.String(), "WARNING") || !strings.Contains(sink.String(), "KMAIL_SECRETS_KEY") {
		t.Errorf("expected loud warning about KMAIL_SECRETS_KEY, got %q", sink.String())
	}
}
