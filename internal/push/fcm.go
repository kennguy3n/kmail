// Package push — Firebase Cloud Messaging (FCM) HTTP v1 transport.
//
// FCMTransport implements the Transport interface using the
// Firebase HTTP v1 API. Authentication is OAuth2 via a service
// account: we sign a JWT with the service account private key,
// exchange it for an access token at Google's token endpoint, and
// cache the token until it expires (typically 1 hour).
//
// Wire-level details:
//
//   - Endpoint: https://fcm.googleapis.com/v1/projects/{project_id}/messages:send
//   - Method:   POST
//   - Headers:  Authorization (Bearer <access_token>),
//               Content-Type (application/json).
//   - Body:     {"message": {"token": "<device>", "notification":
//               {"title": "...", "body": "..."}, "data": {...}}}
//
// Failure handling: a 404 / 401 / 403 from FCM with reason
// "registration-token-not-registered" indicates a stale token
// that should be pruned. The error includes the upstream reason
// so the push fan-out worker can react.
package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// FCMConfig wires NewFCMTransport.
type FCMConfig struct {
	// CredentialsPath points at the service-account JSON
	// downloaded from Google Cloud (Firebase project →
	// Project settings → Service accounts → Generate new
	// private key).
	CredentialsPath string
	// HTTPClient is the HTTP client used for both the OAuth2
	// token exchange and the FCM send call. Optional — defaults
	// to http.DefaultClient with a 10s timeout.
	HTTPClient *http.Client
	// Endpoint overrides the FCM API host. Defaults to
	// https://fcm.googleapis.com.
	Endpoint string
	// TokenEndpoint overrides the OAuth2 token endpoint. Tests
	// point this at httptest.NewServer.
	TokenEndpoint string
	// Logger is the diagnostic logger. Defaults to log.Default().
	Logger *log.Logger
	// Now overrides time.Now for deterministic JWT iat/exp.
	Now func() time.Time
}

// fcmServiceAccount is the on-disk JSON shape Google emits for a
// service-account key.
type fcmServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// FCMTransport sends push notifications through FCM HTTP v1.
type FCMTransport struct {
	cfg     FCMConfig
	account fcmServiceAccount
	signKey *rsa.PrivateKey

	// mu guards the cached access token / expiry. It is held
	// only while reading or writing those two fields — never
	// during the OAuth2 HTTP round trip.
	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	// refreshMu serialises concurrent token refreshes so only
	// one OAuth2 call is in flight at a time. Send() callers
	// that find an unexpired cached token never touch it.
	refreshMu sync.Mutex
}

// fcmScope is the OAuth2 scope FCM HTTP v1 requires.
const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// NewFCMTransport returns an FCMTransport, loading and parsing
// the service-account JSON. Returns an error if the file is
// missing, malformed, or carries an invalid private key.
func NewFCMTransport(cfg FCMConfig) (*FCMTransport, error) {
	if cfg.CredentialsPath == "" {
		return nil, errors.New("fcm: CredentialsPath is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://fcm.googleapis.com"
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
	raw, err := os.ReadFile(cfg.CredentialsPath)
	if err != nil {
		return nil, fmt.Errorf("fcm: read credentials: %w", err)
	}
	var acct fcmServiceAccount
	if err := json.Unmarshal(raw, &acct); err != nil {
		return nil, fmt.Errorf("fcm: parse credentials: %w", err)
	}
	if acct.ClientEmail == "" || acct.PrivateKey == "" || acct.ProjectID == "" {
		return nil, errors.New("fcm: credentials missing client_email, private_key, or project_id")
	}
	block, _ := pem.Decode([]byte(acct.PrivateKey))
	if block == nil {
		return nil, errors.New("fcm: private_key is not PEM-encoded")
	}
	var rsaKey *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		rsaKey, ok = k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("fcm: private_key is not RSA")
		}
	} else if k2, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		rsaKey = k2
	} else {
		return nil, fmt.Errorf("fcm: parse private_key: %w", err)
	}
	if cfg.TokenEndpoint == "" {
		cfg.TokenEndpoint = acct.TokenURI
		if cfg.TokenEndpoint == "" {
			cfg.TokenEndpoint = "https://oauth2.googleapis.com/token"
		}
	}
	return &FCMTransport{cfg: cfg, account: acct, signKey: rsaKey}, nil
}

