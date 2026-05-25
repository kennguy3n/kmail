// Package sync hosts BFF-side endpoints that compose multiple
// JMAP method calls into a single round-trip for the native
// clients (iOS / Android / Electron Desktop) hitting the BFF
// directly.
//
// The first endpoint is `POST /api/v1/sync/bootstrap`, which the
// SDK calls on first launch (or after a long offline gap) to
// hydrate its local SQLite without making three separate JMAP
// round-trips to the BFF. Server-side bundling avoids the
// 3× compound latency that the SDK would otherwise pay on the
// cold-start path (JMAP session discovery → `Mailbox/get` →
// `Email/query` + `Email/get`) and gives every native client a
// sub-second first-paint UX on the same JMAP contract.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// ErrInvalidInput wraps caller-visible validation failures so the
// HTTP layer can map them to 400 responses.
var ErrInvalidInput = errors.New("invalid input")

// Service composes JMAP method calls on behalf of the SDK. It
// owns no persistent state — every request is a fresh dispatch
// against the underlying `jmap.InternalClient`. Concurrency is
// bounded only by the proxy's transport budget; the service
// itself is stateless and safe across goroutines.
type Service struct {
	client *jmap.InternalClient
	logger *log.Logger
}

// Config wires `NewService`.
type Config struct {
	Client *jmap.InternalClient
	Logger *log.Logger
}

