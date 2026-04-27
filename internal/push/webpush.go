// Package push — RFC 8030 Web Push transport with VAPID (RFC 8292).
//
// The Web Push protocol delivers a message to the client's push
// service (Mozilla autopush, Firebase for Chrome, Apple's Web
// Push for Safari, …) via an HTTPS POST to the subscription's
// `endpoint`. RFC 8292 (VAPID) requires the POST to carry a
// signed JWT identifying the application server and an `aes128gcm`
// payload encrypted to the user agent's `p256dh` + `auth` keys.
//
// KMail's Web Push transport supports two modes:
//
//   - Notification-only: the JWT alone is sent (`Authorization:
//     vapid t=<jwt>, k=<pub>`), no payload. Clients receive a
//     `push` event with empty data and fetch fresh content via
//     the existing JMAP API.
//   - Encrypted payload: when the subscription carries `auth_key`
//     and `p256dh_key`, the notification body is encrypted with
//     RFC 8291 aes128gcm. Clients can render the body without an
//     immediate JMAP round-trip.
//
// Configuration:
//
//   KMAIL_VAPID_PUBLIC_KEY  — uncompressed P-256 point, base64url.
//   KMAIL_VAPID_PRIVATE_KEY — P-256 scalar, base64url.
//   KMAIL_VAPID_SUBJECT     — `mailto:` or `https://` URL identifying the operator.
//
// Generate a fresh keypair with:
//
//   openssl ecparam -name prime256v1 -genkey -noout -out vapid.pem
//   openssl ec -in vapid.pem -pubout -outform DER | tail -c 65 | base64 | tr +/ -_ | tr -d '='
//
// (or call `GenerateVAPIDKeys()` below from a cmd/util once.)
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebPushConfig wires the Web Push transport.
type WebPushConfig struct {
	// PrivateKey is the application server private key (P-256
	// scalar). When empty the transport short-circuits to a
	// logging no-op.
	PrivateKey *ecdsa.PrivateKey
	// PublicKey is the matching public key. The base64url-encoded
	// uncompressed point ends up in the `Crypto-Key` header /
	// VAPID `k=` parameter.
	PublicKey *ecdsa.PublicKey
	// Subject is the VAPID claim — operator contact, e.g.
	// `mailto:ops@kmail.example`.
	Subject string
	// HTTP overrides the HTTP client. Defaults to a 15s-timeout client.
	HTTP *http.Client
	// Logger captures non-fatal errors (4xx from the push service).
	Logger *log.Logger
	// TTL is the max lifetime of an undelivered notification at
	// the push service. Defaults to 86400s (24h) which matches
	// Mozilla autopush's recommended max.
	TTL int
}

// WebPushTransport is the Transport implementation. Wired by
// buildPushTransport in cmd/kmail-api/main.go as router.Web.
type WebPushTransport struct {
	cfg WebPushConfig
}

// NewWebPushTransport returns a transport. Use NewWebPushFromEnv
// for the typical KMAIL_VAPID_* env-driven path.
func NewWebPushTransport(cfg WebPushConfig) *WebPushTransport {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.TTL == 0 {
		cfg.TTL = 86400
	}
	return &WebPushTransport{cfg: cfg}
}

// NewWebPushFromKeys decodes the VAPID public + private keys from
// their base64url string forms and returns a configured transport.
// Returns an error if either key is missing or malformed.
func NewWebPushFromKeys(publicKey, privateKey, subject string, logger *log.Logger) (*WebPushTransport, error) {
	if publicKey == "" || privateKey == "" {
		return nil, errors.New("push: KMAIL_VAPID_PUBLIC_KEY and KMAIL_VAPID_PRIVATE_KEY required")
	}
	pub, err := decodeVAPIDPublic(publicKey)
	if err != nil {
		return nil, fmt.Errorf("push: parse VAPID public key: %w", err)
	}
	priv, err := decodeVAPIDPrivate(privateKey, pub)
	if err != nil {
		return nil, fmt.Errorf("push: parse VAPID private key: %w", err)
	}
	if subject == "" {
		subject = "mailto:ops@kmail.invalid"
	}
	return NewWebPushTransport(WebPushConfig{
		PrivateKey: priv,
		PublicKey:  pub,
		Subject:    subject,
		Logger:     logger,
	}), nil
}

