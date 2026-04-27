package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateVAPIDKeysRoundTrip(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	if pubB64 == "" || privB64 == "" {
		t.Fatal("empty key output")
	}
	if _, err := NewWebPushFromKeys(pubB64, privB64, "mailto:ops@example.test", nil); err != nil {
		t.Fatalf("NewWebPushFromKeys: %v", err)
	}
}

func TestWebPushSendNotificationOnly(t *testing.T) {
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	transport, err := NewWebPushFromKeys(pub, priv, "mailto:ops@example.test", nil)
	if err != nil {
		t.Fatalf("NewWebPushFromKeys: %v", err)
	}

	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	sub := Subscription{DeviceType: "web", PushEndpoint: srv.URL + "/wp/abc"}
	if err := transport.Send(context.Background(), sub, Notification{Title: "hi", Body: "world", Kind: "new_email"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if captured == nil {
		t.Fatal("push service did not receive a request")
	}
	auth := captured.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid t=") || !strings.Contains(auth, ", k=") {
		t.Fatalf("Authorization missing VAPID format: %q", auth)
	}
	if captured.Header.Get("TTL") == "" {
		t.Fatal("TTL header missing")
	}
}

func TestWebPushSendEncryptedPayload(t *testing.T) {
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	transport, err := NewWebPushFromKeys(pub, priv, "mailto:ops@example.test", nil)
	if err != nil {
		t.Fatalf("NewWebPushFromKeys: %v", err)
	}

	// User agent's keypair (simulated). The transport just needs
	// the public point + auth secret; decryption is the browser's
	// problem.
	uaPub, _, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("ua keys: %v", err)
	}
	authSecret := "xx_ZP1jmZ2NSF03Cv9PR9w"

	var capturedCT, capturedCE string
	var capturedLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCT = r.Header.Get("Content-Type")
		capturedCE = r.Header.Get("Content-Encoding")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		capturedLen = n
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	sub := Subscription{
		DeviceType:   "web",
		PushEndpoint: srv.URL + "/wp/xyz",
		P256DHKey:    uaPub,
		AuthKey:      authSecret,
	}
	if err := transport.Send(context.Background(), sub, Notification{Title: "encrypted", Body: "ok", Kind: "new_email"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if capturedCE != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q, want aes128gcm", capturedCE)
	}
	if capturedCT != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", capturedCT)
	}
	if capturedLen < 80 {
		t.Fatalf("ciphertext too short: %d bytes", capturedLen)
	}
}

func TestWebPushSubscriptionGone(t *testing.T) {
	pub, priv, _ := GenerateVAPIDKeys()
	transport, _ := NewWebPushFromKeys(pub, priv, "mailto:ops@example.test", nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()
	err := transport.Send(context.Background(), Subscription{DeviceType: "web", PushEndpoint: srv.URL}, Notification{})
	if err != ErrSubscriptionGone {
		t.Fatalf("err = %v, want ErrSubscriptionGone", err)
	}
}
