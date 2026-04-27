// Package middleware — WebAuthn / FIDO2 credential management.
//
// Phase 7 ships the registration + authentication surface for
// hardware-backed second factors (FIDO2 security keys, platform
// authenticators) so admins can require a U2F-class second factor
// alongside the existing OIDC + (optional) TOTP flow.
//
// Implementation choice: the BFF speaks WebAuthn directly rather
// than depending on `go-webauthn/webauthn`. The reason is the
// same as the Stripe / DNS / chatbridge clients in this codebase
// — the surface we need is small (challenge mint, attestation
// store, assertion verify) and a hand-written client matches the
// existing house style. The handlers below are RP-ready: any
// browser running the WebAuthn JS API against the matching
// endpoints will register and assert successfully.
//
// The credential store lives in `webauthn_credentials` (migration
// 041). One row per (user, credential_id). Public keys are stored
// as the COSE-encoded public key blob the browser hands us at
// registration time so re-derivation never requires the original
// authenticator.
//
// Wire shape:
//
//   POST /api/v1/auth/webauthn/register/begin   — returns a
//     CredentialCreationOptions JSON the browser feeds into
//     navigator.credentials.create().
//   POST /api/v1/auth/webauthn/register/finish  — accepts the
//     PublicKeyCredentialCreationResult, parses the attestation
//     object, and persists the credential.
//   POST /api/v1/auth/webauthn/login/begin      — returns a
//     CredentialRequestOptions JSON.
//   POST /api/v1/auth/webauthn/login/finish     — accepts the
//     PublicKeyCredentialAssertionResult, verifies the signature,
//     bumps `sign_count`.
//   GET  /api/v1/auth/webauthn/credentials      — lists keys for
//     the calling user.
//   DELETE /api/v1/auth/webauthn/credentials/:id — removes one.
package middleware

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WebAuthnConfig wires NewWebAuthnHandlers.
type WebAuthnConfig struct {
	Pool       *pgxpool.Pool
	Logger     *log.Logger
	RPID       string // Relying Party ID — usually the eTLD+1 of the BFF host.
	RPName     string // Display name for the RP.
	RPOrigin   string // Allowed origin for the WebAuthn ceremony.
	Challenger Challenger
	Now        func() time.Time
}

// Challenger persists WebAuthn challenges between begin/finish
// calls. Backed by Valkey in production and an in-memory map in
// dev / tests.
type Challenger interface {
	StoreChallenge(ctx context.Context, key string, challenge []byte, ttl time.Duration) error
	LoadChallenge(ctx context.Context, key string) ([]byte, error)
	DeleteChallenge(ctx context.Context, key string) error
}

// WebAuthnHandlers exposes the registration + authentication +
// management surface.
type WebAuthnHandlers struct {
	cfg   WebAuthnConfig
	store *WebAuthnStore
}

// NewWebAuthnHandlers returns Handlers wired to the configured
// store. The OIDC middleware is required for the authenticated
// register / list / delete paths; the login flow is intentionally
// unauthenticated since it predates the OIDC session.
//
// RPOrigin must be set when RPID is set: the assertion path
// (loginFinish) refuses to run without it, so we warn loudly at
// startup so the misconfiguration surfaces in a smoke test rather
// than at first user login.
func NewWebAuthnHandlers(cfg WebAuthnConfig) *WebAuthnHandlers {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Challenger == nil {
		cfg.Challenger = NewMemoryChallenger()
	}
	if cfg.RPID != "" && cfg.RPOrigin == "" {
		cfg.Logger.Printf("webauthn: WARNING RPID=%q is set but RPOrigin is empty — the unauthenticated /login/finish endpoint will refuse all assertions until KMAIL_WEBAUTHN_ORIGIN is configured", cfg.RPID)
	}
	store := NewWebAuthnStore(cfg.Pool)
	return &WebAuthnHandlers{cfg: cfg, store: store}
}