// Send delivers a single notification via FCM. Returns nil on
// 200, a wrapped error on every other status. Stale-token
// reasons are surfaced verbatim through the error string.
func (t *FCMTransport) Send(ctx context.Context, sub Subscription, n Notification) error {
	if sub.PushEndpoint == "" {
		return errors.New("fcm: subscription missing push_endpoint (registration token)")
	}
	access, err := t.accessTokenLocked(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"message": map[string]any{
			"token": sub.PushEndpoint,
			"notification": map[string]any{
				"title": n.Title,
				"body":  n.Body,
			},
		},
	}
	msg := payload["message"].(map[string]any)
	data := map[string]string{}
	for k, v := range n.Data {
		data[k] = v
	}
	if n.Kind != "" {
		data["kind"] = n.Kind
	}
	if len(data) > 0 {
		msg["data"] = data
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("fcm: marshal payload: %w", err)
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/messages:send", t.cfg.Endpoint, t.account.ProjectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fcm: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var parsed struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	if parsed.Error.Status != "" {
		reason := parsed.Error.Status
		if len(parsed.Error.Details) > 0 && parsed.Error.Details[0].ErrorCode != "" {
			reason = parsed.Error.Details[0].ErrorCode
		}
		return fmt.Errorf("fcm: %d %s: %s", resp.StatusCode, reason, parsed.Error.Message)
	}
	return fmt.Errorf("fcm: %d %s", resp.StatusCode, string(respBody))
}

// accessTokenLocked returns a cached OAuth2 access token, or
// fetches a fresh one if the cached value is missing / expired.
//
// The token cache is guarded by t.mu, which is held only while
// reading / writing the two cache fields. The OAuth2 HTTP
// exchange runs OUTSIDE t.mu under a separate t.refreshMu so a
// slow refresh does not serialise concurrent Send() callers that
// only need to read an already-valid token. refreshMu also
// coalesces concurrent refreshes — only one round trip is in
// flight at a time, even when many goroutines simultaneously
// observe an expired token.
func (t *FCMTransport) accessTokenLocked(ctx context.Context) (string, error) {
	// Fast path: cached token still valid.
	if tok, ok := t.cachedToken(); ok {
		return tok, nil
	}
	// Slow path: serialise refreshes.
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	// Re-check the cache: another goroutine may have refreshed
	// while we were waiting for refreshMu.
	if tok, ok := t.cachedToken(); ok {
		return tok, nil
	}
	now := t.cfg.Now()
	jwtStr, err := t.signServiceJWT(now)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwtStr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("fcm: token req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm: token do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fcm: token %d: %s", resp.StatusCode, string(respBody))
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return "", fmt.Errorf("fcm: token unmarshal: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("fcm: empty access_token")
	}
	ttl := time.Duration(tokenResp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	// Refresh 60s before expiry to absorb clock skew.
	t.mu.Lock()
	t.accessToken = tokenResp.AccessToken
	t.expiresAt = now.Add(ttl - 60*time.Second)
	token := t.accessToken
	t.mu.Unlock()
	return token, nil
}

// cachedToken returns the cached access token if it is still
// valid for the current clock. The caller must NOT hold t.mu.
func (t *FCMTransport) cachedToken() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.accessToken != "" && t.cfg.Now().Before(t.expiresAt) {
		return t.accessToken, true
	}
	return "", false
}

// signServiceJWT signs the JWT assertion exchanged at the OAuth2
// token endpoint for an access token.
func (t *FCMTransport) signServiceJWT(now time.Time) (string, error) {
	claims := jwt.MapClaims{
		"iss":   t.account.ClientEmail,
		"scope": fcmScope,
		"aud":   t.cfg.TokenEndpoint,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(t.signKey)
	if err != nil {
		return "", fmt.Errorf("fcm: sign jwt: %w", err)
	}
	return signed, nil
}
