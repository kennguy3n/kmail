package smartfeatures

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// OneClickUnsubscriber performs an RFC 8058 one-click HTTP POST to
// a sender's unsubscribe endpoint. Declared as an interface so the
// handler can be tested without real outbound network calls and so
// the production implementation can enforce an SSRF guard
// (see unsubscribehttp.go).
type OneClickUnsubscriber interface {
	Post(ctx context.Context, rawurl string) error
}

// Handlers is the REST surface for the smart-features layer.
//
// Routes (all behind the OIDC middleware, which populates
// tenant_id + kchat_user_id on the request context):
//
//	GET  /api/v1/emails/{id}/smart-replies   → rule-based reply chips
//	GET  /api/v1/emails/{id}/unsubscribe     → parsed unsubscribe affordance
//	POST /api/v1/emails/{id}/unsubscribe     → perform one-click / record intent
//	POST /api/v1/emails/categories           → batch Gmail-style categories
//	GET  /api/v1/contacts/frequent           → most-emailed recipients
//	GET  /api/v1/contacts/suggestions        → co-recipient suggestions
//	POST /api/v1/contacts/record             → record a sent message's recipients
type Handlers struct {
	fetcher  EmailFetcher
	contacts *ContactTracker
	unsub    *UnsubscribeStore
	oneClick OneClickUnsubscriber
	logger   *log.Logger
	now      func() time.Time
}

// HandlersConfig wires the handler dependencies. fetcher is
// required; contacts/unsub/oneClick are optional and, when nil,
// degrade the corresponding endpoints to a 503 rather than
// panicking — this lets the API boot when Valkey is unavailable
// without taking down the whole smart-features surface.
type HandlersConfig struct {
	Fetcher  EmailFetcher
	Contacts *ContactTracker
	Unsub    *UnsubscribeStore
	OneClick OneClickUnsubscriber
	Logger   *log.Logger
}

// NewHandlers constructs the REST surface. A nil fetcher is a
// wiring bug and panics, matching the loud-fail convention used by
// snooze / undosend NewHandlers.
func NewHandlers(cfg HandlersConfig) *Handlers {
	if cfg.Fetcher == nil {
		panic("smartfeatures.NewHandlers: Fetcher is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{
		fetcher:  cfg.Fetcher,
		contacts: cfg.Contacts,
		unsub:    cfg.Unsub,
		oneClick: cfg.OneClick,
		logger:   logger,
		now:      time.Now,
	}
}

// Register wires the routes behind the OIDC middleware.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/emails/{id}/smart-replies", authMW.Wrap(http.HandlerFunc(h.smartReplies)))
	mux.Handle("GET /api/v1/emails/{id}/unsubscribe", authMW.Wrap(http.HandlerFunc(h.getUnsubscribe)))
	mux.Handle("POST /api/v1/emails/{id}/unsubscribe", authMW.Wrap(http.HandlerFunc(h.postUnsubscribe)))
	mux.Handle("POST /api/v1/emails/categories", authMW.Wrap(http.HandlerFunc(h.categories)))
	mux.Handle("GET /api/v1/contacts/frequent", authMW.Wrap(http.HandlerFunc(h.frequentContacts)))
	mux.Handle("GET /api/v1/contacts/suggestions", authMW.Wrap(http.HandlerFunc(h.coRecipients)))
	mux.Handle("POST /api/v1/contacts/record", authMW.Wrap(http.HandlerFunc(h.recordSend)))
}

func (h *Handlers) ids(r *http.Request) (tenantID, userID string, ok bool) {
	tenantID = middleware.TenantIDFrom(r.Context())
	userID = middleware.KChatUserIDFrom(r.Context())
	return tenantID, userID, tenantID != "" && userID != ""
}

// fetchOne loads a single message and reports a typed not-found so
// the handlers can return 404 vs 500 correctly.
func (h *Handlers) fetchOne(r *http.Request, tenantID, userID, id string) (Message, bool, error) {
	msgs, err := h.fetcher.FetchMessages(r.Context(), tenantID, userID, []string{id})
	if err != nil {
		return Message{}, false, err
	}
	m, ok := msgs[id]
	return m, ok, nil
}

func (h *Handlers) smartReplies(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing id"))
		return
	}
	m, found, err := h.fetchOne(r, tenantID, userID, id)
	if err != nil {
		h.logger.Printf("smartfeatures: smart-replies fetch %s: %v", id, err)
		writeJSON(w, http.StatusBadGateway, errBody("could not load email"))
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errBody("email not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"email_id":    id,
		"suggestions": SuggestReplies(m),
	})
}

