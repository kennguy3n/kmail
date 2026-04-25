package cmk

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
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
