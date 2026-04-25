package cmk

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires the CMK admin routes.
type Handlers struct {
	svc    *CMKService
	pool   *pgxpool.Pool
	logger *log.Logger
}

// NewHandlers returns Handlers. `pool` is used for the per-request
// `SELECT plan FROM tenants` lookup so the service doesn't have to
// embed a tenant-service dependency.
func NewHandlers(svc *CMKService, pool *pgxpool.Pool, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, pool: pool, logger: logger}
}

// Register installs the routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/cmk", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/cmk", authMW.Wrap(http.HandlerFunc(h.register)))
	mux.Handle("PUT /api/v1/tenants/{id}/cmk/{keyId}/rotate", authMW.Wrap(http.HandlerFunc(h.rotate)))
	mux.Handle("DELETE /api/v1/tenants/{id}/cmk/{keyId}/revoke", authMW.Wrap(http.HandlerFunc(h.revoke)))
	mux.Handle("GET /api/v1/tenants/{id}/cmk/active", authMW.Wrap(http.HandlerFunc(h.active)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.ListKeys(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []Key{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) register(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	plan, err := h.lookupPlan(r, tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var in struct {
		PublicKeyPEM string `json:"public_key_pem"`
		Algorithm    string `json:"algorithm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.RegisterKey(r.Context(), tenantID, plan, in.PublicKeyPEM, in.Algorithm)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrPlanNotEligible) {
			status = http.StatusForbidden
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) rotate(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	plan, err := h.lookupPlan(r, tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var in struct {
		PublicKeyPEM string `json:"public_key_pem"`
		Algorithm    string `json:"algorithm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.RotateKey(r.Context(), tenantID, plan, in.PublicKeyPEM, in.Algorithm)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrPlanNotEligible) {
			status = http.StatusForbidden
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) revoke(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	keyID := r.PathValue("keyId")
	if err := h.svc.RevokeKey(r.Context(), tenantID, keyID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) active(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.GetActiveKey(r.Context(), tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		writeJSON(w, http.StatusOK, map[string]any{"key": nil})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) lookupPlan(r *http.Request, tenantID string) (string, error) {
	if h.pool == nil {
		return "", nil
	}
	var plan string
	if err := h.pool.QueryRow(r.Context(),
		`SELECT plan FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&plan); err != nil {
		return "", err
	}
	return plan, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
