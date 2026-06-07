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
