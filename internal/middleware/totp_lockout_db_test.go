package middleware

// Per-account TOTP brute-force lockout integration test (Session 6
// / SOC 2 auth-hardening step). Skips unless KMAIL_TEST_DATABASE_URL
// or DATABASE_URL points at a migrated database.
//
// Verifies that /api/v1/auth/totp/check:
//   - returns 401 for each wrong code until the ceiling is reached,
//   - returns 429 (with Retry-After) once locked, refusing even a
//     correct code while the lock stands,
//   - clears the lock and the failure counter on the first
//     successful verification after the cooldown elapses.

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTOTPLockout_EnforcedPerAccount(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-lock")

	// Controllable clock shared by the handler and the test.
	var (
		mu  sync.Mutex
		now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	const maxAttempts = 3
	const lockFor = 10 * time.Minute
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               clock,
		MaxFailedAttempts: maxAttempts,
		LockoutDuration:   lockFor,
	})

	// Enroll an enabled credential with a known secret (nil envelope
	// stores raw bytes, which unwrapSecret reads back verbatim).
	secret := []byte("12345678901234567890") // RFC 4226 test vector key
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "", true, clock()); err != nil {
		t.Fatalf("seed totp credential: %v", err)
	}

	validCode := func() string {
		return generateHOTP(secret, clock().Unix()/30)
	}
	wrongCode := func() string {
		w := "654321"
		if w == validCode() {
			w = "123456"
		}
		return w
	}

	doCheck := func(code string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/check",
			bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
		req.Header.Set("X-KMail-Dev-User-Id", userID)
		rec := httptest.NewRecorder()
		h.check(rec, req)
		return rec
	}

	// maxAttempts wrong codes: each rejected with 401, no lock yet on
	// the first (maxAttempts-1).
	for i := 0; i < maxAttempts; i++ {
		rec := doCheck(wrongCode())
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, rec.Code)
		}
	}

	// Account is now locked: even the correct code is refused with 429
	// and a Retry-After header.
	rec := doCheck(validCode())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after ceiling: want 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("locked response missing Retry-After header")
	}

	// Within the cooldown the lock still stands.
	advance(lockFor / 2)
	if rec := doCheck(validCode()); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("mid-cooldown: want 429, got %d", rec.Code)
	}

	// After the cooldown, a correct code succeeds and clears state.
	advance(lockFor)
	if rec := doCheck(validCode()); rec.Code != http.StatusOK {
		t.Fatalf("post-cooldown valid code: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}

	// Counter was reset: a single wrong code is a 401 (not an
	// immediate re-lock to 429).
	if rec := doCheck(wrongCode()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after reset: want 401, got %d", rec.Code)
	}
}

// TestTOTPLockout_ReenrollmentClearsLock guards the fix for the
// enrollment path leaking lockout state: Upsert (used by enroll and
// by verify on enrollment confirmation) must reset failed_attempts
// and locked_until, so a freshly (re)enrolled credential never
// carries a stale lock into the login (check) phase.
func TestTOTPLockout_ReenrollmentClearsLock(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-reenroll")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	const maxAttempts = 3
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               clock,
		MaxFailedAttempts: maxAttempts,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "", true, clock()); err != nil {
		t.Fatalf("seed totp credential: %v", err)
	}

	doCheck := func(code string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/check",
			bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
		req.Header.Set("X-KMail-Dev-User-Id", userID)
		rec := httptest.NewRecorder()
		h.check(rec, req)
		return rec
	}

	valid := generateHOTP(secret, clock().Unix()/30)
	wrong := "654321"
	if wrong == valid {
		wrong = "123456"
	}

	// Drive the account into a locked state.
	for i := 0; i < maxAttempts; i++ {
		doCheck(wrong)
	}
	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after lock: %v", err)
	}
	if cred.LockedUntil == nil {
		t.Fatalf("precondition: account should be locked after %d failures", maxAttempts)
	}

	// Re-enroll over the locked row (mirrors the verify success path).
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "newrecoveryhash", true, clock()); err != nil {
		t.Fatalf("re-enroll Upsert: %v", err)
	}

	cred, err = h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after re-enroll: %v", err)
	}
	if cred.FailedAttempts != 0 || cred.LockedUntil != nil {
		t.Fatalf("re-enrollment must clear lockout: failed_attempts=%d locked_until=%v",
			cred.FailedAttempts, cred.LockedUntil)
	}

	// And the lock no longer bites: a correct code succeeds immediately.
	if rec := doCheck(valid); rec.Code != http.StatusOK {
		t.Fatalf("after re-enroll valid code: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestTOTPLockout_ConcurrentBurstRespectsCeiling guards the fix for
// the check-then-act (TOCTOU) race: a burst of simultaneous wrong
// guesses must not be evaluated past the lockout ceiling. Because
// EvaluateAttempt locks the credential row FOR UPDATE, attempts are
// fully serialized — exactly maxAttempts guesses are evaluated (401)
// and every later request in the burst is refused with 429 without
// its guess being checked. Before the fix, all N requests read the
// pre-attempt state, sailed past the in-memory lock check, and were
// all evaluated as guesses.
func TestTOTPLockout_ConcurrentBurstRespectsCeiling(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-burst")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	const maxAttempts = 3
	const burst = 12
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               clock,
		MaxFailedAttempts: maxAttempts,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "", true, clock()); err != nil {
		t.Fatalf("seed totp credential: %v", err)
	}

	valid := generateHOTP(secret, clock().Unix()/30)
	wrong := "654321"
	if wrong == valid {
		wrong = "123456"
	}

	doCheck := func(code string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/check",
			bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
		req.Header.Set("X-KMail-Dev-User-Id", userID)
		rec := httptest.NewRecorder()
		h.check(rec, req)
		return rec.Code
	}

	var unauthorized, locked, other int64
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch doCheck(wrong) {
			case http.StatusUnauthorized:
				atomic.AddInt64(&unauthorized, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&locked, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}()
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("unexpected non-401/429 responses: %d", other)
	}
	// No more than the ceiling of guesses may ever be evaluated, even
	// under a concurrent burst. Serialization makes this exact.
	if unauthorized != maxAttempts {
		t.Fatalf("guesses evaluated = %d, want exactly %d (ceiling); burst leaked past the lock",
			unauthorized, maxAttempts)
	}
	if locked != burst-maxAttempts {
		t.Fatalf("locked responses = %d, want %d", locked, burst-maxAttempts)
	}

	// The row is locked and the counter was reset to 0 at lock time.
	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after burst: %v", err)
	}
	if cred.LockedUntil == nil {
		t.Fatal("account should be locked after the burst")
	}
}

