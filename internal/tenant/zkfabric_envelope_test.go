package tenant

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/cmk"
)

// newTestEnvelope returns a fresh AES-256-GCM `cmk.SecretsEnvelope`
// backed by random key material. Mirrors the helper used by the
// HSM envelope tests in `internal/cmk/hsm_envelope_test.go` so
// the two suites exercise the same wrap/unwrap surface.
func newTestEnvelope(t *testing.T) cmk.SecretsEnvelope {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	env, err := cmk.NewAESGCMEnvelopeFromKeyMaterial(hex.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewAESGCMEnvelopeFromKeyMaterial: %v", err)
	}
	return env
}

func newQuietProvisioner(env cmk.SecretsEnvelope) (*ZKFabricProvisioner, *bytes.Buffer) {
	var sink bytes.Buffer
	return NewZKFabricProvisioner(ZKFabricProvisioner{
		Envelope: env,
		Logger:   log.New(&sink, "", 0),
	}), &sink
}

// TestWrapSecretKey_WithEnvelope_ProducesCiphertext asserts that
// when an envelope is configured, `wrapSecretKey` returns a blob
// that (a) does not equal the plaintext and (b) carries the
// `kmail-cmk-v1` magic prefix that disambiguates wrapped blobs
// from legacy plaintext on the read path.
func TestWrapSecretKey_WithEnvelope_ProducesCiphertext(t *testing.T) {
	env := newTestEnvelope(t)
	p, sink := newQuietProvisioner(env)

	plaintext := "AKIA-test-secret-key-1234567890"
	blob, err := p.wrapSecretKey(plaintext)
	if err != nil {
		t.Fatalf("wrapSecretKey: %v", err)
	}

	if bytes.Equal(blob, []byte(plaintext)) {
		t.Fatal("wrap returned plaintext-equivalent bytes; envelope was not applied")
	}
	if !bytes.HasPrefix(blob, []byte("kmail-cmk-v1")) {
		t.Fatalf("wrapped blob missing kmail-cmk-v1 magic prefix; got prefix %q", string(blob[:min(16, len(blob))]))
	}
	if strings.Contains(sink.String(), "WARNING") {
		t.Errorf("unexpected warning logged when envelope present: %q", sink.String())
	}
}

// TestUnwrapSecretKey_RoundTrip exercises the happy path:
// plaintext → wrap → unwrap returns the original plaintext and
// `wasEncrypted=true`.
func TestUnwrapSecretKey_RoundTrip(t *testing.T) {
	env := newTestEnvelope(t)
	p, _ := newQuietProvisioner(env)

	plaintext := "secret-with-+/=base64-shaped\x00bytes"
	blob, err := p.wrapSecretKey(plaintext)
	if err != nil {
		t.Fatalf("wrapSecretKey: %v", err)
	}

	got, wasEnc, err := p.unwrapSecretKey(blob)
	if err != nil {
		t.Fatalf("unwrapSecretKey: %v", err)
	}
	if got != plaintext {
		t.Errorf("round-trip mismatch:\n got  %q\n want %q", got, plaintext)
	}
	if !wasEnc {
		t.Errorf("wasEncrypted=false for envelope-wrapped blob")
	}
}

// TestUnwrapSecretKey_TamperedBlobSurfacesCorruption checks that
// a wrapped blob whose ciphertext was modified at rest does NOT
// silently pass through as plaintext — `cmk.ErrEnvelopeCorrupted`
// must surface so the caller can refuse to use the row.
func TestUnwrapSecretKey_TamperedBlobSurfacesCorruption(t *testing.T) {
	env := newTestEnvelope(t)
	p, _ := newQuietProvisioner(env)

	blob, err := p.wrapSecretKey("secret-original")
	if err != nil {
		t.Fatalf("wrapSecretKey: %v", err)
	}

	// Flip a bit in the ciphertext tail (past the 16-byte magic
	// prefix and 12-byte nonce). GCM authentication MUST reject.
	if len(blob) < 32 {
		t.Fatalf("wrapped blob unexpectedly short: %d bytes", len(blob))
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01

	_, _, err = p.unwrapSecretKey(tampered)
	if !errors.Is(err, cmk.ErrEnvelopeCorrupted) {
		t.Fatalf("expected ErrEnvelopeCorrupted, got %v", err)
	}
}

// TestUnwrapSecretKey_LegacyPlaintextPassthrough mirrors the DKIM
// behaviour: rows written before the envelope landed contain raw
// plaintext bytes. The unwrap path must return them verbatim with
// `wasEncrypted=false` so the read can succeed and the lookup
// caller can log a one-shot migration warning.
func TestUnwrapSecretKey_LegacyPlaintextPassthrough(t *testing.T) {
	env := newTestEnvelope(t)
	p, _ := newQuietProvisioner(env)

	plain := []byte("legacy-plaintext-secret-no-magic")
	got, wasEnc, err := p.unwrapSecretKey(plain)
	if err != nil {
		t.Fatalf("unwrapSecretKey: %v", err)
	}
	if got != string(plain) {
		t.Errorf("legacy passthrough mismatch: got %q want %q", got, plain)
	}
	if wasEnc {
		t.Errorf("legacy plaintext reported wasEncrypted=true")
	}
}

// TestWrapSecretKey_NilEnvelopeFallback documents the dev-mode
// fallback: without `KMAIL_SECRETS_KEY`, the provisioner stores
// the plaintext secret and emits a WARNING log line so operators
// notice. Production callers MUST wire `Envelope` to avoid this
// path; the test pins the behaviour so the warning isn't lost in
// a future refactor.
func TestWrapSecretKey_NilEnvelopeFallback(t *testing.T) {
	p, sink := newQuietProvisioner(nil)

	plaintext := "secret-without-envelope"
	blob, err := p.wrapSecretKey(plaintext)
	if err != nil {
		t.Fatalf("wrapSecretKey (nil env): %v", err)
	}
	if string(blob) != plaintext {
		t.Errorf("nil-envelope wrap altered bytes: got %q want %q", blob, plaintext)
	}
	if !strings.Contains(sink.String(), "WARNING") || !strings.Contains(sink.String(), "KMAIL_SECRETS_KEY") {
		t.Errorf("expected loud warning about KMAIL_SECRETS_KEY, got %q", sink.String())
	}

	got, wasEnc, err := p.unwrapSecretKey(blob)
	if err != nil {
		t.Fatalf("unwrapSecretKey (nil env): %v", err)
	}
	if got != plaintext || wasEnc {
		t.Errorf("nil-envelope round-trip got=%q wasEnc=%v want=%q wasEnc=false", got, wasEnc, plaintext)
	}
}

// TestStorageCredential_WasEncryptedAccessor pins the accessor so
// callers that observe migration progress (e.g. an admin
// "outstanding plaintext rows" report) don't break if the
// internal field is renamed.
func TestStorageCredential_WasEncryptedAccessor(t *testing.T) {
	c := &StorageCredential{wasEncrypted: true}
	if !c.WasEncrypted() {
		t.Error("WasEncrypted() returned false for wrapped row")
	}
	c2 := &StorageCredential{}
	if c2.WasEncrypted() {
		t.Error("WasEncrypted() returned true for legacy row")
	}
}
