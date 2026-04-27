// Package calendarbridge — HTTP surface for free/busy.
package calendarbridge

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// FreeBusyHandlers exposes the free/busy publisher to the BFF mux.
type FreeBusyHandlers struct {
	svc     *FreeBusyService
	baseURL string
	logger  *log.Logger
}

// NewFreeBusyHandlers builds the handlers. `baseURL` is the
// public-facing origin (used to render the discovery document).
func NewFreeBusyHandlers(svc *FreeBusyService, baseURL string, logger *log.Logger) *FreeBusyHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &FreeBusyHandlers{svc: svc, baseURL: strings.TrimRight(baseURL, "/"), logger: logger}
}

// Register binds:
//
//   GET    /.well-known/caldav                                     (public)
//   GET    /api/v1/calendars/{accountID}/{calendarID}/freebusy     (auth)
//   REPORT /api/v1/calendars/{accountID}/{calendarID}              (auth — calendar-freebusy)
func (h *FreeBusyHandlers) Register(mux *http.ServeMux, auth Registrar) {
	mux.HandleFunc("GET /.well-known/caldav", h.discovery)
	mux.Handle("GET /api/v1/calendars/{accountID}/{calendarID}/freebusy", auth.Wrap(http.HandlerFunc(h.json)))
	mux.Handle("REPORT /api/v1/calendars/{accountID}/{calendarID}", auth.Wrap(http.HandlerFunc(h.report)))
}

func (h *FreeBusyHandlers) discovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, CalDAVDiscoveryDocument(h.baseURL))
}

func (h *FreeBusyHandlers) json(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountID")
	calendarID := r.PathValue("calendarID")
	start, end, err := parseFreeBusyWindow(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := h.svc.Compute(r.Context(), accountID, calendarID, start, end)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			h.logger.Printf("freebusy.json: %v", err)
			http.Error(w, "internal error", http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// report handles CalDAV `REPORT` with an
// `urn:ietf:params:xml:ns:caldav:calendar-freebusy` payload.
// External clients (Apple Calendar, Outlook, Thunderbird) issue
// this verb to ask "what is X busy with between A and B?".
func (h *FreeBusyHandlers) report(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		XMLName xml.Name `xml:"calendar-freebusy"`
		TimeRange struct {
			Start string `xml:"start,attr"`
			End   string `xml:"end,attr"`
		} `xml:"time-range"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse XML: "+err.Error(), http.StatusBadRequest)
		return
	}
	start, errS := time.Parse("20060102T150405Z", req.TimeRange.Start)
	end, errE := time.Parse("20060102T150405Z", req.TimeRange.End)
	if errS != nil || errE != nil {
		http.Error(w, "calendar-freebusy time-range start/end must be in iCal UTC form", http.StatusBadRequest)
		return
	}
	res, err := h.svc.Compute(r.Context(), r.PathValue("accountID"), r.PathValue("calendarID"), start, end)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	_, _ = io.WriteString(w, res.AsICalendar())
}

// parseFreeBusyWindow accepts ?start=&end=. If end is missing,
// defaults to start+24h. If start is missing, defaults to now.
func parseFreeBusyWindow(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	var start, end time.Time
	if s := q.Get("start"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("start must be RFC3339")
		}
		start = t
	} else {
		start = time.Now()
	}
	if e := q.Get("end"); e != "" {
		t, err := time.Parse(time.RFC3339, e)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("end must be RFC3339")
		}
		end = t
	} else {
		end = start.Add(24 * time.Hour)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("end must be after start")
	}
	return start, end, nil
}
