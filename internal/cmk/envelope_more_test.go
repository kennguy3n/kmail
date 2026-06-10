package cmk

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestHasMagic(t *testing.T) {
	if HasMagic([]byte("short")) {
		t.Error("short blob should not have magic")
	}
	env := newTestEnvelope(t)
	wrapped, err := env.Wrap([]byte("secret"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if !HasMagic(wrapped) {
		t.Error("wrapped blob should carry magic prefix")
	}
	if HasMagic(bytes.Repeat([]byte{0xAB}, 32)) {
		t.Error("random bytes should not match magic")
	}
}

func TestLoadEnvelope(t *testing.T) {
	t.Setenv("KMAIL_SECRETS_KEY", "")
	if _, err := LoadEnvelope(); err == nil {
		t.Error("LoadEnvelope with unset key should error")
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("KMAIL_SECRETS_KEY", hex.EncodeToString(key))
	env, err := LoadEnvelope()
	if err != nil || env == nil {
		t.Fatalf("LoadEnvelope hex: env=%v err=%v", env, err)
	}
}

func TestNewAESGCMEnvelopeFromKeyMaterial(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	for name, material := range map[string]string{
		"hex":        hex.EncodeToString(key),
		"base64std":  base64.StdEncoding.EncodeToString(key),
		"base64url":  base64.URLEncoding.EncodeToString(key),
		"rawbase64":  base64.RawStdEncoding.EncodeToString(key),
	} {
		if _, err := NewAESGCMEnvelopeFromKeyMaterial(material); err != nil {
			t.Errorf("%s material rejected: %v", name, err)
		}
	}
	for _, bad := range []string{"", "tooshort", hex.EncodeToString(make([]byte, 16))} {
		if _, err := NewAESGCMEnvelopeFromKeyMaterial(bad); err == nil {
			t.Errorf("bad material %q accepted", bad)
		}
	}
}

func TestNoopEnvelope(t *testing.T) {
	var e NoopEnvelope
	w, err := e.Wrap([]byte("plain"))
	if err != nil || string(w) != "plain" {
		t.Fatalf("Noop Wrap=%q err=%v", w, err)
	}
	u, enc, err := e.Unwrap([]byte("plain"))
	if err != nil || enc || string(u) != "plain" {
		t.Fatalf("Noop Unwrap=%q enc=%v err=%v", u, enc, err)
	}
}
