package approval

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers is the HTTP surface.
type Handlers struct {
	svc *Service
}

// NewHandlers returns Handlers.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Register installs the routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/approvals", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/approvals", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("POST /api/v1/tenants/{id}/approvals/{approvalId}/approve", authMW.Wrap(http.HandlerFunc(h.approve)))
	mux.Handle("POST /api/v1/tenants/{id}/approvals/{approvalId}/reject", authMW.Wrap(http.HandlerFunc(h.reject)))
	mux.Handle("POST /api/v1/tenants/{id}/approvals/{approvalId}/execute", authMW.Wrap(http.HandlerFunc(h.execute)))
	mux.Handle("GET /api/v1/tenants/{id}/approvals/config", authMW.Wrap(http.HandlerFunc(h.config)))
	mux.Handle("PUT /api/v1/tenants/{id}/approvals/config", authMW.Wrap(http.HandlerFunc(h.setConfig)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	status := r.URL.Query().Get("status")
	var (
		out []Request
		err error
	)
	switch status {
	case "pending":
		out, err = h.svc.ListPendingRequests(r.Context(), tenantID)
	default:
		out, err = h.svc.ListAll(r.Context(), tenantID, 50)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []Request{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in struct {
		RequesterID    string `json:"requester_id"`
		Action         string `json:"action"`
		TargetResource string `json:"target_resource"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.svc.CreateRequest(r.Context(), tenantID, in.RequesterID, in.Action, in.TargetResource)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) approve(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	id := r.PathValue("approvalId")
	var in struct {
		ApproverID string `json:"approver_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	out, err := h.svc.ApproveRequest(r.Context(), tenantID, id, in.ApproverID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) reject(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	id := r.PathValue("approvalId")
	var in struct {
		ApproverID string `json:"approver_id"`
		Reason     string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	out, err := h.svc.RejectRequest(r.Context(), tenantID, id, in.ApproverID, in.Reason)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) execute(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	id := r.PathValue("approvalId")
	if err := h.svc.ExecuteApproved(r.Context(), tenantID, id); err != nil {
		status := http.StatusBadRequest
		if err == ErrNoExecutor {
			status = http.StatusNotImplemented
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "executed"})
}

func (h *Handlers) config(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.ListActionConfig(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = map[string]bool{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) setConfig(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in map[string]bool
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	for action, requires := range in {
		if err := h.svc.SetActionConfig(r.Context(), tenantID, action, requires); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, in)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
