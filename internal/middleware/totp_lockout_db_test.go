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
