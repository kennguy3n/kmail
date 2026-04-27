package sharedinbox

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Registrar is the subset of the OIDC middleware Handlers consume.
type Registrar interface {
	Wrap(h http.Handler) http.Handler
}

// Handlers wires workflow routes under
// `/api/v1/shared-inboxes/{inboxId}/emails/{emailId}/...`.
type Handlers struct {
	svc    *WorkflowService
	logger *log.Logger
}

// NewHandlers returns Handlers.
func NewHandlers(svc *WorkflowService, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register mounts the workflow routes.
func (h *Handlers) Register(mux *http.ServeMux, auth Registrar) {
	mux.Handle("GET /api/v1/shared-inboxes/{inboxId}/assignments",
		auth.Wrap(http.HandlerFunc(h.listAssignments)))
	mux.Handle("POST /api/v1/shared-inboxes/{inboxId}/emails/{emailId}/assign",
		auth.Wrap(http.HandlerFunc(h.assign)))
	mux.Handle("DELETE /api/v1/shared-inboxes/{inboxId}/emails/{emailId}/assign",
		auth.Wrap(http.HandlerFunc(h.unassign)))
	mux.Handle("PUT /api/v1/shared-inboxes/{inboxId}/emails/{emailId}/status",
		auth.Wrap(http.HandlerFunc(h.setStatus)))
	mux.Handle("GET /api/v1/shared-inboxes/{inboxId}/emails/{emailId}/notes",
		auth.Wrap(http.HandlerFunc(h.listNotes)))
	mux.Handle("POST /api/v1/shared-inboxes/{inboxId}/emails/{emailId}/notes",
		auth.Wrap(http.HandlerFunc(h.addNote)))
	mux.Handle("GET /api/v1/shared-inboxes/{inboxId}/mls/status",
		auth.Wrap(http.HandlerFunc(h.mlsStatus)))
}

// mlsStatus surfaces the MLS group epoch + member count to the
// admin UI. Returns {enabled: false} when the workflow service
// is not wired to a manager (KCHAT_MLS_ENDPOINT empty).
func (h *Handlers) mlsStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := tenantFromReq(r); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	inboxID := r.PathValue("inboxId")
	if h.svc == nil || h.svc.MLS == nil {
		writeJSON(w, http.StatusOK, &MLSGroupStatus{InboxID: inboxID, Enabled: false})
		return
	}
	status, err := h.svc.MLS.Status(r.Context(), inboxID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

type assignRequest struct {
	AssigneeUserID string `json:"assignee_user_id"`
}

func (h *Handlers) assign(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromReq(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in assignRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.AssignEmail(r.Context(), tenantID, r.PathValue("inboxId"), r.PathValue("emailId"), in.AssigneeUserID)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) unassign(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromReq(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	if err := h.svc.UnassignEmail(r.Context(), tenantID, r.PathValue("inboxId"), r.PathValue("emailId")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type statusRequest struct {
	Status string `json:"status"`
}

func (h *Handlers) setStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromReq(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in statusRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.SetStatus(r.Context(), tenantID, r.PathValue("inboxId"), r.PathValue("emailId"), in.Status)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type noteRequest struct {
	AuthorUserID string `json:"author_user_id"`
	NoteText     string `json:"note_text"`
}

func (h *Handlers) addNote(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromReq(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in noteRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	author := in.AuthorUserID
	if author == "" {
		author = principal(r)
	}
	out, err := h.svc.AddNote(r.Context(), tenantID, r.PathValue("inboxId"), r.PathValue("emailId"), author, in.NoteText)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) listNotes(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromReq(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	out, err := h.svc.ListNotes(r.Context(), tenantID, r.PathValue("inboxId"), r.PathValue("emailId"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": out})
}

func (h *Handlers) listAssignments(w http.ResponseWriter, r *http.Request) {
	tenantID, err := tenantFromReq(r)
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	q := r.URL.Query()
	opts := ListAssignmentsOptions{
		Status:         q.Get("status"),
		AssigneeUserID: q.Get("assignee_user_id"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Offset = n
		}
	}
	out, err := h.svc.ListAssignments(r.Context(), tenantID, r.PathValue("inboxId"), opts)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": out})
}

func tenantFromReq(r *http.Request) (string, error) {
	id := middleware.TenantIDFrom(r.Context())
	if id == "" {
		return "", errors.New("missing tenant context")
	}
	return id, nil
}

func principal(r *http.Request) string {
	if id := middleware.KChatUserIDFrom(r.Context()); id != "" {
		return id
	}
	return middleware.StalwartAccountIDFrom(r.Context())
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
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
