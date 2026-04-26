package confidentialsend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MLSKeyDeriver derives per-recipient DEK wrapping keys against
// the KChat MLS credential service. KMail sees the wrapping key
// only — the underlying MLS leaf-key material never leaves
// KChat.
type MLSKeyDeriver interface {
	// DeriveWrappingKey returns the symmetric key (32 bytes,
	// hex-encoded) that wraps the per-message DEK for the
	// recipient identified by `recipientCredential`. The sender's
	// MLS leaf identity is supplied for the KDF context.
	DeriveWrappingKey(ctx context.Context, senderLeafKey, recipientCredential string) (string, error)

	// RekeyConfidentialMessage instructs the MLS service to mint
	// a new wrapping key for `messageID` because the participant
	// set has changed. KMail re-wraps the existing DEK with the
	// new key and updates `confidential_send_links`.
	RekeyConfidentialMessage(ctx context.Context, messageID string, newParticipants []string) (string, error)
}

// HTTPKeyDeriver is the production MLS key deriver. It speaks
// JSON over HTTPS to the KChat MLS endpoint configured by
// `KCHAT_MLS_ENDPOINT`. When the endpoint is empty the deriver is
// disabled and the surrounding flow degrades to the link-based
// portal.
type HTTPKeyDeriver struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// NewHTTPKeyDeriver returns a deriver. Pass an empty `endpoint`
// to disable MLS — `Enabled` returns false and the surrounding
// flow falls back gracefully.
func NewHTTPKeyDeriver(endpoint, token string) *HTTPKeyDeriver {
	endpoint = strings.TrimRight(endpoint, "/")
	return &HTTPKeyDeriver{
		Endpoint: endpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether the deriver has a backing endpoint.
func (d *HTTPKeyDeriver) Enabled() bool {
	if d == nil {
		return false
	}
	return strings.TrimSpace(d.Endpoint) != ""
}

// DeriveWrappingKey calls `POST <endpoint>/mls/wrap` with the
// sender + recipient material. The response shape is
// `{ "wrapping_key": "<hex>" }`. When the endpoint is unset the
// caller is expected to fall back to the link flow before calling
// this method; we still return an error here to be defensive.
func (d *HTTPKeyDeriver) DeriveWrappingKey(ctx context.Context, senderLeafKey, recipientCredential string) (string, error) {
	if !d.Enabled() {
		return "", ErrMLSDisabled
	}
	if senderLeafKey == "" || recipientCredential == "" {
		return "", errors.New("confidentialsend: senderLeafKey + recipientCredential required")
	}
	body, _ := json.Marshal(map[string]string{
		"sender_leaf_key":      senderLeafKey,
		"recipient_credential": recipientCredential,
	})
	return d.post(ctx, "/mls/wrap", body)
}

// RekeyConfidentialMessage calls `POST <endpoint>/mls/rekey`.
func (d *HTTPKeyDeriver) RekeyConfidentialMessage(ctx context.Context, messageID string, newParticipants []string) (string, error) {
	if !d.Enabled() {
		return "", ErrMLSDisabled
	}
	if messageID == "" {
		return "", errors.New("confidentialsend: messageID required")
	}
	body, _ := json.Marshal(map[string]any{
		"message_id":      messageID,
		"new_participants": newParticipants,
	})
	return d.post(ctx, "/mls/rekey", body)
}

// ErrMLSDisabled is returned when MLS is not configured. Callers
// should fall back to the link portal flow.
var ErrMLSDisabled = errors.New("confidentialsend: MLS endpoint not configured")

func (d *HTTPKeyDeriver) post(ctx context.Context, path string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("confidentialsend: MLS endpoint returned %d: %s", resp.StatusCode, string(buf))
	}
	var out struct {
		WrappingKey string `json:"wrapping_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.WrappingKey == "" {
		return "", errors.New("confidentialsend: empty wrapping_key in MLS response")
	}
	return out.WrappingKey, nil
}

// DerivePlaceholderWrappingKey is a deterministic local fallback
// used by tests when no MLS endpoint is wired. NOT for production.
func DerivePlaceholderWrappingKey(senderLeafKey, recipientCredential string) string {
	h := sha256.New()
	h.Write([]byte(senderLeafKey))
	h.Write([]byte{0})
	h.Write([]byte(recipientCredential))
	return hex.EncodeToString(h.Sum(nil))
}
