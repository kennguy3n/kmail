package cmk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// nonNilPoolSentinel returns a zero-valued *pgxpool.Pool used to
// exercise guard paths that fire before any DB call. The pointer
// must never be dereferenced; tests use this when they want the
// `pool == nil` guard to fall through but cannot stand up real
// Postgres.
func nonNilPoolSentinel() *pgxpool.Pool {
	return new(pgxpool.Pool)
}

func newTestEnvelope(t *testing.T) SecretsEnvelope {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("read random key: %v", err)
	}
	env, err := NewAESGCMEnvelopeFromKeyMaterial(hex.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewAESGCMEnvelopeFromKeyMaterial: %v", err)
	}
	return env
}

func TestRegisterHSMKey_RefusesWithoutEnvelope(t *testing.T) {
	// NewCMKService leaves the envelope nil. The HSM-registration
	// path is supposed to refuse any plaintext write, but the
	// nil-pool guard fires first when there's no DB. To exercise
	// the envelope guard cleanly we install a sentinel non-nil
	// pool pointer; the envelope check runs before any DB call,
	// so the sentinel never actually gets dereferenced.
	s := &CMKService{pool: nonNilPoolSentinel()}
	_, err := s.RegisterHSMKey(context.Background(), "tenant", PrivacyPlan, HSMRegistration{
		Provider:    HSMKMIP,
		Endpoint:    "kmip://example:5696",
		Credentials: "user:pass",
	})
	if !errors.Is(err, ErrEnvelopeNotConfigured) {
		t.Fatalf("expected ErrEnvelopeNotConfigured, got %v", err)
	}

	// Defence in depth: the read-path helper must still tolerate
	// nil envelopes (legacy rows that were written before the
	// envelope landed should remain readable rather than blowing
	// up the entire service).
	s2 := &CMKService{}
	got, wasEnc, err := s2.unwrapHSMCredentials([]byte("plaintext"))
	if err != nil || !bytes.Equal(got, []byte("plaintext")) {
		t.Fatalf("nil-envelope unwrap should pass-through, got %q err=%v", got, err)
	}
	if wasEnc {
		t.Errorf("nil-envelope unwrap reported wasEncrypted=true")
	}
}

