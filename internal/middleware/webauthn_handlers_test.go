package middleware

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// --- ceremony helpers -------------------------------------------------------

const (
	waTestRPID   = "kmail.example"
	waTestOrigin = "https://kmail.example"
	waTestRPName = "KMail"
)

func newWebAuthnTestHandlers(t *testing.T) (*WebAuthnHandlers, *MemoryChallenger, *pgxpool.Pool, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	ch := NewMemoryChallenger()
	h := NewWebAuthnHandlers(WebAuthnConfig{
		Pool:       pool,
		RPID:       waTestRPID,
		RPName:     waTestRPName,
		RPOrigin:   waTestOrigin,
		Challenger: ch,
	})
	return h, ch, pool, tenant
}

func waAuthedCtx(tenant, user string) context.Context {
	return WithKChatUserID(WithTenantID(context.Background(), tenant), user)
}

// clientDataJSON builds the raw bytes a browser would hand back and
// the base64url form transmitted in the credential response.
func clientDataJSON(t *testing.T, typ, challengeB64URL, origin string) (raw []byte, b64 string) {
	t.Helper()
	cd := map[string]string{"type": typ, "challenge": challengeB64URL, "origin": origin}
	raw, err := json.Marshal(cd)
	if err != nil {
		t.Fatalf("marshal clientData: %v", err)
	}
	return raw, base64.RawURLEncoding.EncodeToString(raw)
}

// signAssertion produces the authenticatorData + ECDSA signature a
// P-256 authenticator would emit for the given clientDataJSON.
func signAssertion(t *testing.T, priv *ecdsa.PrivateKey, rpID string, flags byte, signCount uint32, clientDataRaw []byte) (authDataB64, sigB64 string) {
	t.Helper()
	authData := make([]byte, 37)
	rpHash := sha256.Sum256([]byte(rpID))
	copy(authData[:32], rpHash[:])
	authData[32] = flags
	binary.BigEndian.PutUint32(authData[33:37], signCount)

	clientHash := sha256.Sum256(clientDataRaw)
	signed := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(authData), base64.RawURLEncoding.EncodeToString(sig)
}

