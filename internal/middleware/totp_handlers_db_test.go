package middleware

import (
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func totpReq(method, tenant, user, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/x", nil)
	} else {
		r = httptest.NewRequest(method, "/x", strings.NewReader(body))
	}
	r.Header.Set("X-KMail-Dev-Tenant-Id", tenant)
	r.Header.Set("X-KMail-Dev-User-Id", user)
	return r
}

func TestTOTPHandlersLifecycleDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	user := "user-totp-1"
	fixedNow := time.Unix(1_700_000_000, 0)
	h := NewTOTPHandlers(TOTPConfig{Pool: pool, Now: func() time.Time { return fixedNow }})

	// status before enrollment → enrolled=false
	rec := httptest.NewRecorder()
	h.status(rec, totpReq(http.MethodGet, tenant, user, ""))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"enrolled":true`) {
		t.Fatalf("pre-enroll status=%d body=%s", rec.Code, rec.Body.String())
	}

	// enroll → returns secret + otpauth URI
	rec = httptest.NewRecorder()
	h.enroll(rec, totpReq(http.MethodPost, tenant, user, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll=%d body=%s", rec.Code, rec.Body.String())
	}
	var er EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(er.Secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}

	// status after enroll → enrolled=true, enabled=false
	rec = httptest.NewRecorder()
	h.status(rec, totpReq(http.MethodGet, tenant, user, ""))
	if !strings.Contains(rec.Body.String(), `"enrolled":true`) || strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("post-enroll status body=%s", rec.Body.String())
	}

	// verify with a valid TOTP code → 200 with recovery codes
	code := generateHOTP(secret, fixedNow.Unix()/30)
	rec = httptest.NewRecorder()
	h.verify(rec, totpReq(http.MethodPost, tenant, user, `{"code":"`+code+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("verify=%d body=%s", rec.Code, rec.Body.String())
	}
	var vr VerifyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &vr); err != nil || len(vr.RecoveryCodes) != 10 {
		t.Fatalf("verify recovery codes=%v err=%v", vr.RecoveryCodes, err)
	}

	// check with a valid TOTP code → verified via totp
	rec = httptest.NewRecorder()
	h.check(rec, totpReq(http.MethodPost, tenant, user, `{"code":"`+code+`"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"method":"totp"`) {
		t.Fatalf("check totp=%d body=%s", rec.Code, rec.Body.String())
	}

	// check with a recovery code → verified via recovery
	rec = httptest.NewRecorder()
	h.check(rec, totpReq(http.MethodPost, tenant, user, `{"code":"`+vr.RecoveryCodes[0]+`"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"method":"recovery"`) {
		t.Fatalf("check recovery=%d body=%s", rec.Code, rec.Body.String())
	}

	// check with a wrong code → 401
	rec = httptest.NewRecorder()
	h.check(rec, totpReq(http.MethodPost, tenant, user, `{"code":"000000"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("check wrong=%d want 401", rec.Code)
	}

	// disable requires the current second factor for an enabled
	// credential (closes the delete-then-re-enroll bypass): a valid
	// TOTP code → 204, then status enrolled=false.
	rec = httptest.NewRecorder()
	h.disable(rec, totpReq(http.MethodDelete, tenant, user, `{"code":"`+code+`"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disable=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.status(rec, totpReq(http.MethodGet, tenant, user, ""))
	if strings.Contains(rec.Body.String(), `"enrolled":true`) {
		t.Errorf("post-disable status body=%s want enrolled=false", rec.Body.String())
	}
}

func TestTOTPHandlersErrorsDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	h := NewTOTPHandlers(TOTPConfig{Pool: pool})

	// no identity → 401
	rec := httptest.NewRecorder()
	h.enroll(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("enroll no identity=%d want 401", rec.Code)
	}

	// verify with malformed body → 400
	rec = httptest.NewRecorder()
	h.verify(rec, totpReq(http.MethodPost, tenant, "u1", `{bad`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("verify bad body=%d want 400", rec.Code)
	}

	// verify when not enrolled → 400
	rec = httptest.NewRecorder()
	h.verify(rec, totpReq(http.MethodPost, tenant, "u-none", `{"code":"123456"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("verify not enrolled=%d want 400", rec.Code)
	}

	// check when not enabled → 401
	rec = httptest.NewRecorder()
	h.check(rec, totpReq(http.MethodPost, tenant, "u-none", `{"code":"123456"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("check not enabled=%d want 401", rec.Code)
	}
}
