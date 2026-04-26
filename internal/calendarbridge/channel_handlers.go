package calendarbridge

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ChannelHandlers exposes per-calendar notification channel
// configuration. Mounted under `/api/v1/calendars/...`.
type ChannelHandlers struct {
	resolver *DBChannelResolver
}

// NewChannelHandlers builds ChannelHandlers.
func NewChannelHandlers(resolver *DBChannelResolver) *ChannelHandlers {
	return &ChannelHandlers{resolver: resolver}
}

// Register installs the per-calendar channel routes.
func (h *ChannelHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/calendars/{calendarId}/notification-channel", authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("PUT /api/v1/calendars/{calendarId}/notification-channel", authMW.Wrap(http.HandlerFunc(h.put)))
	mux.Handle("DELETE /api/v1/calendars/{calendarId}/notification-channel", authMW.Wrap(http.HandlerFunc(h.del)))
	mux.Handle("GET /api/v1/tenants/{id}/calendar-default-channel", authMW.Wrap(http.HandlerFunc(h.getDefault)))
	mux.Handle("PUT /api/v1/tenants/{id}/calendar-default-channel", authMW.Wrap(http.HandlerFunc(h.putDefault)))
}

func (h *ChannelHandlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	calendarID := r.PathValue("calendarId")
	out, err := h.resolver.GetCalendarChannel(r.Context(), tenantID, calendarID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		writeJSON(w, http.StatusOK, map[string]any{"channel_id": "", "configured": false})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ChannelHandlers) put(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	calendarID := r.PathValue("calendarId")
	var in struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.resolver.SetCalendarChannel(r.Context(), tenantID, calendarID, in.ChannelID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ChannelHandlers) del(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	calendarID := r.PathValue("calendarId")
	if err := h.resolver.DeleteCalendarChannel(r.Context(), tenantID, calendarID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ChannelHandlers) getDefault(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.resolver.GetCalendarChannel(r.Context(), tenantID, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		writeJSON(w, http.StatusOK, map[string]any{"channel_id": "", "configured": false})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ChannelHandlers) putDefault(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.resolver.SetCalendarChannel(r.Context(), tenantID, "", in.ChannelID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