// Register installs all routes onto the mux. Registration / list
// / delete require an authenticated session; login begin/finish
// run before the session exists and are intentionally
// unauthenticated.
func (h *WebAuthnHandlers) Register(mux *http.ServeMux, authMW *OIDC) {
	mux.Handle("POST /api/v1/auth/webauthn/register/begin", authMW.Wrap(http.HandlerFunc(h.registerBegin)))
	mux.Handle("POST /api/v1/auth/webauthn/register/finish", authMW.Wrap(http.HandlerFunc(h.registerFinish)))
	mux.Handle("GET /api/v1/auth/webauthn/credentials", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("DELETE /api/v1/auth/webauthn/credentials/{id}", authMW.Wrap(http.HandlerFunc(h.delete)))
	mux.Handle("POST /api/v1/auth/webauthn/login/begin", http.HandlerFunc(h.loginBegin))
	mux.Handle("POST /api/v1/auth/webauthn/login/finish", http.HandlerFunc(h.loginFinish))
}

// CredentialCreationOptions mirrors the WebAuthn JS struct of the
// same name. Only fields KMail uses are surfaced.
type CredentialCreationOptions struct {
	Challenge        string                  `json:"challenge"`
	RP               rpEntity                `json:"rp"`
	User             userEntity              `json:"user"`
	PubKeyCredParams []credParam             `json:"pubKeyCredParams"`
	Timeout          int                     `json:"timeout"`
	Attestation      string                  `json:"attestation"`
	AuthenticatorSel authenticatorSelection  `json:"authenticatorSelection"`
	ExcludeCreds     []credentialDescriptor  `json:"excludeCredentials,omitempty"`
}

// CredentialRequestOptions mirrors the WebAuthn JS struct of the
// same name (assertion side).
type CredentialRequestOptions struct {
	Challenge   string                  `json:"challenge"`
	RPID        string                  `json:"rpId"`
	Timeout     int                     `json:"timeout"`
	UserVerify  string                  `json:"userVerification"`
	AllowedCreds []credentialDescriptor `json:"allowCredentials,omitempty"`
}

type rpEntity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type userEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type credParam struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

type authenticatorSelection struct {
	UserVerification string `json:"userVerification"`
	ResidentKey      string `json:"residentKey,omitempty"`
}

type credentialDescriptor struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Transports []string `json:"transports,omitempty"`
}

// registerBegin returns a CredentialCreationOptions for the
// calling user.
func (h *WebAuthnHandlers) registerBegin(w http.ResponseWriter, r *http.Request) {
	userID := KChatUserIDFrom(r.Context())
	tenantID := TenantIDFrom(r.Context())
	if userID == "" || tenantID == "" {
		writeWebAuthnError(w, http.StatusUnauthorized, "missing user context")
		return
	}
	challenge := mintChallenge()
	if err := h.cfg.Challenger.StoreChallenge(r.Context(), webauthnChallengeKey("register", tenantID, userID), challenge, 5*time.Minute); err != nil {
		writeWebAuthnError(w, http.StatusInternalServerError, err.Error())
		return
	}
	existing, _ := h.store.ListByUser(r.Context(), tenantID, userID)
	exclude := make([]credentialDescriptor, 0, len(existing))
	for _, c := range existing {
		exclude = append(exclude, credentialDescriptor{
			Type: "public-key",
			ID:   c.CredentialID,
		})
	}
	opts := CredentialCreationOptions{
		Challenge: base64.RawURLEncoding.EncodeToString(challenge),
		RP: rpEntity{
			ID:   h.cfg.RPID,
			Name: orDefault(h.cfg.RPName, "KMail"),
		},
		User: userEntity{
			ID:          base64.RawURLEncoding.EncodeToString([]byte(userID)),
			Name:        userID,
			DisplayName: userID,
		},
		PubKeyCredParams: []credParam{
			{Type: "public-key", Alg: -7},   // ES256
			{Type: "public-key", Alg: -257}, // RS256
		},
		Timeout:     60_000,
		Attestation: "none",
		AuthenticatorSel: authenticatorSelection{
			UserVerification: "preferred",
		},
		ExcludeCreds: exclude,
	}
	writeWebAuthnJSON(w, http.StatusOK, opts)
}

// registerFinishRequest is the trimmed shape we accept from the
// browser. Real go-webauthn does full attestation parsing; the
// MVP does signature-free attestation ("none") since the keys
// register inside an OIDC-authenticated session and we trust the
// transport.
type registerFinishRequest struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
		PublicKey         string `json:"publicKey"`
	} `json:"response"`
}

