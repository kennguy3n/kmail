package webhooks

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires webhook admin routes.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers builds Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs the admin webhook routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/webhooks", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/webhooks", authMW.Wrap(http.HandlerFunc(h.register)))
	mux.Handle("DELETE /api/v1/tenants/{id}/webhooks/{webhookId}", authMW.Wrap(http.HandlerFunc(h.del)))
	mux.Handle("GET /api/v1/tenants/{id}/webhook-deliveries", authMW.Wrap(http.HandlerFunc(h.deliveries)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.ListWebhooks(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []Endpoint{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) register(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ep, secret, err := h.svc.RegisterWebhook(r.Context(), tenantID, in.URL, in.Events)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"endpoint": ep, "secret": secret})
}

func (h *Handlers) del(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	id := r.PathValue("webhookId")
	if err := h.svc.DeleteWebhook(r.Context(), tenantID, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) deliveries(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := h.svc.ListDeliveries(r.Context(), tenantID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []Delivery{}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