// Send delivers the notification per RFC 8030. The flow:
//   1. Build a VAPID JWT signed with the application server's
//      ECDSA P-256 private key (RFC 8292).
//   2. If the subscription has p256dh + auth, encrypt the JSON
//      payload per RFC 8291 (aes128gcm).
//   3. POST to sub.PushEndpoint with the appropriate headers.
func (t *WebPushTransport) Send(ctx context.Context, sub Subscription, n Notification) error {
	if t == nil || t.cfg.PrivateKey == nil || t.cfg.PublicKey == nil {
		return errors.New("push: WebPushTransport not configured")
	}
	if sub.PushEndpoint == "" {
		return errors.New("push: subscription missing endpoint")
	}
	endpointURL, err := url.Parse(sub.PushEndpoint)
	if err != nil {
		return fmt.Errorf("push: invalid endpoint: %w", err)
	}
	jwt, err := t.signVAPIDJWT(endpointURL)
	if err != nil {
		return fmt.Errorf("push: sign VAPID JWT: %w", err)
	}
	pubB64 := encodeUncompressed(t.cfg.PublicKey)

	var (
		body        []byte
		contentType string
	)
	if sub.P256DHKey != "" && sub.AuthKey != "" {
		payload, err := json.Marshal(n)
		if err != nil {
			return fmt.Errorf("push: marshal notification: %w", err)
		}
		body, err = encryptAES128GCM(payload, sub.P256DHKey, sub.AuthKey)
		if err != nil {
			return fmt.Errorf("push: encrypt payload: %w", err)
		}
		contentType = "application/octet-stream"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.PushEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("TTL", fmt.Sprintf("%d", t.cfg.TTL))
	req.Header.Set("Authorization", fmt.Sprintf("vapid t=%s, k=%s", jwt, pubB64))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Content-Encoding", "aes128gcm")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	}
	resp, err := t.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("push: POST %s: %w", endpointURL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		return ErrSubscriptionGone
	}
	return fmt.Errorf("push: %s returned %d: %s", endpointURL.Host, resp.StatusCode, string(respBody))
}

// ErrSubscriptionGone indicates the push service no longer
// recognises the subscription. Callers should prune the row.
var ErrSubscriptionGone = errors.New("push: subscription gone (404/410)")

// signVAPIDJWT builds the ES256 JWT per RFC 8292. The `aud`
// claim is the origin of the push service's endpoint.
func (t *WebPushTransport) signVAPIDJWT(endpoint *url.URL) (string, error) {
	header := map[string]string{"typ": "JWT", "alg": "ES256"}
	claims := map[string]any{
		"aud": fmt.Sprintf("%s://%s", endpoint.Scheme, endpoint.Host),
		"exp": time.Now().Add(12 * time.Hour).Unix(),
		"sub": t.cfg.Subject,
	}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64URL(headerJSON) + "." + base64URL(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, t.cfg.PrivateKey, digest[:])
	if err != nil {
		return "", err
	}
	curveBytes := (t.cfg.PrivateKey.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*curveBytes)
	r.FillBytes(sig[:curveBytes])
	s.FillBytes(sig[curveBytes:])
	return signingInput + "." + base64URL(sig), nil
}

// base64URL encodes b using URL-safe base64 without padding.
func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// encodeUncompressed returns the base64url SEC1 uncompressed
// public-key encoding (0x04 || X || Y).
func encodeUncompressed(pub *ecdsa.PublicKey) string {
	buf := elliptic.Marshal(pub.Curve, pub.X, pub.Y)
	return base64URL(buf)
}

// decodeVAPIDPublic parses a base64url-encoded uncompressed
// P-256 public key.
func decodeVAPIDPublic(s string) (*ecdsa.PublicKey, error) {
	raw, err := base64URLDecode(s)
	if err != nil {
		return nil, err
	}
	if len(raw) != 65 || raw[0] != 0x04 {
		return nil, fmt.Errorf("push: VAPID public key must be 65-byte uncompressed P-256 (got %d)", len(raw))
	}
	x := new(big.Int).SetBytes(raw[1:33])
	y := new(big.Int).SetBytes(raw[33:])
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

// decodeVAPIDPrivate parses a base64url-encoded P-256 scalar and
// pairs it with the supplied public key.
func decodeVAPIDPrivate(s string, pub *ecdsa.PublicKey) (*ecdsa.PrivateKey, error) {
	raw, err := base64URLDecode(s)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("push: VAPID private key must be 32 bytes (got %d)", len(raw))
	}
	return &ecdsa.PrivateKey{
		PublicKey: *pub,
		D:         new(big.Int).SetBytes(raw),
	}, nil
}

// base64URLDecode is forgiving about padding. Web Push specs use
// URL-safe base64 without padding, but some libraries emit with
// padding so we trim before decoding.
func base64URLDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// encryptAES128GCM implements RFC 8291 (Message Encryption for
// Web Push) using the aes128gcm content encoding (RFC 8188).
//
// Inputs are the user agent's P-256 public key (`p256dh`) and the
// 16-byte authentication secret (`auth`), both base64url-encoded.
//
// Output is a single record framed per RFC 8188 with the recipient's
// salt + record size + key ID, followed by the AEAD ciphertext.
func encryptAES128GCM(payload []byte, p256dh, auth string) ([]byte, error) {
	uaPubBytes, err := base64URLDecode(p256dh)
	if err != nil {
		return nil, fmt.Errorf("decode p256dh: %w", err)
	}
	authSecret, err := base64URLDecode(auth)
	if err != nil {
		return nil, fmt.Errorf("decode auth: %w", err)
	}
	if len(uaPubBytes) != 65 || uaPubBytes[0] != 0x04 {
		return nil, fmt.Errorf("p256dh must be 65-byte uncompressed P-256 (got %d)", len(uaPubBytes))
	}

	curve := ecdh.P256()
	uaPub, err := curve.NewPublicKey(uaPubBytes)
	if err != nil {
		return nil, fmt.Errorf("parse p256dh: %w", err)
	}
	asPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	asPubBytes := asPriv.PublicKey().Bytes()
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}

	// RFC 8291 §3.3 / §3.4 KDF.
	// PRK_key = HMAC-SHA-256(auth_secret, IKM_combined)
	// where IKM_combined = HKDF(... ECDH ...) ; matches the spec.
	prkKey := hkdf(authSecret, shared, []byte("WebPush: info\x00"+string(uaPubBytes)+string(asPubBytes)), 32)
	cek := hkdf(salt, prkKey, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdf(salt, prkKey, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// RFC 8188 padding: 0x02 marker + zero-fill (one record).
	plaintext := append(payload, 0x02)
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	// RFC 8188 header: salt(16) || recordSize(4 BE) || keyIDLen(1) || keyID(=asPubBytes)
	recordSize := uint32(4096)
	header := make([]byte, 0, 16+4+1+len(asPubBytes))
	header = append(header, salt...)
	rs := make([]byte, 4)
	binary.BigEndian.PutUint32(rs, recordSize)
	header = append(header, rs...)
	header = append(header, byte(len(asPubBytes)))
	header = append(header, asPubBytes...)
	return append(header, ciphertext...), nil
}

// hkdf is HKDF-SHA-256 returning `length` bytes. Sufficient for
// the short outputs RFC 8291 needs (≤ 32 bytes).
func hkdf(salt, ikm, info []byte, length int) []byte {
	prkH := hmac.New(sha256.New, salt)
	prkH.Write(ikm)
	prk := prkH.Sum(nil)
	out := make([]byte, 0, length)
	var t []byte
	for i := byte(1); len(out) < length; i++ {
		h := hmac.New(sha256.New, prk)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{i})
		t = h.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// GenerateVAPIDKeys returns a fresh P-256 keypair as base64url
// strings. Used by `cmd/util/vapid` (or operators in a one-shot
// script) to mint the org's permanent VAPID keys.
func GenerateVAPIDKeys() (publicB64, privateB64 string, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	pubB64 := encodeUncompressed(&priv.PublicKey)
	privBytes := make([]byte, 32)
	priv.D.FillBytes(privBytes)
	return pubB64, base64URL(privBytes), nil
}