func spkiB64(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// --- full registration + authentication ceremony ---------------------------

func TestWebAuthnFullCeremony(t *testing.T) {
	h, _, _, tenant := newWebAuthnTestHandlers(t)
	user := "user-" + time.Now().Format("150405.000000000")
	ctx := waAuthedCtx(tenant, user)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}

	// 1) registerBegin
	rr := httptest.NewRecorder()
	h.registerBegin(rr, httptest.NewRequest("POST", "/register/begin", nil).WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("registerBegin code=%d body=%s", rr.Code, rr.Body.String())
	}
	var opts CredentialCreationOptions
	if err := json.Unmarshal(rr.Body.Bytes(), &opts); err != nil {
		t.Fatalf("decode opts: %v", err)
	}
	if opts.RP.ID != waTestRPID || opts.Challenge == "" {
		t.Fatalf("unexpected opts: %+v", opts)
	}

	// 2) registerFinish
	_, cdReg := clientDataJSON(t, "webauthn.create", opts.Challenge, waTestOrigin)
	credID := base64.RawURLEncoding.EncodeToString([]byte("cred-" + user))
	regBody := map[string]any{
		"id":    credID,
		"rawId": credID,
		"type":  "public-key",
		"name":  "My YubiKey",
		"response": map[string]string{
			"clientDataJSON": cdReg,
			"publicKey":      spkiB64(t, &priv.PublicKey),
		},
	}
	rr = doJSON(t, h.registerFinish, ctx, regBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("registerFinish code=%d body=%s", rr.Code, rr.Body.String())
	}

	// 3) list shows the new credential.
	rr = httptest.NewRecorder()
	h.list(rr, httptest.NewRequest("GET", "/credentials", nil).WithContext(ctx))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "My YubiKey") {
		t.Fatalf("list code=%d body=%s", rr.Code, rr.Body.String())
	}

	// 4) loginBegin
	rr = doJSON(t, h.loginBegin, context.Background(), map[string]string{"username": user, "tenant_id": tenant})
	if rr.Code != http.StatusOK {
		t.Fatalf("loginBegin code=%d body=%s", rr.Code, rr.Body.String())
	}
	var reqOpts CredentialRequestOptions
	if err := json.Unmarshal(rr.Body.Bytes(), &reqOpts); err != nil {
		t.Fatalf("decode reqOpts: %v", err)
	}
	if len(reqOpts.AllowedCreds) != 1 || reqOpts.AllowedCreds[0].ID != credID {
		t.Fatalf("loginBegin allowedCreds=%+v", reqOpts.AllowedCreds)
	}

	// 5) loginFinish with a valid assertion (signCount 1 > stored 0).
	cdRaw, cdB64 := clientDataJSON(t, "webauthn.get", reqOpts.Challenge, waTestOrigin)
	authB64, sigB64 := signAssertion(t, priv, waTestRPID, 0x05 /*UP|UV*/, 1, cdRaw)
	loginBody := map[string]any{
		"id":        credID,
		"rawId":     credID,
		"username":  user,
		"tenant_id": tenant,
		"response": map[string]string{
			"clientDataJSON":    cdB64,
			"authenticatorData": authB64,
			"signature":         sigB64,
		},
	}
	rr = doJSON(t, h.loginFinish, context.Background(), loginBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("loginFinish code=%d body=%s", rr.Code, rr.Body.String())
	}
	var lf map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &lf)
	if lf["verified"] != true {
		t.Fatalf("login not verified: %v", lf)
	}

	// 6) replay with same signCount must be rejected as a clone, AND
	// the challenge was consumed so this is also "challenge expired".
	rr = doJSON(t, h.loginFinish, context.Background(), loginBody)
	if rr.Code == http.StatusOK {
		t.Fatalf("expected replay to fail, got 200")
	}

	// 7) delete the credential. The delete path takes the row UUID
	// (the `id` column), not the opaque credential_id, so fetch it
	// from the list response first.
	rr = httptest.NewRecorder()
	h.list(rr, httptest.NewRequest("GET", "/credentials", nil).WithContext(ctx))
	var listResp struct {
		Credentials []WebAuthnCredential `json:"credentials"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil || len(listResp.Credentials) != 1 {
		t.Fatalf("list for delete: err=%v body=%s", err, rr.Body.String())
	}
	rowID := listResp.Credentials[0].ID
	rr = httptest.NewRecorder()
	delReq := httptest.NewRequest("DELETE", "/credentials/"+rowID, nil).WithContext(ctx)
	delReq.SetPathValue("id", rowID)
	h.delete(rr, delReq)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete code=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.list(rr, httptest.NewRequest("GET", "/credentials", nil).WithContext(ctx))
	if strings.Contains(rr.Body.String(), credID) {
		t.Fatalf("credential still listed after delete: %s", rr.Body.String())
	}
}

// doJSON runs an http.HandlerFunc with a JSON body and returns the recorder.
func doJSON(t *testing.T, fn http.HandlerFunc, ctx context.Context, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(string(b)))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	fn(rr, req)
	return rr
}

// --- handler guard clauses (no DB needed) -----------------------------------

func TestWebAuthnHandlerGuards(t *testing.T) {
	h := NewWebAuthnHandlers(WebAuthnConfig{RPID: waTestRPID, RPOrigin: waTestOrigin})

	// registerBegin / registerFinish / list / delete require user ctx.
	for _, tc := range []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"registerBegin", h.registerBegin},
		{"registerFinish", h.registerFinish},
		{"list", h.list},
	} {
		rr := httptest.NewRecorder()
		tc.fn(rr, httptest.NewRequest("POST", "/", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s missing ctx: code=%d want 401", tc.name, rr.Code)
		}
	}

	// registerFinish with bad JSON body.
	rr := doJSON(t, h.registerFinish, waAuthedCtx("t1", "u1"), "not-an-object")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("registerFinish bad json: code=%d want 400", rr.Code)
	}

	// registerFinish missing required fields.
	rr = doJSON(t, h.registerFinish, waAuthedCtx("t1", "u1"), map[string]any{"id": "x"})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("registerFinish missing fields: code=%d want 400", rr.Code)
	}

	// loginBegin missing username/tenant.
	rr = doJSON(t, h.loginBegin, context.Background(), map[string]string{})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("loginBegin missing fields: code=%d want 400", rr.Code)
	}

	// loginFinish missing fields.
	rr = doJSON(t, h.loginFinish, context.Background(), map[string]string{"username": "u"})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("loginFinish missing fields: code=%d want 400", rr.Code)
	}

	// loginFinish with a challenge that was never minted → expired.
	rr = doJSON(t, h.loginFinish, context.Background(), map[string]any{
		"username": "u", "tenant_id": "t", "rawId": "r",
		"response": map[string]string{"clientDataJSON": "x"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("loginFinish no challenge: code=%d want 400", rr.Code)
	}

	// delete without path id.
	rr = httptest.NewRecorder()
	h.delete(rr, httptest.NewRequest("DELETE", "/", nil).WithContext(waAuthedCtx("t1", "u1")))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("delete no id: code=%d want 400", rr.Code)
	}
}

// --- crypto unit tests (no DB needed) ---------------------------------------

func TestVerifyClientDataAssertion(t *testing.T) {
	challenge := []byte("0123456789abcdef0123456789abcdef")
	chB64 := base64.RawURLEncoding.EncodeToString(challenge)

	// Empty origin is a hard failure.
	if _, err := verifyClientDataAssertion("x", challenge, ""); err == nil {
		t.Error("expected error for empty allowedOrigin")
	}

	// Happy path.
	raw, b64 := clientDataJSON(t, "webauthn.get", chB64, waTestOrigin)
	got, err := verifyClientDataAssertion(b64, challenge, waTestOrigin)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("assertion verify: err=%v", err)
	}

	// Wrong type.
	_, bWrongType := clientDataJSON(t, "webauthn.create", chB64, waTestOrigin)
	if _, err := verifyClientDataAssertion(bWrongType, challenge, waTestOrigin); err == nil {
		t.Error("expected type mismatch error")
	}

	// Wrong origin.
	_, bWrongOrigin := clientDataJSON(t, "webauthn.get", chB64, "https://evil.example")
	if _, err := verifyClientDataAssertion(bWrongOrigin, challenge, waTestOrigin); err == nil {
		t.Error("expected origin mismatch error")
	}

	// Challenge mismatch.
	_, bWrongChal := clientDataJSON(t, "webauthn.get", base64.RawURLEncoding.EncodeToString([]byte("nope")), waTestOrigin)
	if _, err := verifyClientDataAssertion(bWrongChal, challenge, waTestOrigin); err == nil {
		t.Error("expected challenge mismatch error")
	}

	// Undecodable base64.
	if _, err := verifyClientDataAssertion("!!!not base64!!!", challenge, waTestOrigin); err == nil {
		t.Error("expected base64 decode error")
	}

	// Registration-side soft check (no type/origin enforcement).
	if err := verifyClientDataChallenge(b64, challenge); err != nil {
		t.Errorf("registration clientData: %v", err)
	}
}

func TestVerifyAssertionSignatureErrors(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub := spkiB64(t, &priv.PublicKey)
	cdRaw := []byte(`{"type":"webauthn.get"}`)

	// Bad public key base64.
	if _, err := verifyAssertionSignature("!!!", "", "", cdRaw, waTestRPID, false); err == nil {
		t.Error("expected pubkey decode error")
	}

	// Valid pubkey but authenticatorData too short.
	shortAuth := base64.RawURLEncoding.EncodeToString([]byte{0x01})
	if _, err := verifyAssertionSignature(pub, shortAuth, "", cdRaw, waTestRPID, false); err == nil {
		t.Error("expected authData too short error")
	}

	// rpIdHash mismatch.
	_, sig := signAssertion(t, priv, "other.rp", 0x01, 1, cdRaw)
	authOther, _ := signAssertion(t, priv, "other.rp", 0x01, 1, cdRaw)
	if _, err := verifyAssertionSignature(pub, authOther, sig, cdRaw, waTestRPID, false); err == nil {
		t.Error("expected rpIdHash mismatch")
	}

	// User-present flag missing (flags=0x00).
	authNoUP, sigNoUP := signAssertion(t, priv, waTestRPID, 0x00, 1, cdRaw)
	if _, err := verifyAssertionSignature(pub, authNoUP, sigNoUP, cdRaw, waTestRPID, false); err == nil {
		t.Error("expected user-present error")
	}

	// requireUV but UV flag missing (flags=UP only).
	authUP, sigUP := signAssertion(t, priv, waTestRPID, 0x01, 1, cdRaw)
	if _, err := verifyAssertionSignature(pub, authUP, sigUP, cdRaw, waTestRPID, true); err == nil {
		t.Error("expected user-verified-required error")
	}

	// Valid ECDSA assertion.
	authOK, sigOK := signAssertion(t, priv, waTestRPID, 0x05, 7, cdRaw)
	count, err := verifyAssertionSignature(pub, authOK, sigOK, cdRaw, waTestRPID, true)
	if err != nil || count != 7 {
		t.Fatalf("valid assertion: count=%d err=%v", count, err)
	}

	// Tampered signature fails.
	if _, err := verifyAssertionSignature(pub, authOK, base64.RawURLEncoding.EncodeToString([]byte("garbagegarbagegarbagegarbage1234")), cdRaw, waTestRPID, true); err == nil {
		t.Error("expected signature invalid")
	}
}

func TestVerifyAssertionSignatureRSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	pub := spkiB64(t, &priv.PublicKey)
	cdRaw := []byte(`{"type":"webauthn.get"}`)

	authData := make([]byte, 37)
	rpHash := sha256.Sum256([]byte(waTestRPID))
	copy(authData[:32], rpHash[:])
	authData[32] = 0x05
	binary.BigEndian.PutUint32(authData[33:37], 3)
	clientHash := sha256.Sum256(cdRaw)
	signed := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("rsa sign: %v", err)
	}
	authB64 := base64.RawURLEncoding.EncodeToString(authData)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	count, err := verifyAssertionSignature(pub, authB64, sigB64, cdRaw, waTestRPID, false)
	if err != nil || count != 3 {
		t.Fatalf("rsa assertion: count=%d err=%v", count, err)
	}
}

func TestDecodeBase64Loose(t *testing.T) {
	want := []byte("hello-webauthn")
	for _, enc := range []string{
		base64.RawURLEncoding.EncodeToString(want),
		base64.URLEncoding.EncodeToString(want),
		base64.RawStdEncoding.EncodeToString(want),
		base64.StdEncoding.EncodeToString(want),
	} {
		got, err := decodeBase64Loose(enc)
		if err != nil || string(got) != string(want) {
			t.Errorf("decodeBase64Loose(%q)=%q err=%v", enc, got, err)
		}
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "fallback") != "fallback" {
		t.Error("empty should use fallback")
	}
	if orDefault("set", "fallback") != "set" {
		t.Error("non-empty should be kept")
	}
}
