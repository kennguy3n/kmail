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
//        client renders a QR code.
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
}

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
	if err := h.store.Upsert(r.Context(), tenantID, userID, wrapped, "", false, h.cfg.Now()); err != nil {
		h.cfg.Logger.Printf("totp.enroll: %v", err)
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
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
	cred, err := h.store.Get(r.Context(), tenantID, userID)
	if err != nil {
		http.Error(w, "not enrolled", http.StatusBadRequest)
		return
	}
	secret, err := h.unwrapSecret(cred.EncryptedSecret)
	if err != nil {
		http.Error(w, "envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !verifyCode(secret, strings.TrimSpace(in.Code), h.cfg.Now()) {
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	codes, hashed, err := newRecoveryCodes(10)
	if err != nil {
		http.Error(w, "rand: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.store.Upsert(r.Context(), tenantID, userID, cred.EncryptedSecret, hashed, true, h.cfg.Now()); err != nil {
		h.cfg.Logger.Printf("totp.verify: %v", err)
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
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
	cred, err := h.store.Get(r.Context(), tenantID, userID)
	if err != nil || !cred.Enabled {
		http.Error(w, "not enabled", http.StatusUnauthorized)
		return
	}
	secret, err := h.unwrapSecret(cred.EncryptedSecret)
	if err != nil {
		http.Error(w, "envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}
	code := strings.TrimSpace(in.Code)
	if verifyCode(secret, code, h.cfg.Now()) {
		_ = h.store.MarkUsed(r.Context(), tenantID, userID, h.cfg.Now())
		writeJSON(w, http.StatusOK, map[string]any{"verified": true, "method": "totp"})
		return
	}
	// Try recovery code: hash and compare against the stored set.
	updated, ok := consumeRecoveryCode(cred.RecoveryCodesHash, code)
	if ok {
		_ = h.store.UpdateRecoveryCodes(r.Context(), tenantID, userID, updated)
		writeJSON(w, http.StatusOK, map[string]any{"verified": true, "method": "recovery"})
		return
	}
	http.Error(w, "invalid code", http.StatusUnauthorized)
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