func TestUnwrapHSMCredentials_RoundtripWithEnvelope(t *testing.T) {
	env := newTestEnvelope(t)
	s := &CMKService{envelope: env}

	plain := []byte("kmip-user:kmip-pass\n")
	wrapped, err := env.Wrap(plain)
	if err != nil {
		t.Fatalf("env.Wrap: %v", err)
	}
	if bytes.Equal(wrapped, plain) {
		t.Fatal("wrap produced plaintext output")
	}

	got, wasEnc, err := s.unwrapHSMCredentials(wrapped)
	if err != nil {
		t.Fatalf("unwrapHSMCredentials: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("unwrap mismatch: got %q want %q", got, plain)
	}
	if !wasEnc {
		t.Errorf("expected wasEncrypted=true for envelope-wrapped blob")
	}
}

func TestUnwrapHSMCredentials_LegacyPlaintextPassthrough(t *testing.T) {
	// Rows written before the envelope landed contain raw
	// plaintext bytes. The unwrap path must return them verbatim
	// rather than throwing — that's how the migration window
	// works.
	env := newTestEnvelope(t)
	s := &CMKService{envelope: env}

	plain := []byte("legacy-plaintext-cred")
	got, wasEnc, err := s.unwrapHSMCredentials(plain)
	if err != nil {
		t.Fatalf("unwrapHSMCredentials: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("legacy passthrough mismatch: got %q want %q", got, plain)
	}
	if wasEnc {
		t.Errorf("expected wasEncrypted=false for legacy plaintext blob")
	}
}

// TestUnwrapHSMCredentials_CorruptionDetected pins the round-7
// finding: a wrapped blob whose AEAD authentication fails must
// surface ErrEnvelopeCorrupted instead of silently being returned
// as if it were a legacy plaintext credential. The previous
// silent-fallback behavior would have surfaced ciphertext bytes
// from a key-rotation epoch as plaintext to the HSM provider.
func TestUnwrapHSMCredentials_CorruptionDetected(t *testing.T) {
	env := newTestEnvelope(t)
	s := &CMKService{envelope: env}

	wrapped, err := env.Wrap([]byte("kmip-user:pw"))
	if err != nil {
		t.Fatalf("env.Wrap: %v", err)
	}
	// Tamper with the ciphertext body (after the 16-byte magic
	// header). Flipping a single byte invalidates the GCM tag.
	if len(wrapped) <= 16 {
		t.Fatalf("wrapped blob too short for tamper: %d bytes", len(wrapped))
	}
	tampered := append([]byte(nil), wrapped...)
	tampered[len(tampered)-1] ^= 0xFF

	_, _, err = s.unwrapHSMCredentials(tampered)
	if err == nil {
		t.Fatal("expected error from tampered wrapped blob; got nil (would surface ciphertext as plaintext)")
	}
	if !errors.Is(err, ErrEnvelopeCorrupted) {
		t.Errorf("expected ErrEnvelopeCorrupted, got %v", err)
	}
}

// TestUnwrapHSMCredentials_KeyRotationSurfacesAsCorruption pins
// the specific scenario the bot called out: a master-key rotation
// leaves the previous epoch's rows un-decryptable. The new key
// MUST surface those rows as ErrEnvelopeCorrupted, never as
// "legacy plaintext", or operations would silently send the OLD
// epoch's ciphertext to the HSM provider as if it were a
// plaintext credential.
func TestUnwrapHSMCredentials_KeyRotationSurfacesAsCorruption(t *testing.T) {
	keyA := make([]byte, 32)
	if _, err := rand.Read(keyA); err != nil {
		t.Fatalf("read keyA: %v", err)
	}
	envA, err := NewAESGCMEnvelopeFromKeyMaterial(hex.EncodeToString(keyA))
	if err != nil {
		t.Fatalf("envA: %v", err)
	}

	keyB := make([]byte, 32)
	if _, err := rand.Read(keyB); err != nil {
		t.Fatalf("read keyB: %v", err)
	}
	envB, err := NewAESGCMEnvelopeFromKeyMaterial(hex.EncodeToString(keyB))
	if err != nil {
		t.Fatalf("envB: %v", err)
	}

	wrappedWithA, err := envA.Wrap([]byte("rotated-epoch-cred"))
	if err != nil {
		t.Fatalf("envA.Wrap: %v", err)
	}
	s := &CMKService{envelope: envB} // simulating post-rotation
	_, _, err = s.unwrapHSMCredentials(wrappedWithA)
	if err == nil {
		t.Fatal("post-rotation envelope must NOT silently return prior epoch's ciphertext as plaintext")
	}
	if !errors.Is(err, ErrEnvelopeCorrupted) {
		t.Errorf("expected ErrEnvelopeCorrupted on key-rotation mismatch, got %v", err)
	}
}

// TestUnwrapHSMCredentials_LegacyMagicAbsentReturnsPassthrough
// guards the other side of the magic-prefix contract: a blob that
// does NOT start with the magic prefix is treated as legacy
// plaintext (the row was written before the envelope landed) and
// returned verbatim with wasEncrypted=false. The migration warning
// is the operator's signal to re-register such rows.
func TestUnwrapHSMCredentials_LegacyMagicAbsentReturnsPassthrough(t *testing.T) {
	env := newTestEnvelope(t)
	s := &CMKService{envelope: env}

	// Legacy plaintext: indistinguishable from a random short blob
	// to the runtime, but we know it doesn't start with the
	// kmail-cmk-v1 magic.
	legacy := []byte("plaintext-creds-from-pre-envelope-era")
	got, wasEnc, err := s.unwrapHSMCredentials(legacy)
	if err != nil {
		t.Fatalf("unwrapHSMCredentials: %v", err)
	}
	if !bytes.Equal(got, legacy) {
		t.Errorf("legacy passthrough mismatch: %q != %q", got, legacy)
	}
	if wasEnc {
		t.Error("expected wasEncrypted=false for magic-prefix-absent blob")
	}
}

// TestUnwrapHSMCredentials_OldFormatBlobReadable pins the round-8
// data-continuity fix: blobs encrypted by an EARLIER revision of
// this envelope used the old wire format (no magic prefix, just
// nonce(12) || ciphertext_with_tag). The new Unwrap must continue
// to decrypt those blobs so deployments that wrote encrypted DKIM
// keys or TOTP secrets before the magic prefix was introduced
// don't lose access to that data after upgrading.
//
// The test bypasses Wrap and constructs an old-format blob by
// driving the underlying AEAD directly, then asserts that Unwrap
// returns the original plaintext with wasEncrypted=true. Without
// the data-continuity path in Unwrap, the magic-absent guard
// would short-circuit and return the raw ciphertext bytes as
// "legacy plaintext" — which is exactly the silent corruption the
// round-8 review caught.
func TestUnwrapHSMCredentials_OldFormatBlobReadable(t *testing.T) {
	env := newTestEnvelope(t).(*AESGCMEnvelope)
	s := &CMKService{envelope: env}

	plaintext := []byte("kmip-user:pre-magic-secret")

	// Build an old-format blob: nonce(12) || ciphertext_with_tag,
	// no magic prefix. This is byte-for-byte what the pre-round-7
	// Wrap implementation produced.
	nonce := make([]byte, env.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("read nonce: %v", err)
	}
	oldBlob := env.aead.Seal(nonce, nonce, plaintext, nil)

	got, wasEnc, err := s.unwrapHSMCredentials(oldBlob)
	if err != nil {
		t.Fatalf("unwrapHSMCredentials on old-format blob: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("old-format plaintext mismatch: %q != %q", got, plaintext)
	}
	if !wasEnc {
		t.Error("expected wasEncrypted=true for old-format ciphertext")
	}
}

// TestUnwrapHSMCredentials_OldFormatWithWrongKeyTreatedAsPlaintext
// documents the bounded loss of key-rotation safety on the
// old-format path: when a magic-absent blob fails AEAD auth, we
// cannot distinguish "wrong key on old-format ciphertext" from
// "genuine legacy plaintext" — both look like a decrypt failure.
// The defensible choice is to treat the blob as legacy plaintext
// (return verbatim with wasEncrypted=false) so deployments that
// never wrote old-format ciphertext continue to migrate cleanly.
// Once every old-format row has been re-wrapped via Wrap, every
// read goes through the magic-prefix path and the key-rotation
// guarantee becomes exact again.
func TestUnwrapHSMCredentials_OldFormatWithWrongKeyTreatedAsPlaintext(t *testing.T) {
	envA := newTestEnvelope(t).(*AESGCMEnvelope)
	envB := newTestEnvelope(t).(*AESGCMEnvelope) // different key

	plaintext := []byte("kmip-user:pre-magic-secret")
	nonce := make([]byte, envA.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("read nonce: %v", err)
	}
	oldBlobUnderA := envA.aead.Seal(nonce, nonce, plaintext, nil)

	s := &CMKService{envelope: envB}
	got, wasEnc, err := s.unwrapHSMCredentials(oldBlobUnderA)
	if err != nil {
		t.Fatalf("unwrap with wrong key: %v", err)
	}
	if wasEnc {
		t.Error("expected wasEncrypted=false for old-format blob under wrong key")
	}
	if !bytes.Equal(got, oldBlobUnderA) {
		t.Error("expected verbatim passthrough for indistinguishable plaintext/ciphertext")
	}
}

func TestWarnLegacyPlaintextHSM_DeduplicatesPerConfig(t *testing.T) {
	var buf bytes.Buffer
	s := &CMKService{envelope: newTestEnvelope(t)}
	s.SetLogger(log.New(&buf, "", 0))

	s.warnLegacyPlaintextHSM("tenant-a", "cfg-1")
	s.warnLegacyPlaintextHSM("tenant-a", "cfg-1") // duplicate
	s.warnLegacyPlaintextHSM("tenant-a", "cfg-2") // different config
	s.warnLegacyPlaintextHSM("tenant-b", "cfg-1") // different tenant

	out := buf.String()
	if n := strings.Count(out, "legacy-plaintext HSM credentials"); n != 3 {
		t.Errorf("got %d warnings, want 3 (one per unique tenant/config): %s", n, out)
	}
	if !strings.Contains(out, "tenant=tenant-a config=cfg-1") {
		t.Errorf("missing tenant-a/cfg-1 warning: %s", out)
	}
	if !strings.Contains(out, "tenant=tenant-a config=cfg-2") {
		t.Errorf("missing tenant-a/cfg-2 warning: %s", out)
	}
	if !strings.Contains(out, "tenant=tenant-b config=cfg-1") {
		t.Errorf("missing tenant-b/cfg-1 warning: %s", out)
	}
}

func TestRegisterHSMKey_RefusesNonPrivacyPlan(t *testing.T) {
	s := NewCMKServiceWithEnvelope(nil, newTestEnvelope(t))
	_, err := s.RegisterHSMKey(context.Background(), "tenant", "core", HSMRegistration{
		Provider:    HSMKMIP,
		Endpoint:    "kmip://example:5696",
		Credentials: "user:pass",
	})
	if !errors.Is(err, ErrPlanNotEligible) {
		t.Fatalf("expected ErrPlanNotEligible, got %v", err)
	}
}

func TestNewCMKServiceWithEnvelope_StoresEnvelope(t *testing.T) {
	env := newTestEnvelope(t)
	s := NewCMKServiceWithEnvelope(nil, env)
	if s.Envelope() == nil {
		t.Fatal("envelope was not stored")
	}
}

func TestSetEnvelope_ReplacesEnvelope(t *testing.T) {
	s := NewCMKService(nil)
	if s.Envelope() != nil {
		t.Fatal("default service should have nil envelope")
	}
	env := newTestEnvelope(t)
	s.SetEnvelope(env)
	if s.Envelope() == nil {
		t.Fatal("SetEnvelope did not install envelope")
	}
}
