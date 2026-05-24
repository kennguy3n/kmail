package cmk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
)

func generatePEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestRegisterKey_Validation(t *testing.T) {
	s := NewCMKService(nil)
	good := generatePEM(t)
	if _, err := s.RegisterKey(context.Background(), "", "privacy", good, ""); err == nil {
		t.Errorf("Register empty tenant expected error")
	}
	if _, err := s.RegisterKey(context.Background(), "t", "core", good, ""); !errors.Is(err, ErrPlanNotEligible) {
		t.Errorf("Register core plan = %v, want ErrPlanNotEligible", err)
	}
	if _, err := s.RegisterKey(context.Background(), "t", "privacy", "", ""); err == nil || !strings.Contains(err.Error(), "public_key_pem required") {
		t.Errorf("Register empty PEM = %v", err)
	}
	if _, err := s.RegisterKey(context.Background(), "t", "privacy", "not a pem", ""); err == nil || !strings.Contains(err.Error(), "invalid PEM") {
		t.Errorf("Register garbage PEM = %v", err)
	}
}

func TestRotateKey_Validation(t *testing.T) {
	s := NewCMKService(nil)
	good := generatePEM(t)
	if _, err := s.RotateKey(context.Background(), "t", "core", good, ""); !errors.Is(err, ErrPlanNotEligible) {
		t.Errorf("Rotate core plan = %v, want ErrPlanNotEligible", err)
	}
	if _, err := s.RotateKey(context.Background(), "", "privacy", good, ""); err == nil {
		t.Errorf("Rotate empty tenant expected error")
	}
}

func TestRevokeKey_RequiresIDs(t *testing.T) {
	s := NewCMKService(nil)
	if err := s.RevokeKey(context.Background(), "", "k"); err == nil {
		t.Errorf("Revoke empty tenantID expected error")
	}
	if err := s.RevokeKey(context.Background(), "t", ""); err == nil {
		t.Errorf("Revoke empty keyID expected error")
	}
}

func TestGetActiveKey_RequiresTenantID(t *testing.T) {
	s := NewCMKService(nil)
	if _, err := s.GetActiveKey(context.Background(), ""); err == nil {
		t.Errorf("GetActiveKey empty tenant expected error")
	}
}

func TestFingerprintPEM_RejectsCertificate(t *testing.T) {
	bad := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	if _, err := fingerprintPEM(bad); err == nil {
		t.Errorf("fingerprintPEM cert expected error")
	}
}

func TestFingerprintPEM_AcceptsRSAPublic(t *testing.T) {
	good := generatePEM(t)
	fp, err := fingerprintPEM(good)
	if err != nil {
		t.Fatalf("fingerprintPEM = %v", err)
	}
	if len(fp) != 64 {
		t.Errorf("fingerprint length = %d, want 64", len(fp))
	}
}

func TestNilPoolGuards(t *testing.T) {
	s := NewCMKService(nil)
	good := generatePEM(t)
	if _, err := s.RegisterKey(context.Background(), "t", "privacy", good, ""); err == nil {
		t.Errorf("Register nil-pool expected error")
	}
	if _, err := s.RotateKey(context.Background(), "t", "privacy", good, ""); err == nil {
		t.Errorf("Rotate nil-pool expected error")
	}
	if err := s.RevokeKey(context.Background(), "t", "k"); err == nil {
		t.Errorf("Revoke nil-pool expected error")
	}
}

// TestSetEnvelope_ConcurrentSafe verifies the RWMutex around the
// envelope field actually protects concurrent SetEnvelope vs.
// Envelope() reads. The race detector flags an unsynchronised
// interface write against a concurrent read, so this test
// regresses to the pre-RWMutex version of CMKService if the lock
// is ever removed.
func TestSetEnvelope_ConcurrentSafe(t *testing.T) {
	// 64 hex chars = 32-byte master key, the format accepted by
	// the envelope constructor.
	envA, err := NewAESGCMEnvelopeFromKeyMaterial(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	envB, err := NewAESGCMEnvelopeFromKeyMaterial(strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewCMKService(nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			if i%2 == 0 {
				svc.SetEnvelope(envA)
			} else {
				svc.SetEnvelope(envB)
			}
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = svc.Envelope()
	}
	<-done
	// Final state is one of the two envelopes; nil-or-typed.
	if svc.Envelope() == nil {
		t.Fatalf("Envelope() returned nil after concurrent stores; expected envA or envB")
	}
}

// TestSetLogger_ConcurrentSafe pins the round-10 fix that moved
// `s.logger` reads/writes under `envMu`. Before the fix, a
// concurrent `SetLogger` racing with `warnLegacyPlaintextHSM`
// would be flagged by `-race`; with the fix, the pointer swap is
// serialised so `getLogger()` always returns a fully published
// `*log.Logger` value.
func TestSetLogger_ConcurrentSafe(t *testing.T) {
	svc := NewCMKService(nil)

	// Pre-mark the (tenant, config) pair as seen so
	// warnLegacyPlaintextHSM races on logger access for every
	// iteration instead of short-circuiting on the sync.Map
	// LoadOrStore. This keeps the race window wide enough that
	// `-race` will reliably catch a regression if SetLogger
	// goes back to unsynchronised writes.
	svc.legacyPlaintextSeen.Store("tenant-a/cfg-1", struct{}{})

	var buf1, buf2 bytes.Buffer
	loggerA := log.New(&buf1, "A:", 0)
	loggerB := log.New(&buf2, "B:", 0)

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer goroutine: thrash SetLogger.
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			if i%2 == 0 {
				svc.SetLogger(loggerA)
			} else {
				svc.SetLogger(loggerB)
			}
		}
	}()

	// Reader goroutine: thrash warnLegacyPlaintextHSM
	// (short-circuits on the sync.Map but still goes through
	// getLogger() on the unrelated branch we exercise below).
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			// getLogger directly so the race window is the
			// pointer load, not the LoadOrStore short-circuit
			// in warnLegacyPlaintextHSM.
			_ = svc.getLogger()
		}
	}()

	wg.Wait()

	// Final invariant: getLogger returns one of the two
	// loggers (or log.Default if SetLogger somehow regressed to
	// store nil — would be a bug, but the helper falls back so
	// the test still works).
	if svc.getLogger() == nil {
		t.Fatal("getLogger() returned nil after concurrent SetLogger storms")
	}
}

// TestSetLogger_NilFallsBackToDiscard verifies the documented
// nil-input behaviour of SetLogger: passing nil is interpreted
// as "silence this service" and routes to io.Discard rather
// than panicking on the next log call.
func TestSetLogger_NilFallsBackToDiscard(t *testing.T) {
	svc := NewCMKService(nil)
	svc.SetLogger(nil)
	got := svc.getLogger()
	if got == nil {
		t.Fatal("SetLogger(nil) -> getLogger() returned nil, expected io.Discard logger")
	}
	// Print to it — must not panic.
	got.Printf("ignored")
}
