package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// manager is the slice of Service the REST surface depends on.
// Tests substitute an in-memory fake.
type manager interface {
	ListByUser(ctx context.Context, tenantID, kchatUserID string) ([]ScheduledSend, error)
	Get(ctx context.Context, tenantID, id string) (*ScheduledSend, error)
	Cancel(ctx context.Context, tenantID, id string) error
}

// Handlers exposes the scheduled-send REST surface.
//
// Routes:
//
//	GET    /api/v1/scheduled-sends         → list this user's scheduled sends
//	GET    /api/v1/scheduled-sends/{id}    → fetch one row
//	DELETE /api/v1/scheduled-sends/{id}    → cancel a pending row
//
// Create is intentionally NOT exposed here: a scheduled send is
// created via the JMAP proxy hook so the caller pays exactly one
// Stalwart round-trip (Email/set creates the draft, the hook
// holds the EmailSubmission/set call). See proxy_hook.go.
type Handlers struct {
	svc manager
}

// NewHandlers constructs the REST surface. Panics on nil so
// wiring bugs in main.go are loud.
func NewHandlers(svc *Service) *Handlers {
	if svc == nil {
		panic("scheduledsend.NewHandlers: svc is required")
	}
	return &Handlers{svc: svc}
}

// newHandlersWithManager is the test seam.
func newHandlersWithManager(m manager) *Handlers {
	return &Handlers{svc: m}
}

// Register wires the routes behind the existing OIDC middleware.
// The middleware populates `tenant_id` + `kchat_user_id` on the
// request context so the handlers can authorize without parsing
// headers themselves.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/scheduled-sends", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/v1/scheduled-sends/{id}", authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("DELETE /api/v1/scheduled-sends/{id}", authMW.Wrap(http.HandlerFunc(h.cancel)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing auth context"})
		return
	}
	rows, err := h.svc.ListByUser(r.Context(), tenantID, kchatUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scheduled_sends": toResponses(rows),
	})
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing tenant context"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	ss, err := h.svc.Get(r.Context(), tenantID, id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, toResponse(ss))
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTenantMismatch):
		// Collapse "tenant mismatch" into 404 so the wire can't
		// enumerate cross-tenant ids.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}
}

func (h *Handlers) cancel(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing tenant context"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	err := h.svc.Cancel(r.Context(), tenantID, id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
	case errors.Is(err, ErrAlreadyDispatched):
		// 410 Gone — the canonical "we know what you mean but
		// it's no longer cancellable" status, same as undosend.
		writeJSON(w, http.StatusGone, map[string]any{"cancelled": false, "error": "already sent"})
	case errors.Is(err, ErrAlreadyCancelled):
		// Idempotent double-cancel — 200 with the same body the
		// happy path returns. The client doesn't need to
		// distinguish between "we cancelled it just now" and
		// "it was already cancelled".
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
	case errors.Is(err, ErrTenantMismatch), errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}
}

// responsePayload strips the raw submission JSON from the wire
// response — the React client doesn't need it and exposing the
// full body would leak the addressees on the user's "Scheduled"
// page.
type responsePayload struct {
	ID                string  `json:"id"`
	Status            string  `json:"status"`
	EmailID           string  `json:"email_id"`
	IdentityID        string  `json:"identity_id"`
	StalwartAccountID string  `json:"stalwart_account_id,omitempty"`
	SendAt            string  `json:"send_at"`
	Attempts          int     `json:"attempts"`
	LastError         string  `json:"last_error,omitempty"`
	SentAt            *string `json:"sent_at,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

func toResponse(ss *ScheduledSend) responsePayload {
	var sentAt *string
	if ss.SentAt != nil {
		s := ss.SentAt.UTC().Format(time.RFC3339)
		sentAt = &s
	}
	return responsePayload{
		ID:                ss.ID,
		Status:            ss.Status,
		EmailID:           ss.EmailID,
		IdentityID:        ss.IdentityID,
		StalwartAccountID: ss.StalwartAccountID,
		SendAt:            ss.SendAt.UTC().Format(time.RFC3339),
		Attempts:          ss.Attempts,
		LastError:         ss.LastError,
		SentAt:            sentAt,
		CreatedAt:         ss.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toResponses(rows []ScheduledSend) []responsePayload {
	out := make([]responsePayload, len(rows))
	for i := range rows {
		out[i] = toResponse(&rows[i])
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
