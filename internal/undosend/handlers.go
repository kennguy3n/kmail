package undosend

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wraps Service with the cancel/status HTTP surface.
//
// Routes:
//
//	POST /api/v1/send/{id}/cancel  → cancel the pending send
//	GET  /api/v1/send/{id}         → return the current status
type Handlers struct {
	svc *Service
}

// NewHandlers panics on nil to make wiring bugs loud.
func NewHandlers(svc *Service) *Handlers {
	if svc == nil {
		panic("undosend.NewHandlers: svc is required")
	}
	return &Handlers{svc: svc}
}

// Register wires the cancel/status endpoints behind the existing
// OIDC middleware. The same middleware sets the tenant + KChat
// user on the request context so the handlers can authorize.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/send/{id}/cancel", authMW.Wrap(http.HandlerFunc(h.cancel)))
	mux.Handle("GET /api/v1/send/{id}", authMW.Wrap(http.HandlerFunc(h.status)))
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
	err := h.svc.Cancel(r.Context(), id, tenantID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
	case errors.Is(err, ErrAlreadySent):
		// 410 Gone is the canonical "we know what you mean but
		// it's no longer cancellable" status. The JMAP client
		// surfaces this as a friendlier "Message already sent"
		// toast rather than treating it as a hard error.
		writeJSON(w, http.StatusGone, map[string]any{"cancelled": false, "error": "already sent"})
	case errors.Is(err, ErrTenantMismatch):
		// Do NOT surface "tenant mismatch" to the wire — that
		// would confirm the id exists in some other tenant. 404
		// is the only safe answer.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}
}

func (h *Handlers) status(w http.ResponseWriter, r *http.Request) {
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
	ps, err := h.svc.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A missing key is either dispatched-and-cleaned or
			// cancelled. The JMAP client wants to know the
			// outcome rather than a 404; return a typed status.
			writeJSON(w, http.StatusOK, map[string]any{
				"id":     id,
				"status": StatusSent, // dispatched-or-cancelled — client decides UI from prior state
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	if ps.TenantID != tenantID {
		// Same reasoning as Cancel: don't leak cross-tenant existence.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, statusResponse(ps))
}

// statusResponse strips the SubmissionPayload before returning to
// the wire — the payload may contain Internet Mail addressees and
// has no value to the React client.
func statusResponse(ps *PendingSend) map[string]any {
	return map[string]any{
		"id":           ps.ID,
		"status":       ps.Status,
		"email_id":     ps.EmailID,
		"created_at":   ps.CreatedAt.UTC().Format(time.RFC3339),
		"deadline_at":  ps.DeadlineAt.UTC().Format(time.RFC3339),
		"attempts":     ps.Attempts,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
