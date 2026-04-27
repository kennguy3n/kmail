package middleware

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGenerateHOTPRFC4226(t *testing.T) {
	// Test vectors from RFC 4226 appendix D.
	secret := []byte("12345678901234567890")
	want := []string{
		"755224", "287082", "359152", "969429", "338314",
	}
	for i, w := range want {
		got := generateHOTP(secret, int64(i))
		if got != w {
			t.Errorf("HOTP[%d] = %s, want %s", i, got, w)
		}
	}
}

func TestVerifyCodeAcceptsCurrentAndNeighbouring(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(59, 0)
	step := now.Unix() / 30
	good := generateHOTP(secret, step)
	if !verifyCode(secret, good, now) {
		t.Fatal("current step rejected")
	}
	prev := generateHOTP(secret, step-1)
	if !verifyCode(secret, prev, now) {
		t.Fatal("previous step rejected")
	}
	old := generateHOTP(secret, step-5)
	if verifyCode(secret, old, now) {
		t.Fatal("ancient step accepted")
	}
}

func TestRecoveryCodeRoundTrip(t *testing.T) {
	codes, hashed, err := newRecoveryCodes(5)
	if err != nil {
		t.Fatalf("newRecoveryCodes: %v", err)
	}
	if len(codes) != 5 {
		t.Fatalf("got %d codes", len(codes))
	}
	updated, ok := consumeRecoveryCode(hashed, codes[2])
	if !ok {
		t.Fatal("expected to consume code")
	}
	// Same code does not work twice.
	if _, ok := consumeRecoveryCode(updated, codes[2]); ok {
		t.Fatal("recovery code re-usable")
	}
	if _, ok := consumeRecoveryCode(updated, "BOGUS-BOGUS"); ok {
		t.Fatal("bogus code accepted")
	}
}

func TestOtpauthURIShape(t *testing.T) {
	h := NewTOTPHandlers(TOTPConfig{Issuer: "KMail"})
	secret := make([]byte, 20)
	for i := range secret {
		secret[i] = byte(i)
	}
	uri := h.otpauthURI("00000000-0000-0000-0000-000000000001", "user-1", secret)
	for _, want := range []string{"otpauth://totp/", "issuer=KMail", "digits=6", "period=30"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("URI missing %q: %s", want, uri)
		}
	}
}

// keep sha1 + binary referenced even when rest of suite is light
var _ = sha1.New
var _ = hmac.Equal
var _ = binary.BigEndian
var _ = fmt.Sprintf