// TestTOTPLockout_RecoveryCodeConsumeAtomic guards the fix for the
// non-atomic recovery-code consume + lockout-clear: two concurrent
// /check requests presenting the SAME recovery code must not both
// succeed (a double-spend). EvaluateAttempt consumes the code and
// persists the post-consumption bundle inside the same row-locked
// transaction, so the second request sees the already-consumed
// bundle and is rejected.
func TestTOTPLockout_RecoveryCodeConsumeAtomic(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-recov")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// A ceiling well above the racer count guarantees the burst's
	// losing attempts can never themselves trip the lockout, so this
	// test isolates the double-spend property cleanly.
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               clock,
		MaxFailedAttempts: 50,
		LockoutDuration:   15 * time.Minute,
	})

	// Mint a real recovery bundle and seed it on an enabled credential.
	codes, hashed, err := newRecoveryCodes(5)
	if err != nil {
		t.Fatalf("newRecoveryCodes: %v", err)
	}
	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, hashed, true, clock()); err != nil {
		t.Fatalf("seed totp credential: %v", err)
	}

	doCheck := func(code string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/check",
			bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
		req.Header.Set("X-KMail-Dev-User-Id", userID)
		rec := httptest.NewRecorder()
		h.check(rec, req)
		return rec.Code
	}

	const racers = 8
	var success, rejected int64
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch doCheck(codes[0]) {
			case http.StatusOK:
				atomic.AddInt64(&success, 1)
			default:
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("recovery code accepted %d times, want exactly 1 (double-spend)", success)
	}
	if rejected != racers-1 {
		t.Fatalf("rejected = %d, want %d", rejected, racers-1)
	}

	// The consumed code is gone from the stored bundle; reusing it fails.
	if rec := doCheck(codes[0]); rec != http.StatusUnauthorized {
		t.Fatalf("reused recovery code: want 401, got %d", rec)
	}
	// A different, still-valid recovery code is accepted.
	if rec := doCheck(codes[1]); rec != http.StatusOK {
		t.Fatalf("second recovery code: want 200, got %d", rec)
	}
}

