package calendarbridge

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// SharingHandlers wires the calendar-sharing / resource-calendar
// routes onto the BFF mux.
type SharingHandlers struct {
	svc   *Service
	store *SharingStore
}

// NewSharingHandlers returns SharingHandlers.
func NewSharingHandlers(svc *Service, store *SharingStore) *SharingHandlers {
	return &SharingHandlers{svc: svc, store: store}
}

// Register mounts the sharing / resource-calendar routes.
func (h *SharingHandlers) Register(mux *http.ServeMux, auth Registrar) {
	mux.Handle("POST /api/v1/calendars",
		auth.Wrap(http.HandlerFunc(h.createCalendar)))
	mux.Handle("PUT /api/v1/calendars/{id}",
		auth.Wrap(http.HandlerFunc(h.updateCalendar)))
	mux.Handle("DELETE /api/v1/calendars/{id}",
		auth.Wrap(http.HandlerFunc(h.deleteCalendar)))
	mux.Handle("POST /api/v1/calendars/{id}/share",
		auth.Wrap(http.HandlerFunc(h.shareCalendar)))
	mux.Handle("GET /api/v1/calendars/shared",
		auth.Wrap(http.HandlerFunc(h.listShared)))
	mux.Handle("POST /api/v1/calendars/{id}/book",
		auth.Wrap(http.HandlerFunc(h.bookResource)))
	mux.Handle("GET /api/v1/resource-calendars",
		auth.Wrap(http.HandlerFunc(h.listResources)))
	mux.Handle("POST /api/v1/resource-calendars",
		auth.Wrap(http.HandlerFunc(h.createResource)))
}

type createCalendarRequest struct {
	Name         string       `json:"name"`
	CalendarType CalendarType `json:"calendar_type"`
	Description  string       `json:"description,omitempty"`
	Color        string       `json:"color,omitempty"`
}

func (h *SharingHandlers) createCalendar(w http.ResponseWriter, r *http.Request) {
	accountID := principalAccountID(r)
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in createCalendarRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cal, err := h.svc.CreateCalendar(r.Context(), accountID, in.Name, in.CalendarType)
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, cal)
}

func (h *SharingHandlers) updateCalendar(w http.ResponseWriter, r *http.Request) {
	accountID := principalAccountID(r)
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in UpdateCalendarInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cal, err := h.svc.UpdateCalendar(r.Context(), accountID, r.PathValue("id"), in)
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, cal)
}

func (h *SharingHandlers) deleteCalendar(w http.ResponseWriter, r *http.Request) {
	accountID := principalAccountID(r)
	if err := h.svc.DeleteCalendar(r.Context(), accountID, r.PathValue("id")); err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type shareRequest struct {
	TargetAccountID string `json:"target_account_id"`
	Permission      string `json:"permission"`
}

func (h *SharingHandlers) shareCalendar(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	accountID := principalAccountID(r)
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in shareRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	share, err := h.store.ShareCalendar(r.Context(), tenantID, accountID, r.PathValue("id"), in.TargetAccountID, in.Permission)
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, share)
}

func (h *SharingHandlers) listShared(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	shares, err := h.store.ListSharedCalendars(r.Context(), tenantID, principalAccountID(r))
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, map[string]any{"shares": shares})
}

func (h *SharingHandlers) bookResource(w http.ResponseWriter, r *http.Request) {
	accountID := principalAccountID(r)
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in BookResourceInput
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	uid, err := h.svc.BookResource(r.Context(), accountID, r.PathValue("id"), in)
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, map[string]string{"uid": uid})
}

func (h *SharingHandlers) listResources(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	out, err := h.store.ListResourceCalendars(r.Context(), tenantID)
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, map[string]any{"resources": out})
}

func (h *SharingHandlers) createResource(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in ResourceCalendar
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.store.CreateResourceCalendar(r.Context(), tenantID, in)
	if err != nil {
		writeErr(w, statusFromErr(err), err)
		return
	}
	writeOK(w, out)
}

func principalAccountID(r *http.Request) string {
	if id := middleware.StalwartAccountIDFrom(r.Context()); id != "" {
		return id
	}
	return r.PathValue("accountID")
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func writeOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func statusFromErr(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
