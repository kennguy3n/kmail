package deliverability

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the Deliverability Control Plane over REST.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns Handlers bound to the provided Service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register mounts every deliverability route.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	// Suppression lists
	mux.Handle("GET /api/v1/tenants/{id}/suppression", authMW.Wrap(http.HandlerFunc(h.listSuppression)))
	mux.Handle("POST /api/v1/tenants/{id}/suppression", authMW.Wrap(http.HandlerFunc(h.addSuppression)))
	mux.Handle("DELETE /api/v1/tenants/{id}/suppression/{email}", authMW.Wrap(http.HandlerFunc(h.removeSuppression)))

	// Bounces
	mux.Handle("GET /api/v1/tenants/{id}/bounces", authMW.Wrap(http.HandlerFunc(h.listBounces)))
	mux.Handle("POST /api/v1/tenants/{id}/bounces", authMW.Wrap(http.HandlerFunc(h.recordBounce)))

	// IP pools (admin, global)
	mux.Handle("GET /api/v1/admin/ip-pools", authMW.Wrap(http.HandlerFunc(h.listPools)))
	mux.Handle("POST /api/v1/admin/ip-pools", authMW.Wrap(http.HandlerFunc(h.createPool)))
	mux.Handle("POST /api/v1/admin/ip-pools/{id}/ips", authMW.Wrap(http.HandlerFunc(h.addIP)))
	mux.Handle("GET /api/v1/admin/ip-pools/{id}/ips", authMW.Wrap(http.HandlerFunc(h.listIPs)))

	// Tenant pool assignment
	mux.Handle("GET /api/v1/tenants/{id}/ip-pool", authMW.Wrap(http.HandlerFunc(h.getTenantPool)))
	mux.Handle("POST /api/v1/tenants/{id}/ip-pool", authMW.Wrap(http.HandlerFunc(h.assignTenantPool)))

	// Send limits + warmup
	mux.Handle("GET /api/v1/tenants/{id}/send-limit", authMW.Wrap(http.HandlerFunc(h.getSendLimit)))
	mux.Handle("PATCH /api/v1/tenants/{id}/send-limit", authMW.Wrap(http.HandlerFunc(h.setSendLimit)))
	mux.Handle("GET /api/v1/tenants/{id}/warmup", authMW.Wrap(http.HandlerFunc(h.getWarmup)))

	// DMARC reports
	mux.Handle("POST /api/v1/tenants/{id}/dmarc-reports", authMW.Wrap(http.HandlerFunc(h.uploadDMARC)))
	mux.Handle("GET /api/v1/tenants/{id}/dmarc-reports", authMW.Wrap(http.HandlerFunc(h.listDMARC)))
	mux.Handle("GET /api/v1/tenants/{id}/dmarc-reports/summary", authMW.Wrap(http.HandlerFunc(h.dmarcSummary)))
}

// -- Suppression ---------------------------------------------------

func (h *Handlers) listSuppression(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	opts := ListSuppressionsOptions{
		Reason: r.URL.Query().Get("reason"),
		Limit:  atoiDefault(r.URL.Query().Get("limit"), 100),
		Offset: atoiDefault(r.URL.Query().Get("offset"), 0),
	}
	out, err := h.svc.Suppression.ListSuppressions(r.Context(), tenantID, opts)
	if err != nil {
		h.logger.Printf("suppression.list: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) addSuppression(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in struct {
		Email  string `json:"email"`
		Reason string `json:"reason"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	row, err := h.svc.Suppression.AddSuppression(r.Context(), tenantID, in.Email, in.Reason, in.Source)
	if err != nil {
		h.logger.Printf("suppression.add: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *Handlers) removeSuppression(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	email := r.PathValue("email")
	if err := h.svc.Suppression.RemoveSuppression(r.Context(), tenantID, email); err != nil {
		h.logger.Printf("suppression.remove: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- Bounces -------------------------------------------------------

func (h *Handlers) listBounces(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	out, err := h.svc.Bounce.ListBounces(r.Context(), tenantID,
		atoiDefault(r.URL.Query().Get("limit"), 100),
		atoiDefault(r.URL.Query().Get("offset"), 0),
	)
	if err != nil {
		h.logger.Printf("bounce.list: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) recordBounce(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var evt BounceEvent
	if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.Bounce.ProcessBounce(r.Context(), tenantID, evt)
	if err != nil {
		h.logger.Printf("bounce.record: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// -- IP pools ------------------------------------------------------

func (h *Handlers) listPools(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.IPPool.ListPools(r.Context())
	if err != nil {
		h.logger.Printf("ippool.list: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) createPool(w http.ResponseWriter, r *http.Request) {
	var in CreatePoolInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.IPPool.CreatePool(r.Context(), in)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) addIP(w http.ResponseWriter, r *http.Request) {
	poolID := r.PathValue("id")
	var in AddIPInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.IPPool.AddIP(r.Context(), poolID, in)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) listIPs(w http.ResponseWriter, r *http.Request) {
	poolID := r.PathValue("id")
	out, err := h.svc.IPPool.ListIPs(r.Context(), poolID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) getTenantPool(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	out, err := h.svc.IPPool.GetTenantPool(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) assignTenantPool(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in struct {
		PoolType string `json:"pool_type"`
		Priority int    `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.svc.IPPool.AssignTenantPool(r.Context(), tenantID, in.PoolType, in.Priority); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- Send limits / warmup -----------------------------------------

func (h *Handlers) getSendLimit(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	lim, err := h.svc.SendLimit.GetLimit(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, lim)
}

func (h *Handlers) setSendLimit(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in struct {
		DailyLimit  int `json:"daily_limit"`
		HourlyLimit int `json:"hourly_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.svc.SendLimit.SetLimit(r.Context(), tenantID, in.DailyLimit, in.HourlyLimit); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) getWarmup(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	cap, err := h.svc.Warmup.GetCurrentLimit(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"current_daily_limit": cap})
}

// -- DMARC ---------------------------------------------------------

func (h *Handlers) uploadDMARC(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 25*1024*1024)) // 25MB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.DMARC.IngestReport(r.Context(), tenantID, body)
	if err != nil {
		h.logger.Printf("dmarc.ingest: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) listDMARC(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	out, err := h.svc.DMARC.ListReports(r.Context(), tenantID,
		r.URL.Query().Get("domain_id"),
		atoiDefault(r.URL.Query().Get("limit"), 50),
		atoiDefault(r.URL.Query().Get("offset"), 0),
	)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) dmarcSummary(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	out, err := h.svc.DMARC.GetReportSummary(r.Context(), tenantID, r.URL.Query().Get("domain_id"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// -- helpers -------------------------------------------------------

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrSuppressed):
		return http.StatusForbidden
	case errors.Is(err, ErrSendLimitExceeded):
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func checkScope(r *http.Request, pathTenantID string) error {
	ctxTenantID := middleware.TenantIDFrom(r.Context())
	if ctxTenantID == "" {
		return errors.New("missing tenant context")
	}
	if ctxTenantID != pathTenantID {
		return errors.New("cross-tenant access forbidden")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func atoiDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
