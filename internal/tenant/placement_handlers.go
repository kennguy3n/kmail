package tenant

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// PlacementHandlers wires the storage placement admin endpoints.
type PlacementHandlers struct {
	svc  *PlacementService
	pool *pgxpool.Pool
}

// NewPlacementHandlers returns the handlers.
func NewPlacementHandlers(svc *PlacementService, pool *pgxpool.Pool) *PlacementHandlers {
	return &PlacementHandlers{svc: svc, pool: pool}
}

// Register installs the routes.
func (h *PlacementHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/storage/regions", authMW.Wrap(http.HandlerFunc(h.regions)))
	mux.Handle("GET /api/v1/tenants/{id}/storage/placement", authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("PUT /api/v1/tenants/{id}/storage/placement", authMW.Wrap(http.HandlerFunc(h.put)))
}

func (h *PlacementHandlers) regions(w http.ResponseWriter, r *http.Request) {
	writeJSONStatus(w, http.StatusOK, h.svc.ListAvailableRegions())
}

func (h *PlacementHandlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.GetPlacementPolicy(r.Context(), tenantID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSONStatus(w, http.StatusOK, out)
}

func (h *PlacementHandlers) put(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in PlacementPolicy
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	plan, _ := h.lookupPlan(r, tenantID)
	out, err := h.svc.UpdatePlacementPolicy(r.Context(), tenantID, plan, in)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSONStatus(w, http.StatusOK, out)
}

func (h *PlacementHandlers) lookupPlan(r *http.Request, tenantID string) (string, error) {
	if h.pool == nil {
		return "", nil
	}
	var plan string
	err := h.pool.QueryRow(r.Context(), `SELECT plan FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&plan)
	return plan, err
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
