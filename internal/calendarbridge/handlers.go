package calendarbridge

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Registrar is the subset of the OIDC middleware Handlers consume.
// Matching the `tenant.Registrar` shape lets the HTTP mux-wiring
// code treat every package's handlers identically.
type Registrar interface {
	Wrap(h http.Handler) http.Handler
}

// Handlers wires the Calendar Bridge into the BFF's HTTP mux.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers constructs the Handlers facade.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register mounts calendar routes under `/api/v1/calendars/...`.
// Every route runs behind the provided auth middleware so the
// tenant-scoping contract from `docs/SCHEMA.md` §4 is preserved.
func (h *Handlers) Register(mux *http.ServeMux, auth Registrar) {
	mux.Handle("GET /api/v1/calendars/{accountID}", auth.Wrap(http.HandlerFunc(h.listCalendars)))
	mux.Handle("GET /api/v1/calendars/{accountID}/{calendarID}/events", auth.Wrap(http.HandlerFunc(h.listEvents)))
	mux.Handle("POST /api/v1/calendars/{accountID}/{calendarID}/events", auth.Wrap(http.HandlerFunc(h.createEvent)))
	mux.Handle("PUT /api/v1/calendars/{accountID}/{calendarID}/events/{eventUID}", auth.Wrap(http.HandlerFunc(h.updateEvent)))
	mux.Handle("DELETE /api/v1/calendars/{accountID}/{calendarID}/events/{eventUID}", auth.Wrap(http.HandlerFunc(h.deleteEvent)))
	mux.Handle("POST /api/v1/calendars/{accountID}/{calendarID}/events/{eventUID}/respond", auth.Wrap(http.HandlerFunc(h.respondEvent)))
}

func (h *Handlers) listCalendars(w http.ResponseWriter, r *http.Request) {
	accountID := accountIDFromRequest(r)
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "accountID required")
		return
	}
	out, err := h.svc.ListCalendars(r.Context(), accountID)
	h.writeResult(w, out, err)
}

func (h *Handlers) listEvents(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	calendarID := r.PathValue("calendarID")
	q := r.URL.Query()
	tr := TimeRange{}
	if s := q.Get("start"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			tr.Start = t
		}
	}
	if e := q.Get("end"); e != "" {
		if t, err := time.Parse(time.RFC3339, e); err == nil {
			tr.End = t
		}
	}
	events, err := h.svc.GetEvents(r.Context(), accountID, calendarID, tr)
	h.writeResult(w, events, err)
}

type createEventRequest struct {
	ICalData string `json:"icalData"`
}

func (h *Handlers) createEvent(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	calendarID := r.PathValue("calendarID")
	body, err := readJSONBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in createEventRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	uid, err := h.svc.CreateEvent(r.Context(), accountID, calendarID, in.ICalData)
	if err != nil {
		h.respondError(w, err)
		return
	}
	h.writeResult(w, map[string]string{"uid": uid}, nil)
}

func (h *Handlers) updateEvent(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	calendarID := r.PathValue("calendarID")
	eventUID := r.PathValue("eventUID")
	body, err := readJSONBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in createEventRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateEvent(r.Context(), accountID, calendarID, eventUID, in.ICalData); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) deleteEvent(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	calendarID := r.PathValue("calendarID")
	eventUID := r.PathValue("eventUID")
	if err := h.svc.DeleteEvent(r.Context(), accountID, calendarID, eventUID); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type respondRequest struct {
	Participant string              `json:"participant"`
	Response    ParticipantResponse `json:"response"`
}

func (h *Handlers) respondEvent(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	calendarID := r.PathValue("calendarID")
	eventUID := r.PathValue("eventUID")
	body, err := readJSONBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in respondRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.RespondToEvent(r.Context(), accountID, calendarID, eventUID, in.Participant, in.Response); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) writeResult(w http.ResponseWriter, v any, err error) {
	if err != nil {
		h.respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Printf("calendarbridge: encode response: %v", err)
	}
}

func (h *Handlers) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		h.logger.Printf("calendarbridge: %v", err)
		writeError(w, http.StatusBadGateway, "upstream error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSONBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

// accountIDFromRequest prefers the resolved Stalwart account ID
// from the auth middleware context and falls back to the path
// variable. This lets JMAP-first clients omit the segment when the
// BFF already knows which account to route to.
func accountIDFromRequest(r *http.Request) string {
	accountID := r.PathValue("accountID")
	if accountID != "" {
		return accountID
	}
	return strings.TrimSpace(middleware.StalwartAccountIDFrom(r.Context()))
}
