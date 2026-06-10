package cmk

import (
	"context"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestHSMLifecycleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM cmk_hsm_configs WHERE tenant_id=$1::uuid`, tenant)
	})

	// Register a KMIP config pointed at a closed local port.
	reg, err := svc.RegisterHSMKey(ctx, tenant, PrivacyPlan, HSMRegistration{
		Provider:    HSMKMIP,
		Endpoint:    "kmips://127.0.0.1:1",
		Credentials: "user:secret",
	})
	if err != nil {
		t.Fatalf("RegisterHSMKey: %v", err)
	}
	if reg.Status != "pending" {
		t.Errorf("new config status=%q want pending", reg.Status)
	}

	// ListHSMConfigs sees it.
	cfgs, err := svc.ListHSMConfigs(ctx, tenant)
	if err != nil || len(cfgs) != 1 {
		t.Fatalf("ListHSMConfigs=%d err=%v", len(cfgs), err)
	}

	// EncryptDEK exercises loadHSMConfig + the KMIP wire branch.
	// The dial to a closed port fails, so we assert the error
	// propagates (loadHSMConfig succeeded, the wire call did not).
	if _, _, err := svc.EncryptDEK(ctx, tenant, reg.ID, "key-1", []byte("dek")); err == nil {
		t.Error("EncryptDEK against closed port should error")
	}
	if _, err := svc.DecryptDEK(ctx, tenant, reg.ID, "key-1", []byte("ct"), []byte("iv")); err == nil {
		t.Error("DecryptDEK against closed port should error")
	}

	// markHSMUsed bumps last_used_at without error.
	if err := svc.markHSMUsed(ctx, tenant, reg.ID); err != nil {
		t.Errorf("markHSMUsed: %v", err)
	}

	// TestHSMConnection re-validates: kmips://127.0.0.1:1 has valid
	// shape so the stub provider marks it active.
	tested, err := svc.TestHSMConnection(ctx, tenant, reg.ID)
	if err != nil {
		t.Fatalf("TestHSMConnection: %v", err)
	}
	if tested.Status != "active" {
		t.Errorf("TestHSMConnection status=%q want active", tested.Status)
	}
}

func TestHSMPKCS11EncryptBranchDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")
	svc := NewCMKServiceWithEnvelope(pool, newTestEnvelope(t))
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM cmk_hsm_configs WHERE tenant_id=$1::uuid`, tenant)
	})

	reg, err := svc.RegisterHSMKey(ctx, tenant, PrivacyPlan, HSMRegistration{
		Provider:    HSMPKCS11,
		Endpoint:    "/usr/lib/softhsm/libsofthsm2.so",
		SlotID:      "0",
		Credentials: "1234",
	})
	if err != nil {
		t.Fatalf("RegisterHSMKey pkcs11: %v", err)
	}
	// Without the pkcs11 cgo build tag the shim returns an error;
	// this still drives the PKCS#11 branch of EncryptDEK/DecryptDEK.
	if _, _, err := svc.EncryptDEK(ctx, tenant, reg.ID, "key-1", []byte("dek")); err == nil {
		t.Error("pkcs11 EncryptDEK without cgo should error")
	}
	if _, err := svc.DecryptDEK(ctx, tenant, reg.ID, "key-1", []byte("ct"), []byte("iv")); err == nil {
		t.Error("pkcs11 DecryptDEK without cgo should error")
	}
}

func TestEncryptDEKErrorsDB(t *testing.T) {
	// Nil pool ⇒ loadHSMConfig refuses.
	svc := NewCMKServiceWithEnvelope(nil, newTestEnvelope(t))
	if _, _, err := svc.EncryptDEK(context.Background(), "t", "c", "k", []byte("x")); err == nil {
		t.Error("EncryptDEK with nil pool should error")
	}
	_, _, err := svc.loadHSMConfig(context.Background(), "t", "c")
	if err == nil || !strings.Contains(err.Error(), "pool not configured") {
		t.Errorf("loadHSMConfig nil pool err=%v", err)
	}
}
