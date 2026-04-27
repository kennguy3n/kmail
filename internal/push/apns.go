// Package push — Apple Push Notification service (APNs) transport.
//
// APNsTransport implements the Transport interface using Apple's
// HTTP/2 push API with token-based authentication (p8). The token
// is a short-lived ES256 JWT signed with the AuthKey_<KeyID>.p8
// downloaded from the Apple Developer portal. The same token is
// reused for up to 50 minutes (Apple's documented refresh window
// is 60 minutes; we refresh at 50 to leave headroom).
//
// Wire-level details:
//
//   - Endpoint: https://api.push.apple.com (production) or
//     https://api.development.push.apple.com (sandbox).
//   - Method:   POST /3/device/{device_token}
//   - Headers:  apns-topic (bundle ID), authorization (bearer JWT),
//               apns-push-type (alert / background / voip),
//               apns-priority (10 = immediate, 5 = power-aware).
//   - Body:     JSON `aps` payload + custom keys.
//
// Apple returns 200 on accept; non-200 includes a JSON
// `{"reason":"BadDeviceToken"}` body. We surface that reason
// verbatim through the returned error so the push fan-out worker
// can prune dead tokens.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// APNsConfig wires NewAPNsTransport.
type APNsConfig struct {
	// KeyID is the 10-character APNs auth key identifier (the
	// suffix on AuthKey_<KeyID>.p8).
	KeyID string
	// TeamID is the 10-character Apple Developer team identifier
	// that owns the key.
	TeamID string
	// KeyPath points at the AuthKey_<KeyID>.p8 file. The file
	// contents (PEM-encoded PKCS#8 EC private key) are read once
	// at startup; rotation requires a service restart.
	KeyPath string
	// Topic is the iOS bundle identifier the token is provisioned
	// for (e.g. "com.kchat.kmail").
	Topic string
	// Endpoint overrides the API host. Defaults to the production
	// host; tests use httptest's URL.
	Endpoint string
	// HTTPClient is the HTTP/2-capable client used for requests.
	// Optional — defaults to http.DefaultClient with a 10s timeout.
	HTTPClient *http.Client
	// Logger is the diagnostic logger. Defaults to log.Default().
	Logger *log.Logger
	// Now overrides time.Now for deterministic JWT iat claims.
	Now func() time.Time
}

// APNsTransport sends push notifications through APNs.
type APNsTransport struct {
	cfg     APNsConfig
	signKey *ecdsa.PrivateKey

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

// apnsTokenTTL is how long we reuse a signed JWT before re-signing.
// Apple rejects tokens older than 60 minutes; we refresh at 50.
const apnsTokenTTL = 50 * time.Minute

// NewAPNsTransport returns an APNsTransport, loading and parsing
// the p8 key from disk. Returns an error if the key file is
// missing, malformed, or not an ECDSA P-256 key.
func NewAPNsTransport(cfg APNsConfig) (*APNsTransport, error) {
	if cfg.KeyID == "" || cfg.TeamID == "" || cfg.KeyPath == "" {
		return nil, errors.New("apns: KeyID, TeamID, and KeyPath are required")
	}
	if cfg.Topic == "" {
		return nil, errors.New("apns: Topic (bundle identifier) is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.push.apple.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	keyBytes, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("apns: read key: %w", err)
	}
	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return nil, errors.New("apns: key is not PEM-encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parse PKCS#8 key: %w", err)
	}
	ec, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: key is not an ECDSA private key")
	}
	return &APNsTransport{cfg: cfg, signKey: ec}, nil
}

// Send delivers a single notification via APNs. Returns nil on
// 200, a wrapped error on every other status. The error string
// includes Apple's `reason` field so callers can prune dead
// tokens (`BadDeviceToken`, `Unregistered`).
func (t *APNsTransport) Send(ctx context.Context, sub Subscription, n Notification) error {
	if sub.PushEndpoint == "" {
		return errors.New("apns: subscription missing push_endpoint (device token)")
	}
	token, err := t.token()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{
				"title": n.Title,
				"body":  n.Body,
			},
			"sound": "default",
		},
	}
	if n.Kind != "" {
		payload["kind"] = n.Kind
	}
	if len(n.Data) > 0 {
		payload["data"] = n.Data
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("apns: marshal payload: %w", err)
	}
	url := fmt.Sprintf("%s/3/device/%s", t.cfg.Endpoint, sub.PushEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("apns: new request: %w", err)
	}
	req.Header.Set("authorization", "bearer "+token)
	req.Header.Set("apns-topic", t.cfg.Topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")
	resp, err := t.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("apns: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var parsed struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	if parsed.Reason != "" {
		return fmt.Errorf("apns: %d %s", resp.StatusCode, parsed.Reason)
	}
	return fmt.Errorf("apns: %d %s", resp.StatusCode, string(respBody))
}

// token returns a cached JWT or re-signs one if the cached token
// is older than apnsTokenTTL.
func (t *APNsTransport) token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.cfg.Now()
	if t.cached != "" && now.Sub(t.cachedAt) < apnsTokenTTL {
		return t.cached, nil
	}
	claims := jwt.MapClaims{
		"iss": t.cfg.TeamID,
		"iat": now.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = t.cfg.KeyID
	signed, err := tok.SignedString(t.signKey)
	if err != nil {
		return "", fmt.Errorf("apns: sign jwt: %w", err)
	}
	t.cached = signed
	t.cachedAt = now
	return signed, nil
}
