package monitoring

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the SLO tracker over `/api/v1/admin/slo`.
type Handlers struct {
	tracker *SLOTracker
	logger  *log.Logger
}

// NewHandlers returns Handlers bound to the given tracker.
func NewHandlers(t *SLOTracker, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{tracker: t, logger: logger}
}

// Register installs the admin SLO routes onto the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/admin/slo", authMW.Wrap(http.HandlerFunc(h.platform)))
	mux.Handle("GET /api/v1/admin/slo/breaches", authMW.Wrap(http.HandlerFunc(h.breaches)))
	mux.Handle("GET /api/v1/admin/slo/{tenantId}", authMW.Wrap(http.HandlerFunc(h.tenant)))
}

func (h *Handlers) platform(w http.ResponseWriter, r *http.Request) {
	h.respond(w, r, "")
}

func (h *Handlers) tenant(w http.ResponseWriter, r *http.Request) {
	h.respond(w, r, r.PathValue("tenantId"))
}

func (h *Handlers) respond(w http.ResponseWriter, r *http.Request, tenantID string) {
	avail, err := h.tracker.GetAvailability(r.Context(), tenantID, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lat, err := h.tracker.GetLatencyPercentiles(r.Context(), tenantID, 24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"availability": avail,
		"latency":      lat,
	})
}

func (h *Handlers) breaches(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	out, err := h.tracker.ListSLOBreaches(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []SLOBreach{}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
