package chatbridge

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Registrar matches the auth-middleware shape every package uses.
type Registrar interface {
	Wrap(h http.Handler) http.Handler
}

// Handlers mounts the chat-bridge HTTP surface on a std-lib mux.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers builds the handler facade.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register wires the handlers onto the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, auth Registrar) {
	mux.Handle("POST /api/v1/chat-bridge/share", auth.Wrap(http.HandlerFunc(h.share)))
	mux.Handle("GET /api/v1/chat-bridge/routes", auth.Wrap(http.HandlerFunc(h.listRoutes)))
	mux.Handle("POST /api/v1/chat-bridge/routes", auth.Wrap(http.HandlerFunc(h.createRoute)))
	mux.Handle("DELETE /api/v1/chat-bridge/routes/{routeID}", auth.Wrap(http.HandlerFunc(h.deleteRoute)))
}

type shareRequest struct {
	EmailID   string `json:"emailId"`
	ChannelID string `json:"channelId"`
}

func (h *Handlers) share(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	userID := middleware.KChatUserIDFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in shareRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.ShareEmailToChannel(r.Context(), tenantID, in.EmailID, in.ChannelID, userID); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) listRoutes(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	routes, err := h.svc.ListRoutes(r.Context(), tenantID)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": routes})
}

type createRouteRequest struct {
	AliasAddress string `json:"aliasAddress"`
	ChannelID    string `json:"channelId"`
}

func (h *Handlers) createRoute(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in createRouteRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	route, err := h.svc.ConfigureAlertRoute(r.Context(), tenantID, in.AliasAddress, in.ChannelID)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, route)
}

func (h *Handlers) deleteRoute(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	routeID := r.PathValue("routeID")
	if err := h.svc.DeleteRoute(r.Context(), tenantID, routeID); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		h.logger.Printf("chatbridge: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}