// TestTOTPVerify_AlreadyEnabledRefusesRegeneration guards the fix for
// /verify (enrollment confirmation) doubling as an implicit
// recovery-code regenerator. /verify is only meant to flip a freshly
// enrolled (disabled) credential live; calling it again with a valid
// TOTP code on an already-enabled credential must NOT mint and persist
// a fresh recovery bundle (which would silently invalidate the codes
// the user already saved). It returns 409 and leaves the stored
// recovery hash untouched. Re-running enroll is the supported way to
// rotate.
func TestTOTPVerify_AlreadyEnabledRefusesRegeneration(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-reverify")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               clock,
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	// Seed an ALREADY-ENABLED credential with a known recovery hash.
	const originalHash = "original-recovery-bundle-hash"
	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, originalHash, true, clock()); err != nil {
		t.Fatalf("seed totp credential: %v", err)
	}

	validCode := generateHOTP(secret, clock().Unix()/30)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/verify",
		bytes.NewReader([]byte(`{"code":"`+validCode+`"}`)))
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
	req.Header.Set("X-KMail-Dev-User-Id", userID)
	rec := httptest.NewRecorder()
	h.verify(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("re-verify on enabled credential: want 409, got %d (body=%q)", rec.Code, rec.Body.String())
	}

	// The stored recovery bundle must be byte-for-byte unchanged: the
	// guard aborted before the success UPDATE could overwrite it.
	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get credential: %v", err)
	}
	if cred.RecoveryCodesHash != originalHash {
		t.Fatalf("recovery hash was overwritten: got %q, want %q", cred.RecoveryCodesHash, originalHash)
	}
	// No attempt was spent either (failure counter untouched).
	if cred.FailedAttempts != 0 {
		t.Fatalf("failed_attempts = %d, want 0 (guard must not spend an attempt)", cred.FailedAttempts)
	}
}

