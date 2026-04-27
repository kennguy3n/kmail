// Package sharedinbox — MLS group key rotation for shared inboxes.
//
// PROPOSAL.md §3.1 specifies "shared-inbox MLS group keys". KMail
// does not own the MLS state machine itself — the KChat MLS
// credential service does — so this file plays the role of an
// adapter: when a shared inbox's membership changes (via
// `tenant.Service.AddSharedInboxMember` or `RemoveSharedInboxMember`)
// the WorkflowService asks the manager to mint or rotate the
// underlying MLS group so the next message bound for the inbox is
// encrypted to the current member set.
//
// The HTTP shape mirrors `internal/confidentialsend/mls.go`'s
// HTTPKeyDeriver: a JSON request to KChat's MLS endpoint, gated
// by `KCHAT_MLS_ENDPOINT`. When the endpoint is empty the manager
// is in "disabled" mode — every operation is a no-op + warning
// log so the surrounding flow degrades gracefully.
package sharedinbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MLSGroupManager is the contract between sharedinbox / tenant
// services and the underlying MLS implementation. It mirrors the
// shape of `confidentialsend.MLSKeyDeriver` so the wire shape is
// recognisable to KChat operators familiar with the existing
// confidential-send flow.
type MLSGroupManager interface {
	// EnsureGroup creates an MLS group for the inbox if one
	// doesn't yet exist. Returns the KChat-side group ID. Idempotent.
	EnsureGroup(ctx context.Context, inboxID string, members []string) (string, error)
	// RotateGroup applies a membership delta + rekeys. Called from
	// tenant.Service.AddSharedInboxMember / RemoveSharedInboxMember.
	// `members` is the post-mutation member set.
	RotateGroup(ctx context.Context, inboxID string, members []string, reason string) (string, error)
	// Status returns the current epoch + member count for the
	// inbox's MLS group, surfacing it to the admin UI.
	Status(ctx context.Context, inboxID string) (*MLSGroupStatus, error)
	// Enabled reports whether a backing MLS endpoint is wired.
	// When false, the workflow caller may skip rotation cleanly.
	Enabled() bool
}

// MLSGroupStatus is the JSON shape returned by the status
// endpoint and the manager's `Status` method.
type MLSGroupStatus struct {
	InboxID     string    `json:"inbox_id"`
	GroupID     string    `json:"group_id"`
	Epoch       int64     `json:"epoch"`
	MemberCount int       `json:"member_count"`
	UpdatedAt   time.Time `json:"updated_at"`
	Enabled     bool      `json:"enabled"`
}

// HTTPMLSGroupManager talks JSON over HTTPS to the KChat MLS
// endpoint configured by `KCHAT_MLS_ENDPOINT`.
type HTTPMLSGroupManager struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// NewHTTPMLSGroupManager constructs a manager. Pass an empty
// `endpoint` for the disabled path: every method becomes a no-op
// returning an empty group ID + warning log.
func NewHTTPMLSGroupManager(endpoint, token string) *HTTPMLSGroupManager {
	endpoint = strings.TrimRight(endpoint, "/")
	return &HTTPMLSGroupManager{
		Endpoint: endpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled returns true when an endpoint is configured.
func (m *HTTPMLSGroupManager) Enabled() bool {
	return m != nil && m.Endpoint != ""
}

// EnsureGroup POSTs to /mls/shared-inbox/groups.
func (m *HTTPMLSGroupManager) EnsureGroup(ctx context.Context, inboxID string, members []string) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	var resp struct {
		GroupID string `json:"group_id"`
	}
	body := map[string]any{"inbox_id": inboxID, "members": members}
	if err := m.do(ctx, http.MethodPost, "/mls/shared-inbox/groups", body, &resp); err != nil {
		return "", err
	}
	return resp.GroupID, nil
}

// RotateGroup POSTs to /mls/shared-inbox/groups/:id/rotate.
func (m *HTTPMLSGroupManager) RotateGroup(ctx context.Context, inboxID string, members []string, reason string) (string, error) {
	if !m.Enabled() {
		return "", nil
	}
	var resp struct {
		GroupID string `json:"group_id"`
	}
	body := map[string]any{"members": members, "reason": reason}
	path := fmt.Sprintf("/mls/shared-inbox/groups/%s/rotate", inboxID)
	if err := m.do(ctx, http.MethodPost, path, body, &resp); err != nil {
		return "", err
	}
	return resp.GroupID, nil
}

// Status GETs /mls/shared-inbox/groups/:id.
func (m *HTTPMLSGroupManager) Status(ctx context.Context, inboxID string) (*MLSGroupStatus, error) {
	if !m.Enabled() {
		return &MLSGroupStatus{InboxID: inboxID, Enabled: false}, nil
	}
	var s MLSGroupStatus
	path := fmt.Sprintf("/mls/shared-inbox/groups/%s", inboxID)
	if err := m.do(ctx, http.MethodGet, path, nil, &s); err != nil {
		return nil, err
	}
	s.InboxID = inboxID
	s.Enabled = true
	return &s, nil
}

func (m *HTTPMLSGroupManager) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.Endpoint+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if r != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.Token)
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("kchat mls: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("kchat mls: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("kchat mls: decode: %w", err)
	}
	return nil
}

// NoopMLSGroupManager is the disabled-mode default. EnsureGroup /
// RotateGroup return ("", nil); Status returns
// `{Enabled: false}`. Useful as a safe fallback in tests and
// development environments.
type NoopMLSGroupManager struct{}

// NewNoopMLSGroupManager returns the noop manager.
func NewNoopMLSGroupManager() *NoopMLSGroupManager { return &NoopMLSGroupManager{} }

// EnsureGroup returns ("", nil).
func (NoopMLSGroupManager) EnsureGroup(context.Context, string, []string) (string, error) {
	return "", nil
}

// RotateGroup returns ("", nil).
func (NoopMLSGroupManager) RotateGroup(context.Context, string, []string, string) (string, error) {
	return "", nil
}

// Status returns {Enabled: false}.
func (NoopMLSGroupManager) Status(_ context.Context, inboxID string) (*MLSGroupStatus, error) {
	return &MLSGroupStatus{InboxID: inboxID, Enabled: false}, nil
}

// Enabled returns false.
func (NoopMLSGroupManager) Enabled() bool { return false }

// ErrMLSDisabled is returned by status endpoints when the manager
// is in disabled mode and the caller wants to surface that.
var ErrMLSDisabled = errors.New("sharedinbox: MLS not configured")
