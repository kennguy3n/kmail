package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestWebAuthnStoreDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewWebAuthnStore(pool)
	ctx := context.Background()
	user := "user-" + time.Now().Format("150405.000000")

	cred := WebAuthnCredential{
		TenantID:     tenant,
		UserID:       user,
		CredentialID: "cred-abc",
		PublicKey:    "cose-bytes",
		Name:         "YubiKey",
		CreatedAt:    time.Now(),
	}
	if err := store.Insert(ctx, cred); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	list, err := store.ListByUser(ctx, tenant, user)
	if err != nil || len(list) != 1 || list[0].CredentialID != "cred-abc" {
		t.Fatalf("ListByUser=%+v err=%v", list, err)
	}
	id := list[0].ID

	got, err := store.Get(ctx, tenant, "cred-abc")
	if err != nil || got.Name != "YubiKey" || got.SignCount != 0 {
		t.Fatalf("Get=%+v err=%v", got, err)
	}

	// BumpSignCount increments and stamps last_used_at.
	if err := store.BumpSignCount(ctx, tenant, "cred-abc", time.Now()); err != nil {
		t.Fatalf("BumpSignCount: %v", err)
	}
	got, _ = store.Get(ctx, tenant, "cred-abc")
	if got.SignCount != 1 || got.LastUsedAt == nil {
		t.Errorf("after bump: count=%d lastUsed=%v", got.SignCount, got.LastUsedAt)
	}

	// SetSignCount only advances when strictly greater.
	if err := store.SetSignCount(ctx, tenant, "cred-abc", 5, time.Now()); err != nil {
		t.Fatalf("SetSignCount up: %v", err)
	}
	got, _ = store.Get(ctx, tenant, "cred-abc")
	if got.SignCount != 5 {
		t.Errorf("SetSignCount up: got %d want 5", got.SignCount)
	}
	// A lower value is ignored (clone defence).
	if err := store.SetSignCount(ctx, tenant, "cred-abc", 3, time.Now()); err != nil {
		t.Fatalf("SetSignCount down: %v", err)
	}
	got, _ = store.Get(ctx, tenant, "cred-abc")
	if got.SignCount != 5 {
		t.Errorf("SetSignCount down should be ignored: got %d want 5", got.SignCount)
	}

	// Delete of a non-matching id reports not found.
	if err := store.Delete(ctx, tenant, user, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Error("Delete bogus id: expected error")
	}
	if err := store.Delete(ctx, tenant, user, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if l, _ := store.ListByUser(ctx, tenant, user); len(l) != 0 {
		t.Errorf("after delete: still %d creds", len(l))
	}
}

func TestTOTPStoreDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewTOTPStore(pool)
	ctx := context.Background()
	user := "user-" + time.Now().Format("150405.000000")

	if _, err := store.Get(ctx, tenant, user); !errors.Is(err, ErrTOTPNotFound) {
		t.Fatalf("Get missing: want ErrTOTPNotFound got %v", err)
	}

	now := time.Now()
	if err := store.Upsert(ctx, tenant, user, []byte("secret-bytes"), "rhash", false, now); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c, err := store.Get(ctx, tenant, user)
	if err != nil || c.Enabled || string(c.EncryptedSecret) != "secret-bytes" || c.RecoveryCodesHash != "rhash" {
		t.Fatalf("Get=%+v err=%v", c, err)
	}

	// Upsert again enabling the credential.
	if err := store.Upsert(ctx, tenant, user, []byte("secret2"), "rhash2", true, now); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	c, _ = store.Get(ctx, tenant, user)
	if !c.Enabled || string(c.EncryptedSecret) != "secret2" {
		t.Errorf("after enable: %+v", c)
	}

	// Recovery-hash rotation and last_used_at stamping now happen
	// atomically inside EvaluateAttempt's success path; the standalone
	// UpdateRecoveryCodes / MarkUsed methods were folded into it to
	// close the lockout TOCTOU + recovery double-spend window.
	rhash3 := "rhash3"
	res, err := store.EvaluateAttempt(ctx, tenant, user, time.Now(), 5, 15*time.Minute, false,
		func(*TOTPCredential) TOTPVerification {
			return TOTPVerification{OK: true, Method: "totp", SetRecoveryHash: &rhash3}
		})
	if err != nil || !res.Verified {
		t.Fatalf("EvaluateAttempt: verified=%v err=%v", res.Verified, err)
	}
	c, _ = store.Get(ctx, tenant, user)
	if c.RecoveryCodesHash != "rhash3" || c.LastUsedAt == nil {
		t.Errorf("after recovery+used: %+v", c)
	}

	if err := store.Delete(ctx, tenant, user); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, tenant, user); !errors.Is(err, ErrTOTPNotFound) {
		t.Errorf("after delete: want ErrTOTPNotFound got %v", err)
	}
}

func TestMemoryChallenger(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryChallenger()

	if _, err := m.LoadChallenge(ctx, "missing"); err == nil {
		t.Error("LoadChallenge missing: expected error")
	}
	if err := m.StoreChallenge(ctx, "k1", []byte("chal"), time.Minute); err != nil {
		t.Fatalf("StoreChallenge: %v", err)
	}
	got, err := m.LoadChallenge(ctx, "k1")
	if err != nil || string(got) != "chal" {
		t.Fatalf("LoadChallenge=%q err=%v", got, err)
	}
	if err := m.DeleteChallenge(ctx, "k1"); err != nil {
		t.Fatalf("DeleteChallenge: %v", err)
	}
	if _, err := m.LoadChallenge(ctx, "k1"); err == nil {
		t.Error("LoadChallenge after delete: expected error")
	}

	// Expired challenge is not returned.
	if err := m.StoreChallenge(ctx, "k2", []byte("x"), -time.Second); err != nil {
		t.Fatalf("StoreChallenge expired: %v", err)
	}
	if _, err := m.LoadChallenge(ctx, "k2"); err == nil {
		t.Error("LoadChallenge expired: expected error")
	}
}
