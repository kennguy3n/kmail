package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeP8Key generates an ECDSA P-256 private key and writes it as
// PKCS#8 PEM to a temp file.
func writeP8Key(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	dir := t.TempDir()
	p := filepath.Join(dir, "AuthKey_TESTKEY01.p8")
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestAPNsTransport_Send(t *testing.T) {
	keyPath := writeP8Key(t)
	var capturedAuth, capturedTopic, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("authorization")
		capturedTopic = r.Header.Get("apns-topic")
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tr, err := NewAPNsTransport(APNsConfig{
		KeyID:    "TESTKEY01",
		TeamID:   "TESTTEAM01",
		KeyPath:  keyPath,
		Topic:    "com.kchat.kmail",
		Endpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewAPNsTransport: %v", err)
	}
	sub := Subscription{ID: "s1", DeviceType: "ios", PushEndpoint: "abc123"}
	err = tr.Send(context.Background(), sub, Notification{Title: "Hi", Body: "hey", Kind: "new_email"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(capturedAuth, "bearer ") {
		t.Errorf("missing bearer token: %q", capturedAuth)
	}
	if capturedTopic != "com.kchat.kmail" {
		t.Errorf("topic=%q", capturedTopic)
	}
	if capturedPath != "/3/device/abc123" {
		t.Errorf("path=%q", capturedPath)
	}
}

func TestAPNsTransport_BadStatus(t *testing.T) {
	keyPath := writeP8Key(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
	}))
	defer srv.Close()
	tr, err := NewAPNsTransport(APNsConfig{
		KeyID:    "K",
		TeamID:   "T",
		KeyPath:  keyPath,
		Topic:    "com.kchat.kmail",
		Endpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewAPNsTransport: %v", err)
	}
	err = tr.Send(context.Background(), Subscription{PushEndpoint: "tok"}, Notification{})
	if err == nil || !strings.Contains(err.Error(), "Unregistered") {
		t.Fatalf("expected Unregistered err, got %v", err)
	}
}

// writeFCMServiceAccount writes a plausible service account JSON
// (with a generated RSA key) to a temp file and returns the path.
func writeFCMServiceAccount(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa gen: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(priv)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	acct := map[string]any{
		"type":         "service_account",
		"project_id":   "kmail-test",
		"client_email": "fcm@kmail-test.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
	}
	raw, _ := json.Marshal(acct)
	dir := t.TempDir()
	p := filepath.Join(dir, "fcm.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestFCMTransport_Send(t *testing.T) {
	credPath := writeFCMServiceAccount(t)
	var sendBody []byte
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenSrv.Close()
	fcmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sendBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer fcmSrv.Close()
	tr, err := NewFCMTransport(FCMConfig{
		CredentialsPath: credPath,
		Endpoint:        fcmSrv.URL,
		TokenEndpoint:   tokenSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewFCMTransport: %v", err)
	}
	err = tr.Send(context.Background(), Subscription{DeviceType: "android", PushEndpoint: "fcm-tok"}, Notification{Title: "T", Body: "B", Kind: "new_email"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(string(sendBody), "fcm-tok") {
		t.Errorf("body missing token: %s", sendBody)
	}
	if !strings.Contains(string(sendBody), `"kind":"new_email"`) {
		t.Errorf("body missing kind: %s", sendBody)
	}
}

func TestFCMTransport_TokenCached(t *testing.T) {
	credPath := writeFCMServiceAccount(t)
	tokenCalls := 0
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls++
		_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenSrv.Close()
	fcmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fcmSrv.Close()
	tr, err := NewFCMTransport(FCMConfig{
		CredentialsPath: credPath,
		Endpoint:        fcmSrv.URL,
		TokenEndpoint:   tokenSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewFCMTransport: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := tr.Send(context.Background(), Subscription{PushEndpoint: "tok"}, Notification{Title: "x"}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if tokenCalls != 1 {
		t.Errorf("tokenCalls=%d want 1 (cached)", tokenCalls)
	}
}

type recordingTransport struct {
	calls []string
}

func (r *recordingTransport) Send(_ context.Context, sub Subscription, _ Notification) error {
	r.calls = append(r.calls, sub.DeviceType)
	return nil
}

func TestTransportRouter_Dispatch(t *testing.T) {
	ios := &recordingTransport{}
	android := &recordingTransport{}
	web := &recordingTransport{}
	def := &recordingTransport{}
	r := &TransportRouter{IOS: ios, Android: android, Web: web, Default: def}
	for _, dt := range []string{"ios", "android", "web", "weird"} {
		_ = r.Send(context.Background(), Subscription{DeviceType: dt, PushEndpoint: "x"}, Notification{Title: "t"})
	}
	if len(ios.calls) != 1 || ios.calls[0] != "ios" {
		t.Errorf("ios=%v", ios.calls)
	}
	if len(android.calls) != 1 || android.calls[0] != "android" {
		t.Errorf("android=%v", android.calls)
	}
	if len(web.calls) != 1 || web.calls[0] != "web" {
		t.Errorf("web=%v", web.calls)
	}
	if len(def.calls) != 1 || def.calls[0] != "weird" {
		t.Errorf("default=%v", def.calls)
	}
}

func TestTransportRouter_NoTransport(t *testing.T) {
	r := &TransportRouter{}
	err := r.Send(context.Background(), Subscription{DeviceType: "ios", PushEndpoint: "x"}, Notification{})
	if err == nil {
		t.Fatalf("expected error when no transport configured")
	}
}

// silence unused-import warnings if any.
var _ = time.Second