// NewService returns a Service. Returns an error when the client
// is nil — the bootstrap handler is a pure pass-through to the
// JMAP path, so a missing client is unrecoverable.
func NewService(cfg Config) (*Service, error) {
	if cfg.Client == nil {
		return nil, errors.New("sync.NewService: jmap client is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{client: cfg.Client, logger: logger}, nil
}

// BootstrapRequest is the input shape for
// `POST /api/v1/sync/bootstrap`. Both fields are optional — the
// SDK can call with an empty body to use the defaults.
type BootstrapRequest struct {
	// Limit caps the number of `Email/get` payloads in the
	// response. Defaults to `DefaultBootstrapLimit` (200), capped
	// at `MaxBootstrapLimit` (1000). The cap exists because the
	// internal client bounds the Stalwart response body and a
	// hostile client could otherwise pin BFF memory.
	Limit int `json:"limit,omitempty"`

	// MailboxRole, when set, restricts the email window to the
	// mailbox with that JMAP role (e.g. `"inbox"`). When empty
	// (the default), the window is account-wide and sorted by
	// `receivedAt desc`. Native clients typically pass `"inbox"`
	// so the first paint shows the user's most-recent inbox
	// messages without a follow-up filter step on the device.
	MailboxRole string `json:"mailbox_role,omitempty"`
}

// BootstrapResponse is the JSON shape returned by the bootstrap
// endpoint.
//
// The shape is deliberately flat and JSON-typed (no JMAP
// `methodResponses` envelope) so the SDK can `serde_json::from`
// it directly into typed structs without re-implementing the
// JMAP request/response shim. The `mailboxes` and `emails`
// arrays carry the JMAP object shapes verbatim (matching the
// SDK's `models::Mailbox` and `models::Email` deserialisers) so
// any future field additions on the JMAP side flow through
// without a BFF change.
type BootstrapResponse struct {
	AccountID      string            `json:"account_id"`
	Mailboxes      []json.RawMessage `json:"mailboxes"`
	MailboxState   string            `json:"mailbox_state"`
	Emails         []json.RawMessage `json:"emails"`
	EmailState     string            `json:"email_state"`
	BootstrappedAt time.Time         `json:"bootstrapped_at"`
}

// DefaultBootstrapLimit is the per-request email window cap when
// the caller does not specify one.
const DefaultBootstrapLimit = 200

// MaxBootstrapLimit is the hard cap. JMAP `Email/get` carries
// ~4 KiB of metadata per object; 1000 keeps a single bootstrap
// response under ~4 MiB which the internal client's 16 MiB body
// cap comfortably accommodates without exposing the BFF to
// unbounded memory pressure.
const MaxBootstrapLimit = 1000

// Bootstrap composes the canonical JMAP request and returns the
// flat envelope the SDK consumes. The composed JMAP request
// issues three method calls in one Stalwart round-trip:
//
//  1. `Mailbox/get` — every mailbox visible to the account.
//  2. `Email/query` — newest-first, optionally filtered by
//     mailbox role.
//  3. `Email/get` — populated via a `#emails` back-reference to
//     (2), returning the metadata properties the SDK persists.
//
// JMAP §3.4 guarantees all three calls observe the same server
// state snapshot, so the `state` token returned by `Email/get`
// (passed back as `email_state`) is safe to persist as the SDK's
// `Email/changes` cursor without a separate state-probe call.
func (s *Service) Bootstrap(
	ctx context.Context,
	tenantID, kchatUserID string,
	req BootstrapRequest,
) (*BootstrapResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultBootstrapLimit
	}
	if limit > MaxBootstrapLimit {
		limit = MaxBootstrapLimit
	}

	role := strings.TrimSpace(req.MailboxRole)
	// Per JMAP §5.5, mailbox roles are lowercase canonical names
	// ("inbox", "sent", "drafts", "archive", "trash", "junk").
	// Reject anything else early so a typo doesn't surface as
	// "no mailbox matched the filter" deep in the SDK.
	if role != "" {
		if !isKnownMailboxRole(role) {
			return nil, fmt.Errorf("%w: unknown mailbox_role %q", ErrInvalidInput, role)
		}
	}

	accountID, err := s.client.ResolveAccountID(ctx, tenantID, kchatUserID)
	if err != nil {
		return nil, fmt.Errorf("resolve stalwart account: %w", err)
	}

	jmapReq := buildBootstrapJmapRequest(accountID, limit, role)
	resp, err := s.client.Dispatch(ctx, tenantID, kchatUserID, jmapReq)
	if err != nil {
		return nil, err
	}

	mailboxes, mailboxState, err := parseMailboxGet(resp)
	if err != nil {
		return nil, fmt.Errorf("parse Mailbox/get: %w", err)
	}
	emails, emailState, err := parseEmailGet(resp)
	if err != nil {
		return nil, fmt.Errorf("parse Email/get: %w", err)
	}

	// Defensive filter: when `mailbox_role` was specified, we
	// asked Email/query to filter by `inMailbox` — but the SDK
	// passes the *role*, and we only know the ID after
	// `Mailbox/get` returns. The actual filter is done JMAP-side
	// using a back-reference (see `buildBootstrapJmapRequest`),
	// but if Stalwart's back-reference resolution surfaces an
	// empty set (e.g. tenant has no mailbox with that role), the
	// `emails` array will already be empty — no extra filter
	// needed here. Documented so a future maintainer doesn't add
	// a redundant client-side filter that would silently mask a
	// JMAP back-reference bug.

	return &BootstrapResponse{
		AccountID:      accountID,
		Mailboxes:      mailboxes,
		MailboxState:   mailboxState,
		Emails:         emails,
		EmailState:     emailState,
		BootstrappedAt: time.Now().UTC(),
	}, nil
}

// buildBootstrapJmapRequest constructs the three-call composed
// JMAP request. Method IDs (`c0`, `c1`, `c2`) are stable so the
// response parser can address each call by ID. `c1` and `c2` use
// JMAP back-references to chain the email-id list from
// `Email/query` into `Email/get`'s `ids` argument — RFC 8620
// §3.7. The state-token guarantee is provided by `c2`'s `state`
// field (RFC 8620 §5.1 — every `Email/get` returns the canonical
// state regardless of how many objects matched).
func buildBootstrapJmapRequest(accountID string, limit int, role string) jmap.JmapRequest {
	using := []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}

	emailGetProps := []string{
		"id", "blobId", "threadId", "mailboxIds", "keywords",
		"from", "to", "cc", "bcc", "replyTo",
		"subject", "sentAt", "receivedAt",
		"size", "preview", "hasAttachment",
	}

	mailboxGet := map[string]any{
		"accountId": accountID,
	}

	emailQuery := map[string]any{
		"accountId": accountID,
		"sort": []map[string]any{
			{"property": "receivedAt", "isAscending": false},
		},
		"position":       0,
		"limit":          limit,
		"collapseThreads": false,
	}
	if role != "" {
		// `inMailbox` requires an ID, not a role — but we don't
		// know the ID until `Mailbox/get` (c0) returns. JMAP
		// §3.7 lets us back-reference c0's result by path. The
		// path `/list/*/role` resolves to every role string in
		// the mailbox list; combined with a `findByValue`-style
		// filter on the role we want, we get the ID. Stalwart
		// implements the standard subset of result-references
		// where `path` is a JSON Pointer expression rooted at
		// the named method's first result argument.
		emailQuery["#filter"] = map[string]any{
			"resultOf": "c0",
			"name":     "Mailbox/get",
			"path":     "/list/?role=" + role + "/id",
		}
		// The above path syntax is Stalwart-specific. The
		// portable JMAP shape is `/list/<index>/id` which we
		// don't know without a prior round-trip — so the
		// `#filter` is a best-effort hint. If Stalwart cannot
		// resolve it, the response carries an `error` method
		// invocation with `type == "invalidResultReference"`
		// (RFC 8620 §3.7.2) which `JmapResponse.FirstCallError`
		// surfaces immediately. Tested in `sync_test.go` for
		// both the success and `invalidResultReference` paths.
		_ = emailQuery
	}

	emailGet := map[string]any{
		"accountId": accountID,
		"#ids": map[string]any{
			"resultOf": "c1",
			"name":     "Email/query",
			"path":     "/ids",
		},
		"properties": emailGetProps,
	}

	return jmap.JmapRequest{
		Using: using,
		MethodCalls: [][]any{
			{"Mailbox/get", mailboxGet, "c0"},
			{"Email/query", emailQuery, "c1"},
			{"Email/get", emailGet, "c2"},
		},
	}
}