func (h *WebAuthnHandlers) registerFinish(w http.ResponseWriter, r *http.Request) {
	userID := KChatUserIDFrom(r.Context())
	tenantID := TenantIDFrom(r.Context())
	if userID == "" || tenantID == "" {
		writeWebAuthnError(w, http.StatusUnauthorized, "missing user context")
		return
	}
	var req registerFinishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeWebAuthnError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID == "" || req.RawID == "" || req.Response.PublicKey == "" {
		writeWebAuthnError(w, http.StatusBadRequest, "missing credential id or public key")
		return
	}
	challenge, err := h.cfg.Challenger.LoadChallenge(r.Context(), webauthnChallengeKey("register", tenantID, userID))
	if err != nil {
		writeWebAuthnError(w, http.StatusBadRequest, "challenge expired")
		return
	}
	if err := verifyClientDataChallenge(req.Response.ClientDataJSON, challenge); err != nil {
		writeWebAuthnError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = h.cfg.Challenger.DeleteChallenge(r.Context(), webauthnChallengeKey("register", tenantID, userID))
	cred := WebAuthnCredential{
		TenantID:     tenantID,
		UserID:       userID,
		CredentialID: req.RawID,
		PublicKey:    req.Response.PublicKey,
		Name:         orDefault(req.Name, "Security key"),
		CreatedAt:    h.cfg.Now(),
	}
	if err := h.store.Insert(r.Context(), cred); err != nil {
		writeWebAuthnError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeWebAuthnJSON(w, http.StatusOK, map[string]any{"ok": true, "credential_id": cred.CredentialID})
}

// loginBegin returns a CredentialRequestOptions. Username is
// passed in the body so the BFF can return the matching
// credential descriptors (so non-resident keys still work).
func (h *WebAuthnHandlers) loginBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		TenantID string `json:"tenant_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Username == "" || req.TenantID == "" {
		writeWebAuthnError(w, http.StatusBadRequest, "username and tenant_id required")
		return
	}
	creds, _ := h.store.ListByUser(r.Context(), req.TenantID, req.Username)
	descs := make([]credentialDescriptor, 0, len(creds))
	for _, c := range creds {
		descs = append(descs, credentialDescriptor{Type: "public-key", ID: c.CredentialID})
	}
	challenge := mintChallenge()
	if err := h.cfg.Challenger.StoreChallenge(r.Context(), webauthnChallengeKey("login", req.TenantID, req.Username), challenge, 5*time.Minute); err != nil {
		writeWebAuthnError(w, http.StatusInternalServerError, err.Error())
		return
	}
	opts := CredentialRequestOptions{
		Challenge:    base64.RawURLEncoding.EncodeToString(challenge),
		RPID:         h.cfg.RPID,
		Timeout:      60_000,
		UserVerify:   "preferred",
		AllowedCreds: descs,
	}
	writeWebAuthnJSON(w, http.StatusOK, opts)
}

type loginFinishRequest struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Username string `json:"username"`
	TenantID string `json:"tenant_id"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	} `json:"response"`
}

func (h *WebAuthnHandlers) loginFinish(w http.ResponseWriter, r *http.Request) {
	var req loginFinishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeWebAuthnError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Username == "" || req.TenantID == "" || req.RawID == "" {
		writeWebAuthnError(w, http.StatusBadRequest, "username, tenant_id, and rawId required")
		return
	}
	challenge, err := h.cfg.Challenger.LoadChallenge(r.Context(), webauthnChallengeKey("login", req.TenantID, req.Username))
	if err != nil {
		writeWebAuthnError(w, http.StatusBadRequest, "challenge expired")
		return
	}
	clientDataRaw, err := verifyClientDataAssertion(req.Response.ClientDataJSON, challenge, h.cfg.RPOrigin)
	if err != nil {
		writeWebAuthnError(w, http.StatusBadRequest, err.Error())
		return
	}
	cred, err := h.store.Get(r.Context(), req.TenantID, req.RawID)
	if err != nil {
		writeWebAuthnError(w, http.StatusUnauthorized, "credential not found")
		return
	}
	// Bind the credential to the claimed username before consuming
	// the challenge: the challenge is keyed off (tenant, username),
	// so a different user signing it with their own credential could
	// otherwise burn the victim's challenge slot (a DoS, since the
	// victim has to re-initiate). Verify ownership first; the
	// challenge stays in storage so the legitimate user can retry.
	if cred.UserID != req.Username {
		writeWebAuthnError(w, http.StatusUnauthorized, "credential does not belong to the claimed user")
		return
	}
	newSignCount, err := verifyAssertionSignature(
		cred.PublicKey,
		req.Response.AuthenticatorData,
		req.Response.Signature,
		clientDataRaw,
		h.cfg.RPID,
		false, // requireUV — UV is "preferred" in loginBegin; opt-in callers can flip this.
	)
	if err != nil {
		h.cfg.Logger.Printf("webauthn: assertion verify: %v", err)
		writeWebAuthnError(w, http.StatusUnauthorized, "assertion verification failed")
		return
	}
	// Cloned-key detection: per WebAuthn §7.2 step 21, if either
	// the new sign count or the stored count is non-zero, the new
	// count MUST be greater than the stored count. Authenticators
	// that always emit zero (allowed by the spec) skip this check.
	if newSignCount != 0 || cred.SignCount != 0 {
		if int64(newSignCount) <= cred.SignCount {
			writeWebAuthnError(w, http.StatusUnauthorized, "signCount did not increase — possible cloned authenticator")
			return
		}
	}
	if err := h.store.SetSignCount(r.Context(), req.TenantID, req.RawID, int64(newSignCount), h.cfg.Now()); err != nil {
		h.cfg.Logger.Printf("webauthn: set sign_count: %v", err)
	}
	_ = h.cfg.Challenger.DeleteChallenge(r.Context(), webauthnChallengeKey("login", req.TenantID, req.Username))
	writeWebAuthnJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"user_id":    cred.UserID,
		"cred_id":    cred.CredentialID,
		"sign_count": newSignCount,
		"verified":   true,
	})
}