func (h *Handlers) getUnsubscribe(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing id"))
		return
	}
	m, found, err := h.fetchOne(r, tenantID, userID, id)
	if err != nil {
		h.logger.Printf("smartfeatures: unsubscribe fetch %s: %v", id, err)
		writeJSON(w, http.StatusBadGateway, errBody("could not load email"))
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errBody("email not found"))
		return
	}
	info, hasUnsub := ParseUnsubscribe(m)
	resp := map[string]any{
		"email_id":     id,
		"unsubscribe":  hasUnsub,
		"already_done": false,
	}
	if hasUnsub {
		resp["info"] = info
		if h.unsub != nil {
			done, err := h.unsub.IsUnsubscribed(r.Context(), tenantID, userID, info.ListID)
			if err != nil {
				h.logger.Printf("smartfeatures: unsubscribe state %s: %v", id, err)
			} else {
				resp["already_done"] = done
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handlers) postUnsubscribe(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing id"))
		return
	}
	m, found, err := h.fetchOne(r, tenantID, userID, id)
	if err != nil {
		h.logger.Printf("smartfeatures: unsubscribe fetch %s: %v", id, err)
		writeJSON(w, http.StatusBadGateway, errBody("could not load email"))
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errBody("email not found"))
		return
	}
	info, hasUnsub := ParseUnsubscribe(m)
	if !hasUnsub {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("email has no unsubscribe header"))
		return
	}

	// Prefer an RFC 8058 one-click POST when the sender advertised
	// it AND the server-side poster is configured. Otherwise the
	// client must drive the mailto:/http link itself — we still
	// record the user's intent so the UI reflects "unsubscribed".
	method := "recorded"
	if info.OneClick && h.oneClick != nil {
		if target, ok := info.PreferredHTTP(); ok {
			if err := h.oneClick.Post(r.Context(), target); err != nil {
				h.logger.Printf("smartfeatures: one-click POST %s: %v", target, err)
				writeJSON(w, http.StatusBadGateway, errBody("unsubscribe request failed"))
				return
			}
			method = "one-click"
		}
	}

	if h.unsub != nil && info.ListID != "" {
		if err := h.unsub.Mark(r.Context(), tenantID, userID, info.ListID); err != nil {
			h.logger.Printf("smartfeatures: mark unsubscribed %s: %v", info.ListID, err)
		}
	}

	resp := map[string]any{
		"email_id":     id,
		"method":       method,
		"already_done": true,
	}
	// When we couldn't POST server-side, hand the client the link to
	// open so it can complete the flow (mailto: or new-tab http).
	if method == "recorded" {
		if mailto, ok := info.PreferredMailto(); ok {
			resp["mailto"] = mailto
		}
		if target, ok := info.PreferredHTTP(); ok {
			resp["open_url"] = target
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type categoriesRequest struct {
	IDs []string `json:"ids"`
}

func (h *Handlers) categories(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	var req categoriesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid request body"))
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"categories": map[string]string{}})
		return
	}
	if len(req.IDs) > 500 {
		writeJSON(w, http.StatusBadRequest, errBody("too many ids (max 500)"))
		return
	}
	msgs, err := h.fetcher.FetchMessages(r.Context(), tenantID, userID, req.IDs)
	if err != nil {
		h.logger.Printf("smartfeatures: categories fetch: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not load emails"))
		return
	}
	cats := make(map[string]string, len(msgs))
	for id, m := range msgs {
		cats[id] = string(Categorize(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

func (h *Handlers) frequentContacts(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	if h.contacts == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("contact tracking unavailable"))
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 10, 50)
	contacts, err := h.contacts.TopContacts(r.Context(), tenantID, userID, limit)
	if err != nil {
		h.logger.Printf("smartfeatures: frequent contacts: %v", err)
		writeJSON(w, http.StatusInternalServerError, errBody("internal error"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": contacts})
}

func (h *Handlers) coRecipients(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	if h.contacts == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("contact tracking unavailable"))
		return
	}
	anchor := strings.TrimSpace(r.URL.Query().Get("anchor"))
	if anchor == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing anchor"))
		return
	}
	// The client sends each excluded address as its own repeated
	// `exclude` query param (smart.ts uses params.append), so read
	// every value — not just the first — and also tolerate a single
	// comma-joined value for manual callers.
	var exclude []string
	for _, ex := range r.URL.Query()["exclude"] {
		for _, part := range strings.Split(ex, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				exclude = append(exclude, trimmed)
			}
		}
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 3, 10)
	suggestions, err := h.contacts.SuggestCoRecipients(r.Context(), tenantID, userID, anchor, exclude, limit)
	if err != nil {
		h.logger.Printf("smartfeatures: co-recipients: %v", err)
		writeJSON(w, http.StatusInternalServerError, errBody("internal error"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"anchor":      anchor,
		"suggestions": suggestions,
	})
}

type recordSendRequest struct {
	Recipients []string `json:"recipients"`
}

func (h *Handlers) recordSend(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.ids(r)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}
	if h.contacts == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("contact tracking unavailable"))
		return
	}
	var req recordSendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid request body"))
		return
	}
	if err := h.contacts.RecordSend(r.Context(), tenantID, userID, req.Recipients); err != nil {
		h.logger.Printf("smartfeatures: record send: %v", err)
		writeJSON(w, http.StatusInternalServerError, errBody("internal error"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true})
}

// --- small helpers ---

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errBody(msg string) map[string]any { return map[string]any{"error": msg} }

// clampLimit parses a `limit` query param, falling back to def and
// capping at max. Non-numeric / non-positive values fall back to
// the default so a malformed client param can't request the whole
// keyspace.
func clampLimit(raw string, def, max int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > max {
			return max
		}
	}
	if n <= 0 {
		return def
	}
	return n
}
