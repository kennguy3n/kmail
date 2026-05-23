package cmk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	if got, err := s2.unwrapHSMCredentials([]byte("plaintext")); err != nil || !bytes.Equal(got, []byte("plaintext")) {
		t.Fatalf("nil-envelope unwrap should pass-through, got %q err=%v", got, err)
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

	got, err := s.unwrapHSMCredentials(wrapped)
	if err != nil {
		t.Fatalf("unwrapHSMCredentials: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("unwrap mismatch: got %q want %q", got, plain)
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
	got, err := s.unwrapHSMCredentials(plain)
	if err != nil {
		t.Fatalf("unwrapHSMCredentials: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("legacy passthrough mismatch: got %q want %q", got, plain)
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
