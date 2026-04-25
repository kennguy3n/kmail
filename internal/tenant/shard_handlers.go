package tenant

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ShardHandlers exposes the admin shard-routing API.
type ShardHandlers struct {
	svc *ShardService
}

// NewShardHandlers returns ShardHandlers.
func NewShardHandlers(svc *ShardService) *ShardHandlers {
	return &ShardHandlers{svc: svc}
}

// Register mounts shard routes on the mux.
func (h *ShardHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/admin/shards",
		authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/admin/shards",
		authMW.Wrap(http.HandlerFunc(h.register)))
	mux.Handle("GET /api/v1/admin/shards/{id}",
		authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("PUT /api/v1/admin/shards/{id}",
		authMW.Wrap(http.HandlerFunc(h.update)))
	mux.Handle("POST /api/v1/admin/shards/{id}/rebalance",
		authMW.Wrap(http.HandlerFunc(h.rebalance)))
}

func (h *ShardHandlers) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListShards(r.Context())
	if err != nil {
		shardErr(w, http.StatusInternalServerError, err)
		return
	}
	shardJSON(w, http.StatusOK, map[string]any{"shards": out})
}

func (h *ShardHandlers) register(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		shardErr(w, http.StatusBadRequest, err)
		return
	}
	var in Shard
	if err := json.Unmarshal(body, &in); err != nil {
		shardErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.RegisterShard(r.Context(), in)
	if err != nil {
		shardErr(w, http.StatusInternalServerError, err)
		return
	}
	shardJSON(w, http.StatusCreated, out)
}

func (h *ShardHandlers) get(w http.ResponseWriter, r *http.Request) {
	shard, err := h.svc.GetShard(r.Context(), r.PathValue("id"))
	if err != nil {
		shardErr(w, http.StatusNotFound, err)
		return
	}
	tenants, err := h.svc.ListTenantsOnShard(r.Context(), shard.ID)
	if err != nil {
		shardErr(w, http.StatusInternalServerError, err)
		return
	}
	shardJSON(w, http.StatusOK, map[string]any{"shard": shard, "tenants": tenants})
}

func (h *ShardHandlers) update(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		shardErr(w, http.StatusBadRequest, err)
		return
	}
	var in Shard
	if err := json.Unmarshal(body, &in); err != nil {
		shardErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.UpdateShard(r.Context(), r.PathValue("id"), in)
	if err != nil {
		shardErr(w, http.StatusInternalServerError, err)
		return
	}
	shardJSON(w, http.StatusOK, out)
}

type rebalanceRequest struct {
	TenantID    string `json:"tenant_id"`
	FromShardID string `json:"from_shard_id"`
}

func (h *ShardHandlers) rebalance(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		shardErr(w, http.StatusBadRequest, err)
		return
	}
	var in rebalanceRequest
	if err := json.Unmarshal(body, &in); err != nil {
		shardErr(w, http.StatusBadRequest, err)
		return
	}
	if in.TenantID == "" {
		shardErr(w, http.StatusBadRequest, errors.New("tenant_id required"))
		return
	}
	out, err := h.svc.RebalanceShard(r.Context(), in.FromShardID, r.PathValue("id"), in.TenantID)
	if err != nil {
		shardErr(w, http.StatusInternalServerError, err)
		return
	}
	shardJSON(w, http.StatusOK, out)
}

func shardJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func shardErr(w http.ResponseWriter, status int, err error) {
	shardJSON(w, status, map[string]string{"error": err.Error()})
}