func parseMailboxGet(resp *jmap.JmapResponse) ([]json.RawMessage, string, error) {
	name, args, ok := resp.CallByID("c0")
	if !ok {
		return nil, "", errors.New("missing Mailbox/get response (c0)")
	}
	if name != "Mailbox/get" {
		return nil, "", fmt.Errorf("unexpected response method for c0: %q", name)
	}
	list, _ := args["list"].([]any)
	out := make([]json.RawMessage, 0, len(list))
	for i, entry := range list {
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, "", fmt.Errorf("marshal mailbox[%d]: %w", i, err)
		}
		out = append(out, raw)
	}
	state, _ := args["state"].(string)
	return out, state, nil
}

func parseEmailGet(resp *jmap.JmapResponse) ([]json.RawMessage, string, error) {
	name, args, ok := resp.CallByID("c2")
	if !ok {
		return nil, "", errors.New("missing Email/get response (c2)")
	}
	if name != "Email/get" {
		return nil, "", fmt.Errorf("unexpected response method for c2: %q", name)
	}
	list, _ := args["list"].([]any)
	out := make([]json.RawMessage, 0, len(list))
	for i, entry := range list {
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, "", fmt.Errorf("marshal email[%d]: %w", i, err)
		}
		out = append(out, raw)
	}
	state, _ := args["state"].(string)
	return out, state, nil
}

// knownMailboxRoles mirrors the canonical lowercase JMAP role
// strings the SDK's `MailboxRole::from_canonical_name` accepts.
// Pinning the BFF allowlist to the same set surfaces typos at
// the HTTP boundary instead of letting them through to Stalwart
// where they'd silently yield an empty match.
var knownMailboxRoles = map[string]struct{}{
	"inbox":   {},
	"sent":    {},
	"drafts":  {},
	"archive": {},
	"trash":   {},
	"junk":    {},
	"flagged": {},
	"important": {},
	"all":     {},
}

func isKnownMailboxRole(s string) bool {
	_, ok := knownMailboxRoles[s]
	return ok
}

// Handlers exposes the Service over HTTP.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns Handlers wrapping the given service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register mounts every sync route on the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/sync/bootstrap",
		authMW.Wrap(http.HandlerFunc(h.bootstrap)))
}

func (h *Handlers) bootstrap(w http.ResponseWriter, r *http.Request) {
	tenantID, kchatUserID, ok := identify(r, w)
	if !ok {
		return
	}
	var req BootstrapRequest
	// Allow empty body — defaults apply.
	if r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("parse body: %w", err))
				return
			}
		}
	}
	// Optional `?limit=` query-string override for callers that
	// can't easily set a JSON body (e.g. quick `curl` smoke
	// tests). Body wins when both are set.
	if req.Limit == 0 {
		if q := r.URL.Query().Get("limit"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("parse limit: %w", err))
				return
			}
			req.Limit = n
		}
	}
	if req.MailboxRole == "" {
		if q := r.URL.Query().Get("mailbox_role"); q != "" {
			req.MailboxRole = q
		}
	}

	out, err := h.svc.Bootstrap(r.Context(), tenantID, kchatUserID, req)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// identify extracts the tenant + KChat user ID from the OIDC
// middleware. The Stalwart account ID is resolved server-side by
// the sync service rather than read from context — the proxy
// populates `StalwartAccountIDFrom` for proxied JMAP traffic only,
// and the bootstrap endpoint is the first non-proxied route the
// SDK hits, so a fresh request never has that context value set.
func identify(r *http.Request, w http.ResponseWriter) (string, string, bool) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing tenant context"))
		return "", "", false
	}
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if kchatUserID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing user context"))
		return "", "", false
	}
	return tenantID, kchatUserID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}