// doEnroll posts to /api/v1/auth/totp/enroll for (tenantID, userID).
// A blank code sends an empty body (first-time / unconfirmed
// enrollment); a non-blank code sends {"code":...} to authorize a
// re-enrollment of an already-enabled credential.
func doEnroll(h *TOTPHandlers, tenantID, userID, code string) *httptest.ResponseRecorder {
	var body []byte
	if code != "" {
		body = []byte(`{"code":"` + code + `"}`)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/enroll", bytes.NewReader(body))
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
	req.Header.Set("X-KMail-Dev-User-Id", userID)
	rec := httptest.NewRecorder()
	h.enroll(rec, req)
	return rec
}

// TestTOTPEnroll_FirstEnrollmentFrictionless confirms a first-time
// enrollment (no existing credential row) needs no second factor and
// lands a fresh, not-yet-confirmed (disabled) credential.
func TestTOTPEnroll_FirstEnrollmentFrictionless(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-enroll-first")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{Pool: admin, Now: func() time.Time { return now }})

	if rec := doEnroll(h, tenantID, userID, ""); rec.Code != http.StatusOK {
		t.Fatalf("first enroll: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after first enroll: %v", err)
	}
	if cred.Enabled {
		t.Fatal("first enrollment must land disabled (awaiting /verify)")
	}
	if len(cred.EncryptedSecret) == 0 {
		t.Fatal("first enrollment must store a secret")
	}
}

// TestTOTPEnroll_ReenrollEnabledRequiresFactor is the core
// lockout-bypass hardening: re-enrolling an ALREADY-ENABLED credential
// without proving the current second factor is refused (401), spends a
// failed attempt, and leaves the live secret untouched. Otherwise a
// caller holding only the first factor could mint a new secret and
// sidestep the standing TOTP credential.
func TestTOTPEnroll_ReenrollEnabledRequiresFactor(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-enroll-noauth")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "recov-hash", true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	// No code → refused.
	if rec := doEnroll(h, tenantID, userID, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("re-enroll without factor: want 401, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	// Wrong code → also refused.
	wrong := "654321"
	if wrong == generateHOTP(secret, now.Unix()/30) {
		wrong = "123456"
	}
	if rec := doEnroll(h, tenantID, userID, wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("re-enroll with wrong code: want 401, got %d", rec.Code)
	}

	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after refused re-enroll: %v", err)
	}
	if !cred.Enabled {
		t.Fatal("refused re-enroll must leave the credential enabled")
	}
	if !bytes.Equal(cred.EncryptedSecret, secret) {
		t.Fatal("refused re-enroll must not rotate the live secret")
	}
	if cred.RecoveryCodesHash != "recov-hash" {
		t.Fatalf("refused re-enroll must not touch recovery bundle: got %q", cred.RecoveryCodesHash)
	}
	if cred.FailedAttempts != 2 {
		t.Fatalf("two refused re-enroll attempts must each spend one: failed_attempts=%d, want 2", cred.FailedAttempts)
	}
}

// TestTOTPEnroll_ReenrollWithValidTOTP confirms the happy path: a live
// TOTP code authorizes secret rotation. The new secret is written
// atomically and the credential drops back to disabled (the user
// re-confirms via /verify).
func TestTOTPEnroll_ReenrollWithValidTOTP(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-enroll-totp")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "old-recov", true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	valid := generateHOTP(secret, now.Unix()/30)
	if rec := doEnroll(h, tenantID, userID, valid); rec.Code != http.StatusOK {
		t.Fatalf("re-enroll with valid TOTP: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}

	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after rotation: %v", err)
	}
	if cred.Enabled {
		t.Fatal("rotated credential must be disabled pending /verify")
	}
	if bytes.Equal(cred.EncryptedSecret, secret) {
		t.Fatal("re-enroll must rotate the stored secret")
	}
	if cred.RecoveryCodesHash != "" {
		t.Fatalf("re-enroll must clear the old recovery bundle: got %q", cred.RecoveryCodesHash)
	}
}

// TestTOTPEnroll_ReenrollWithRecoveryCode confirms the escape hatch for
// a lost authenticator: an unused recovery code authorizes rotation.
func TestTOTPEnroll_ReenrollWithRecoveryCode(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-enroll-recov")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	codes, hashed, err := newRecoveryCodes(5)
	if err != nil {
		t.Fatalf("newRecoveryCodes: %v", err)
	}
	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, hashed, true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	if rec := doEnroll(h, tenantID, userID, codes[0]); rec.Code != http.StatusOK {
		t.Fatalf("re-enroll with recovery code: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}

	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after recovery rotation: %v", err)
	}
	if cred.Enabled {
		t.Fatal("rotated credential must be disabled pending /verify")
	}
	if bytes.Equal(cred.EncryptedSecret, secret) {
		t.Fatal("re-enroll must rotate the stored secret")
	}
	// The whole bundle is cleared on rotation, so the old codes are
	// dead even though only one was presented.
	if cred.RecoveryCodesHash != "" {
		t.Fatalf("re-enroll must clear the recovery bundle: got %q", cred.RecoveryCodesHash)
	}
}

// TestTOTPEnroll_ReenrollWhileLockedRefused proves /enroll honours the
// same lockout gate as /check: while an account is locked, even a valid
// current code cannot rotate the secret — closing the door on using
// re-enrollment to clear a standing lockout.
func TestTOTPEnroll_ReenrollWhileLockedRefused(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-enroll-locked")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const maxAttempts = 3
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: maxAttempts,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "", true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	wrong := "654321"
	if wrong == generateHOTP(secret, now.Unix()/30) {
		wrong = "123456"
	}
	doCheck := func(code string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/check",
			bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
		req.Header.Set("X-KMail-Dev-User-Id", userID)
		rec := httptest.NewRecorder()
		h.check(rec, req)
		return rec.Code
	}
	for i := 0; i < maxAttempts; i++ {
		doCheck(wrong)
	}

	// Even a valid current code is refused with 429 while locked.
	valid := generateHOTP(secret, now.Unix()/30)
	if rec := doEnroll(h, tenantID, userID, valid); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("re-enroll while locked: want 429, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec := doEnroll(h, tenantID, userID, valid); rec.Header().Get("Retry-After") == "" {
		t.Error("locked re-enroll response missing Retry-After header")
	}

	// The live secret survived the lockout — no rotation slipped through.
	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after locked re-enroll: %v", err)
	}
	if !bytes.Equal(cred.EncryptedSecret, secret) {
		t.Fatal("locked re-enroll must not rotate the secret")
	}
	if !cred.Enabled {
		t.Fatal("locked re-enroll must not disable the live credential")
	}
}

// doDisable sends DELETE /api/v1/auth/totp for (tenantID, userID). A
// blank code sends an empty body (no current-factor proof); a non-blank
// code sends {"code":...} to authorize disabling an enabled credential.
func doDisable(h *TOTPHandlers, tenantID, userID, code string) *httptest.ResponseRecorder {
	var body []byte
	if code != "" {
		body = []byte(`{"code":"` + code + `"}`)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/totp", bytes.NewReader(body))
	req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
	req.Header.Set("X-KMail-Dev-User-Id", userID)
	rec := httptest.NewRecorder()
	h.disable(rec, req)
	return rec
}

// TestTOTPDisable_NotEnrolledNoOp confirms disabling when nothing is
// enrolled is an idempotent 204 — no factor required, no error.
func TestTOTPDisable_NotEnrolledNoOp(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-disable-none")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{Pool: admin, Now: func() time.Time { return now }})

	if rec := doDisable(h, tenantID, userID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable when not enrolled: want 204, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestTOTPDisable_UnconfirmedFrictionless confirms a not-yet-confirmed
// (disabled) credential can be removed without a factor — there is no
// active second factor to protect yet, mirroring the frictionless
// enroll/restart path.
func TestTOTPDisable_UnconfirmedFrictionless(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-disable-unconf")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{Pool: admin, Now: func() time.Time { return now }})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "", false, now); err != nil {
		t.Fatalf("seed unconfirmed credential: %v", err)
	}

	if rec := doDisable(h, tenantID, userID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disable unconfirmed: want 204, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if _, err := h.store.Get(t.Context(), tenantID, userID); !errors.Is(err, ErrTOTPNotFound) {
		t.Fatalf("disable must remove the unconfirmed credential: err=%v", err)
	}
}

// TestTOTPDisable_EnabledRequiresFactor is the core hardening for the
// delete-then-reenroll bypass: removing an ALREADY-ENABLED credential
// without proving the current second factor is refused (401), spends a
// failed attempt, and leaves the credential intact.
func TestTOTPDisable_EnabledRequiresFactor(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-disable-noauth")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "recov-hash", true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	// No code → refused.
	if rec := doDisable(h, tenantID, userID, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disable without factor: want 401, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	// Wrong code → also refused.
	wrong := "654321"
	if wrong == generateHOTP(secret, now.Unix()/30) {
		wrong = "123456"
	}
	if rec := doDisable(h, tenantID, userID, wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disable with wrong code: want 401, got %d", rec.Code)
	}

	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after refused disable: %v", err)
	}
	if !cred.Enabled {
		t.Fatal("refused disable must leave the credential enabled")
	}
	if !bytes.Equal(cred.EncryptedSecret, secret) {
		t.Fatal("refused disable must not touch the secret")
	}
	if cred.FailedAttempts != 2 {
		t.Fatalf("two refused disable attempts must each spend one: failed_attempts=%d, want 2", cred.FailedAttempts)
	}
}

// TestTOTPDisable_WithValidTOTP confirms a live TOTP code authorizes
// removal of an enabled credential.
func TestTOTPDisable_WithValidTOTP(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-disable-totp")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "old-recov", true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	valid := generateHOTP(secret, now.Unix()/30)
	if rec := doDisable(h, tenantID, userID, valid); rec.Code != http.StatusNoContent {
		t.Fatalf("disable with valid TOTP: want 204, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if _, err := h.store.Get(t.Context(), tenantID, userID); !errors.Is(err, ErrTOTPNotFound) {
		t.Fatalf("authorized disable must remove the credential: err=%v", err)
	}
}

// TestTOTPDisable_WithRecoveryCode confirms the lost-authenticator
// escape hatch: an unused recovery code authorizes removal.
func TestTOTPDisable_WithRecoveryCode(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-disable-recov")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: 5,
		LockoutDuration:   15 * time.Minute,
	})

	codes, hashed, err := newRecoveryCodes(5)
	if err != nil {
		t.Fatalf("newRecoveryCodes: %v", err)
	}
	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, hashed, true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	if rec := doDisable(h, tenantID, userID, codes[0]); rec.Code != http.StatusNoContent {
		t.Fatalf("disable with recovery code: want 204, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if _, err := h.store.Get(t.Context(), tenantID, userID); !errors.Is(err, ErrTOTPNotFound) {
		t.Fatalf("recovery-authorized disable must remove the credential: err=%v", err)
	}
}

// TestTOTPDisable_WhileLockedRefused proves /disable honours the same
// lockout gate as /check: while an account is locked, even a valid
// current code cannot remove the credential — so disable cannot be used
// to clear a standing lockout and then re-enroll.
func TestTOTPDisable_WhileLockedRefused(t *testing.T) {
	admin := testAdminPool(t)
	tenantID, userID := seedTenantWithUser(t, admin, "totp-disable-locked")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const maxAttempts = 3
	h := NewTOTPHandlers(TOTPConfig{
		Pool:              admin,
		Now:               func() time.Time { return now },
		MaxFailedAttempts: maxAttempts,
		LockoutDuration:   15 * time.Minute,
	})

	secret := []byte("12345678901234567890")
	if err := h.store.Upsert(t.Context(), tenantID, userID, secret, "", true, now); err != nil {
		t.Fatalf("seed enabled credential: %v", err)
	}

	wrong := "654321"
	if wrong == generateHOTP(secret, now.Unix()/30) {
		wrong = "123456"
	}
	doCheck := func(code string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/totp/check",
			bytes.NewReader([]byte(`{"code":"`+code+`"}`)))
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
		req.Header.Set("X-KMail-Dev-User-Id", userID)
		rec := httptest.NewRecorder()
		h.check(rec, req)
		return rec.Code
	}
	for i := 0; i < maxAttempts; i++ {
		doCheck(wrong)
	}

	// Even a valid current code is refused with 429 while locked.
	valid := generateHOTP(secret, now.Unix()/30)
	rec := doDisable(h, tenantID, userID, valid)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("disable while locked: want 429, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("locked disable response missing Retry-After header")
	}

	// The credential survived — disable did not slip through the lock.
	cred, err := h.store.Get(t.Context(), tenantID, userID)
	if err != nil {
		t.Fatalf("get after locked disable: %v", err)
	}
	if !cred.Enabled || !bytes.Equal(cred.EncryptedSecret, secret) {
		t.Fatal("locked disable must not remove or alter the live credential")
	}
}
