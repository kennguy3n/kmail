// Package middleware — TOTP (RFC 6238) fallback second factor.
//
// PROPOSAL.md §10.1 specifies TOTP as the fallback for FIDO2 /
// WebAuthn. KMail stores the per-user shared secret wrapped by
// the `kmail-secrets` AEAD envelope (`internal/cmk/envelope.go`)
// and never returns it on the wire after enrolment. Recovery
// codes are bcrypt-hashed.
//
// Wire shape:
//
//   POST /api/v1/auth/totp/enroll   — mints a fresh secret + QR
//        URI, returns the otpauth:// URI and base32 secret. The
//        client renders a QR code. Re-enrolling an already-enabled
//        credential (secret rotation) requires proving the current
//        second factor (a live TOTP code or an unused recovery code)
//        in the body, checked through the same brute-force lockout
//        path as /check so it cannot be used to bypass the cooldown.
//   POST /api/v1/auth/totp/verify   — accepts a 6-digit code; on
//        success flips the credential to `enabled=true` and
//        returns 10 recovery codes (one-time view).
//   POST /api/v1/auth/totp/check    — runs a verification (used at
//        login). Honours both regular codes and recovery codes
//        (recovery codes self-delete on use).
//   GET  /api/v1/auth/totp/status   — returns `{enrolled, enabled}`.
//   DELETE /api/v1/auth/totp        — disable TOTP for the user.
package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TOTPConfig wires NewTOTPHandlers.
type TOTPConfig struct {
	Pool     *pgxpool.Pool
	Logger   *log.Logger
	Issuer   string // shown in authenticator apps; defaults to "KMail"
	Envelope SecretEnvelope
	Now      func() time.Time

	// MaxFailedAttempts is the number of consecutive failed
	// verifications (wrong TOTP or recovery code) tolerated before
	// the account is locked. Defaults to defaultMaxFailedAttempts.
	MaxFailedAttempts int
	// LockoutDuration is how long a locked account is refused before
	// the window resets. Defaults to defaultLockoutDuration.
	LockoutDuration time.Duration
}

const (
	defaultMaxFailedAttempts = 5
	defaultLockoutDuration   = 15 * time.Minute
)

// SecretEnvelope is the small interface this package needs from
// `internal/cmk` (or a test fake). It mirrors cmk.SecretsEnvelope
// without importing it (avoids cyclic deps).
type SecretEnvelope interface {
	Wrap(plaintext []byte) ([]byte, error)
	Unwrap(blob []byte) (plaintext []byte, wasEncrypted bool, err error)
}

// TOTPHandlers exposes the HTTP surface.
type TOTPHandlers struct {
	cfg   TOTPConfig
	store *TOTPStore
}