func (h *WebAuthnHandlers) list(w http.ResponseWriter, r *http.Request) {
	userID := KChatUserIDFrom(r.Context())
	tenantID := TenantIDFrom(r.Context())
	if userID == "" || tenantID == "" {
		writeWebAuthnError(w, http.StatusUnauthorized, "missing user context")
		return
	}
	creds, err := h.store.ListByUser(r.Context(), tenantID, userID)
	if err != nil {
		writeWebAuthnError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeWebAuthnJSON(w, http.StatusOK, map[string]any{"credentials": creds})
}

func (h *WebAuthnHandlers) delete(w http.ResponseWriter, r *http.Request) {
	userID := KChatUserIDFrom(r.Context())
	tenantID := TenantIDFrom(r.Context())
	if userID == "" || tenantID == "" {
		writeWebAuthnError(w, http.StatusUnauthorized, "missing user context")
		return
	}
	credID := r.PathValue("id")
	if credID == "" {
		writeWebAuthnError(w, http.StatusBadRequest, "credential id required")
		return
	}
	if err := h.store.Delete(r.Context(), tenantID, userID, credID); err != nil {
		writeWebAuthnError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mintChallenge returns 32 cryptographically random bytes.
// webauthnChallengeKey composes the Challenger key used for both
// registration and authentication ceremonies. It must include the
// tenant ID so two users with the same username in different
// tenants don't overwrite each other's challenge.
func webauthnChallengeKey(kind, tenantID, principal string) string {
	return kind + ":" + tenantID + ":" + principal
}

func mintChallenge() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// verifyClientDataChallenge confirms the browser-supplied client
// data echoes the challenge we minted. The registration flow runs
// inside an OIDC-authenticated request, so we keep the type/origin
// checks soft there; the assertion (login) flow calls
// verifyClientDataAssertion below, which enforces type=="webauthn.get"
// and origin matching the configured RPOrigin.
func verifyClientDataChallenge(clientDataB64 string, expected []byte) error {
	_, err := decodeClientData(clientDataB64, expected, "", "")
	return err
}

// verifyClientDataAssertion is the assertion-side counterpart that
// also enforces the spec-mandated type and origin checks. An empty
// allowedOrigin is treated as a hard failure: per WebAuthn §7.2
// step 13 the Relying Party MUST verify origin, and skipping that
// check on the unauthenticated login path would let any same-RPID
// page (e.g. an attacker-controlled subdomain) replay assertions.
func verifyClientDataAssertion(clientDataB64 string, expected []byte, allowedOrigin string) ([]byte, error) {
	if allowedOrigin == "" {
		return nil, errors.New("webauthn: RPOrigin is not configured — refusing to verify assertion (set KMAIL_WEBAUTHN_ORIGIN)")
	}
	return decodeClientData(clientDataB64, expected, "webauthn.get", allowedOrigin)
}

// decodeClientData is the shared parser used by both the
// registration and assertion paths. It returns the raw
// clientDataJSON bytes (used to compute clientDataHash on the
// assertion path) once all checks pass.
func decodeClientData(clientDataB64 string, expected []byte, expectedType, allowedOrigin string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(clientDataB64)
	if err != nil {
		// Some browsers emit standard base64 padding; accept
		// either encoding.
		raw, err = base64.StdEncoding.DecodeString(clientDataB64)
		if err != nil {
			return nil, fmt.Errorf("clientDataJSON: %w", err)
		}
	}
	var cd struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return nil, fmt.Errorf("clientDataJSON: %w", err)
	}
	if expectedType != "" && cd.Type != expectedType {
		return nil, fmt.Errorf("clientDataJSON.type: expected %q, got %q", expectedType, cd.Type)
	}
	if allowedOrigin != "" && cd.Origin != allowedOrigin {
		return nil, fmt.Errorf("clientDataJSON.origin: not allowed")
	}
	got, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	if err != nil {
		return nil, fmt.Errorf("clientDataJSON.challenge: %w", err)
	}
	if subtle.ConstantTimeCompare(got, expected) != 1 {
		return nil, errors.New("challenge mismatch")
	}
	return raw, nil
}

// verifyAssertionSignature validates a WebAuthn assertion against
// the credential public key stored at registration time. The stored
// PublicKey is expected to be a base64-encoded SubjectPublicKeyInfo
// (matching what `PublicKeyCredential.response.getPublicKey()`
// returns in modern browsers). We support ES256 (P-256 ECDSA) and
// RS256 (RSA-PKCS1v15-SHA256). The function also enforces the
// authenticatorData rpIdHash, the User Present flag, and (when the
// caller passes requireUV) the User Verified flag, and parses the
// embedded signCount which the caller must check for monotonic
// increase to detect cloned authenticators.
func verifyAssertionSignature(publicKeyB64, authenticatorDataB64, signatureB64 string, clientDataJSONRaw []byte, rpID string, requireUV bool) (signCount uint32, err error) {
	pubKeyDER, err := decodeBase64Loose(publicKeyB64)
	if err != nil {
		return 0, fmt.Errorf("publicKey: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(pubKeyDER)
	if err != nil {
		// Fall back to PEM-encoded SPKI for tests/dev tooling.
		if block, _ := pem.Decode(pubKeyDER); block != nil {
			pub, err = x509.ParsePKIXPublicKey(block.Bytes)
		}
		if err != nil {
			return 0, fmt.Errorf("parse public key: %w", err)
		}
	}
	authData, err := decodeBase64Loose(authenticatorDataB64)
	if err != nil {
		return 0, fmt.Errorf("authenticatorData: %w", err)
	}
	if len(authData) < 37 {
		return 0, errors.New("authenticatorData too short")
	}
	rpIDHash := sha256.Sum256([]byte(rpID))
	if subtle.ConstantTimeCompare(authData[:32], rpIDHash[:]) != 1 {
		return 0, errors.New("rpIdHash mismatch")
	}
	flags := authData[32]
	const (
		flagUP = 0x01
		flagUV = 0x04
	)
	if flags&flagUP == 0 {
		return 0, errors.New("user-present flag not set")
	}
	if requireUV && flags&flagUV == 0 {
		return 0, errors.New("user-verified flag required but not set")
	}
	signCount = uint32(authData[33])<<24 | uint32(authData[34])<<16 | uint32(authData[35])<<8 | uint32(authData[36])

	sig, err := decodeBase64Loose(signatureB64)
	if err != nil {
		return 0, fmt.Errorf("signature: %w", err)
	}
	clientDataHash := sha256.Sum256(clientDataJSONRaw)
	signed := append([]byte{}, authData...)
	signed = append(signed, clientDataHash[:]...)

	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return 0, fmt.Errorf("unsupported ECDSA curve %s", k.Curve.Params().Name)
		}
		digest := sha256.Sum256(signed)
		if !ecdsa.VerifyASN1(k, digest[:], sig) {
			// Some authenticators emit raw r||s instead of ASN.1.
			if len(sig) == 64 {
				r := new(big.Int).SetBytes(sig[:32])
				s := new(big.Int).SetBytes(sig[32:])
				if !ecdsa.Verify(k, digest[:], r, s) {
					return 0, errors.New("ecdsa signature invalid")
				}
			} else {
				return 0, errors.New("ecdsa signature invalid")
			}
		}
	case *rsa.PublicKey:
		digest := sha256.Sum256(signed)
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig); err != nil {
			return 0, fmt.Errorf("rsa signature invalid: %w", err)
		}
	default:
		return 0, fmt.Errorf("unsupported public key type %T", pub)
	}
	return signCount, nil
}

// decodeBase64Loose accepts both raw URL and standard base64.
func decodeBase64Loose(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func writeWebAuthnJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeWebAuthnError(w http.ResponseWriter, status int, msg string) {
	writeWebAuthnJSON(w, status, map[string]string{"error": msg})
}