// NewTOTPHandlers builds the handlers. A nil envelope is allowed
// for dev — the secret is then stored as raw bytes (the migration
// already requires the column to be BYTEA so the read path stays
// consistent), but production deployments MUST configure an
// envelope. The handler logs a warning when running unwrapped.
func NewTOTPHandlers(cfg TOTPConfig) *TOTPHandlers {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Issuer == "" {
		cfg.Issuer = "KMail"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxFailedAttempts <= 0 {
		cfg.MaxFailedAttempts = defaultMaxFailedAttempts
	}
	if cfg.LockoutDuration <= 0 {
		cfg.LockoutDuration = defaultLockoutDuration
	}
	if cfg.Envelope == nil {
		cfg.Logger.Print("totp: KMAIL_SECRETS_KEY not set — running without envelope wrap (DEV ONLY)")
	}
	return &TOTPHandlers{cfg: cfg, store: NewTOTPStore(cfg.Pool)}
}

// Register binds the handlers to mux behind the OIDC middleware.
func (h *TOTPHandlers) Register(mux *http.ServeMux, authMW *OIDC) {
	mux.Handle("POST /api/v1/auth/totp/enroll", authMW.Wrap(http.HandlerFunc(h.enroll)))
	mux.Handle("POST /api/v1/auth/totp/verify", authMW.Wrap(http.HandlerFunc(h.verify)))
	mux.Handle("POST /api/v1/auth/totp/check", authMW.Wrap(http.HandlerFunc(h.check)))
	mux.Handle("GET /api/v1/auth/totp/status", authMW.Wrap(http.HandlerFunc(h.status)))
	mux.Handle("DELETE /api/v1/auth/totp", authMW.Wrap(http.HandlerFunc(h.disable)))
}

// EnrollRequest is the (optional) body of /enroll. The `code` field
// is only consulted when re-enrolling an already-enabled credential
// (secret rotation): the caller must prove possession of the current
// second factor — a live TOTP code or an unused recovery code — before
// a new secret is issued. A first-time or not-yet-confirmed enrollment
// may send an empty body.
type EnrollRequest struct {
	Code string `json:"code"`
}

// EnrollResponse is the body of /enroll.
type EnrollResponse struct {
	OTPAuthURI string `json:"otpauth_uri"`
	Secret     string `json:"secret"`
}

// VerifyRequest is the body of /verify and /check.
type VerifyRequest struct {
	Code string `json:"code"`
}

// VerifyResponse is the body of /verify.
type VerifyResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

// StatusResponse is the body of /status.
type StatusResponse struct {
	Enrolled bool `json:"enrolled"`
	Enabled  bool `json:"enabled"`
}

func (h *TOTPHandlers) enroll(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, err := h.identity(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Optional current-factor proof. Required only to rotate the
	// secret of an already-enabled credential; empty for a first-time
	// or not-yet-confirmed enrollment.
	var in EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(in.Code)

	secret := make([]byte, 20) // 160-bit per RFC 4226 §4
	if _, err := rand.Read(secret); err != nil {
		http.Error(w, "rand: "+err.Error(), http.StatusInternalServerError)
		return
	}
	wrapped, err := h.wrapSecret(secret)
	if err != nil {
		http.Error(w, "envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Re-enrollment guard. An already-enabled credential may only be
	// rotated after proving possession of the *current* second factor,
	// verified through the same SELECT ... FOR UPDATE lockout path as
	// /check — otherwise /enroll would be a free way to clear a standing
	// lockout (the rotation resets failed_attempts/locked_until). The
	// recovery code is the escape hatch for a lost authenticator. On
	// success the new (disabled) secret is written atomically in that
	// same locked transaction and the old recovery bundle is cleared;
	// the user re-confirms via /verify, which mints a fresh bundle.
	//
	// requireEnabled=true means a first-time (no row) or unconfirmed
	// (disabled) credential never reaches the verify closure — those
	// enroll without a second factor.
	emptyRecovery := ""
	disabled := false
	newSecret := wrapped
	res, err := h.store.EvaluateAttempt(
		r.Context(), tenantID, userID, h.cfg.Now(),
		h.cfg.MaxFailedAttempts, h.cfg.LockoutDuration,
		true,
		func(cred *TOTPCredential) TOTPVerification {
			sec, uerr := h.unwrapSecret(cred.EncryptedSecret)
			if uerr != nil {
				return TOTPVerification{Err: uerr}
			}
			if verifyCode(sec, code, h.cfg.Now()) {
				return TOTPVerification{
					OK: true, Method: "totp",
					SetEncryptedSecret: &newSecret,
					SetRecoveryHash:    &emptyRecovery,
					SetEnabled:         &disabled,
				}
			}
			if _, ok := consumeRecoveryCode(cred.RecoveryCodesHash, code); ok {
				return TOTPVerification{
					OK: true, Method: "recovery",
					SetEncryptedSecret: &newSecret,
					SetRecoveryHash:    &emptyRecovery,
					SetEnabled:         &disabled,
				}
			}
			return TOTPVerification{}
		},
	)
	switch {
	case errors.Is(err, ErrTOTPNotFound):
		// No credential yet — first enrollment, no factor required.
		if uerr := h.store.Upsert(r.Context(), tenantID, userID, wrapped, "", false, h.cfg.Now()); uerr != nil {
			h.cfg.Logger.Printf("totp.enroll: %v", uerr)
			http.Error(w, "store: "+uerr.Error(), http.StatusInternalServerError)
			return
		}
	case err != nil:
		h.cfg.Logger.Printf("totp.enroll: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	case res.NotEnabled:
		// Row exists but enrollment was never confirmed — let the user
		// restart enrollment without a second factor.
		if uerr := h.store.Upsert(r.Context(), tenantID, userID, wrapped, "", false, h.cfg.Now()); uerr != nil {
			h.cfg.Logger.Printf("totp.enroll: %v", uerr)
			http.Error(w, "store: "+uerr.Error(), http.StatusInternalServerError)
			return
		}
	case res.Locked:
		h.writeLocked(w, res.RetryAfter)
		return
	case !res.Verified:
		// Enabled credential, but no valid current factor supplied.
		http.Error(w, "current TOTP or recovery code required to re-enroll", http.StatusUnauthorized)
		return
	}
	// res.Verified == true: EvaluateAttempt already persisted the
	// rotated (disabled) secret atomically — fall through to hand back
	// the new provisioning URI.

	uri := h.otpauthURI(tenantID, userID, secret)
	writeJSON(w, http.StatusOK, EnrollResponse{
		OTPAuthURI: uri,
		Secret:     base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret),
	})
}

func (h *TOTPHandlers) verify(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, err := h.identity(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var in VerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(in.Code)
	// Recovery codes are minted up front but only persisted (and
	// returned) when verification succeeds — see the success branch
	// of EvaluateAttempt. On a wrong code they are simply discarded.
	codes, hashed, err := newRecoveryCodes(10)
	if err != nil {
		http.Error(w, "rand: "+err.Error(), http.StatusInternalServerError)
		return
	}
	enabled := true
	res, err := h.store.EvaluateAttempt(
		r.Context(), tenantID, userID, h.cfg.Now(),
		h.cfg.MaxFailedAttempts, h.cfg.LockoutDuration,
		false, // enrollment confirmation: the row exists but is not yet enabled
		func(cred *TOTPCredential) TOTPVerification {
			// verify is enrollment confirmation only. If the credential
			// is already enabled, refuse rather than re-minting (and
			// overwriting) the recovery bundle — that would silently
			// invalidate codes the user already saved. Aborts via Err so
			// no attempt is spent and nothing is written.
			if cred.Enabled {
				return TOTPVerification{Err: ErrTOTPAlreadyEnabled}
			}
			secret, uerr := h.unwrapSecret(cred.EncryptedSecret)
			if uerr != nil {
				return TOTPVerification{Err: uerr}
			}
			if verifyCode(secret, code, h.cfg.Now()) {
				return TOTPVerification{OK: true, Method: "totp", SetRecoveryHash: &hashed, SetEnabled: &enabled}
			}
			return TOTPVerification{}
		},
	)
	if errors.Is(err, ErrTOTPNotFound) {
		http.Error(w, "not enrolled", http.StatusBadRequest)
		return
	}
	if errors.Is(err, ErrTOTPAlreadyEnabled) {
		http.Error(w, "already enrolled", http.StatusConflict)
		return
	}
	if err != nil {
		h.cfg.Logger.Printf("totp.verify: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if res.Locked {
		h.writeLocked(w, res.RetryAfter)
		return
	}
	if !res.Verified {
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, VerifyResponse{RecoveryCodes: codes})
}

func (h *TOTPHandlers) check(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, err := h.identity(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var in VerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(in.Code)
	res, err := h.store.EvaluateAttempt(
		r.Context(), tenantID, userID, h.cfg.Now(),
		h.cfg.MaxFailedAttempts, h.cfg.LockoutDuration,
		true, // login: only an enabled credential may be checked
		func(cred *TOTPCredential) TOTPVerification {
			secret, uerr := h.unwrapSecret(cred.EncryptedSecret)
			if uerr != nil {
				return TOTPVerification{Err: uerr}
			}
			if verifyCode(secret, code, h.cfg.Now()) {
				return TOTPVerification{OK: true, Method: "totp"}
			}
			// Recovery code: hash + compare against the stored set,
			// persisting the post-consumption bundle atomically so the
			// code cannot be double-spent by a concurrent attempt.
			if updated, ok := consumeRecoveryCode(cred.RecoveryCodesHash, code); ok {
				return TOTPVerification{OK: true, Method: "recovery", SetRecoveryHash: &updated}
			}
			return TOTPVerification{}
		},
	)
	if errors.Is(err, ErrTOTPNotFound) {
		http.Error(w, "not enabled", http.StatusUnauthorized)
		return
	}
	if err != nil {
		h.cfg.Logger.Printf("totp.check: %v", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if res.NotEnabled {
		http.Error(w, "not enabled", http.StatusUnauthorized)
		return
	}
	if res.Locked {
		h.writeLocked(w, res.RetryAfter)
		return
	}
	if !res.Verified {
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "method": res.Method})
}

// writeLocked renders the brute-force lockout response: 429 with a
// Retry-After header (seconds until the lock elapses, floored at 1).
func (h *TOTPHandlers) writeLocked(w http.ResponseWriter, remaining time.Duration) {
	secs := int(remaining.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", itoa(secs))
	http.Error(w, "too many attempts; try again later", http.StatusTooManyRequests)
}

func (h *TOTPHandlers) status(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, err := h.identity(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	cred, err := h.store.Get(r.Context(), tenantID, userID)
	if err != nil {
		writeJSON(w, http.StatusOK, StatusResponse{})
		return
	}
	writeJSON(w, http.StatusOK, StatusResponse{Enrolled: true, Enabled: cred.Enabled})
}

func (h *TOTPHandlers) disable(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, err := h.identity(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if err := h.store.Delete(r.Context(), tenantID, userID); err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// identity pulls (tenantID, userID) off the OIDC-decorated request.
// In dev / unit-test paths the OIDC middleware may not be wired so
// we also accept the X-KMail-Dev-Tenant-Id / X-KMail-Dev-User-Id
// headers.
func (h *TOTPHandlers) identity(r *http.Request) (string, string, error) {
	if t, u := TenantIDFrom(r.Context()), KChatUserIDFrom(r.Context()); t != "" && u != "" {
		return t, u, nil
	}
	tenant := r.Header.Get("X-KMail-Dev-Tenant-Id")
	user := r.Header.Get("X-KMail-Dev-User-Id")
	if tenant != "" && user != "" {
		return tenant, user, nil
	}
	return "", "", errors.New("totp: caller has no identity")
}

// wrapSecret runs the TOTP secret through the kmail-secrets
// envelope. When unconfigured (dev), returns the raw bytes —
// callers always read through unwrapSecret which handles both.
func (h *TOTPHandlers) wrapSecret(secret []byte) ([]byte, error) {
	if h.cfg.Envelope == nil {
		return secret, nil
	}
	return h.cfg.Envelope.Wrap(secret)
}

func (h *TOTPHandlers) unwrapSecret(blob []byte) ([]byte, error) {
	if h.cfg.Envelope == nil {
		return blob, nil
	}
	plain, _, err := h.cfg.Envelope.Unwrap(blob)
	if err != nil {
		return nil, err
	}
	return plain, nil
}

// otpauthURI builds the otpauth:// URI per
// https://github.com/google/google-authenticator/wiki/Key-Uri-Format.
func (h *TOTPHandlers) otpauthURI(tenantID, userID string, secret []byte) string {
	label := fmt.Sprintf("%s:%s", h.cfg.Issuer, userID)
	q := url.Values{}
	q.Set("secret", base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret))
	q.Set("issuer", h.cfg.Issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	if tenantID != "" {
		q.Set("tenant", tenantID)
	}
	return "otpauth://totp/" + url.PathEscape(label) + "?" + q.Encode()
}

// verifyCode evaluates the code against TOTP at `now`, allowing
// ±1 30-second window of clock drift (RFC 6238 §5.2).
func verifyCode(secret []byte, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}
	step := now.Unix() / 30
	for delta := int64(-1); delta <= 1; delta++ {
		want := generateHOTP(secret, step+delta)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// generateHOTP returns the 6-digit HOTP for the given counter
// (RFC 4226). TOTP is HOTP with counter = floor(t / 30s).
func generateHOTP(secret []byte, counter int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf)
	sum := mac.Sum(nil)
	off := int(sum[len(sum)-1] & 0x0f)
	value := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])
	return fmt.Sprintf("%06d", value%1_000_000)
}

// newRecoveryCodes mints `n` 10-character base32 codes and returns
// them along with a `|`-delimited string of SHA-256 hashes of each
// code (production deployments would prefer bcrypt; we use SHA-256
// to keep the dependency footprint flat — the codes are 10
// characters of high-entropy base32 so brute force is not credible).
func newRecoveryCodes(n int) (codes []string, hashed string, err error) {
	codes = make([]string, n)
	hashes := make([]string, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			return nil, "", err
		}
		c := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
		codes[i] = c[:5] + "-" + c[5:]
		sum := sha256.Sum256([]byte(codes[i]))
		hashes[i] = hex.EncodeToString(sum[:])
	}
	return codes, strings.Join(hashes, "|"), nil
}

// consumeRecoveryCode looks for `code` in the stored hash bundle
// and returns the bundle with that code removed when found.
func consumeRecoveryCode(bundle, code string) (string, bool) {
	if bundle == "" || code == "" {
		return bundle, false
	}
	want := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	wantHex := hex.EncodeToString(want[:])
	parts := strings.Split(bundle, "|")
	out := make([]string, 0, len(parts))
	found := false
	for _, p := range parts {
		if !found && subtle.ConstantTimeCompare([]byte(p), []byte(wantHex)) == 1 {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return bundle, false
	}
	return strings.Join(out, "|"), true
}

// writeJSON is a tiny helper mirroring the rest of the package.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
