// Package calendarbridge — RFC 4791 free/busy publishing.
//
// PROPOSAL.md §8.4 deferred free/busy to Phase 3+; Phase 8 ships
// the publisher: the React UI calls
// `GET /api/v1/calendars/{accountID}/{calendarID}/freebusy?start=&end=`
// to render the participant availability strip in EventCreate.tsx,
// and external clients (Apple Calendar, Outlook, Thunderbird) hit
// the public CalDAV REPORT route to discover busy times via
// `urn:ietf:params:xml:ns:caldav:calendar-freebusy`.
//
// Both surfaces share a single VFREEBUSY rendering pipeline:
//
//   1. CalDAVClient.GetEvents over the requested window.
//   2. Aggregate every VEVENT's [Start,End) into Busy intervals.
//   3. Emit RFC 5545 VFREEBUSY iCalendar.
//
// Only `BUSY` periods are emitted — KMail does not surface
// "tentative" / "free" data because Stalwart's CalDAV store does
// not retain that distinction reliably across third-party clients.
package calendarbridge

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// FreeBusyService renders VFREEBUSY responses backed by the
// existing Service.GetEvents. It keeps no state of its own.
type FreeBusyService struct {
	cal *Service
}

// NewFreeBusyService wires a publisher around the supplied Service.
func NewFreeBusyService(cal *Service) *FreeBusyService {
	return &FreeBusyService{cal: cal}
}

// BusyInterval is one busy span aggregated across the supplied
// calendar(s).
type BusyInterval struct {
	Start time.Time
	End   time.Time
}

// FreeBusyResult is the JSON-friendly response for the React UI.
// External CalDAV clients consume the iCalendar form.
type FreeBusyResult struct {
	AccountID  string         `json:"account_id"`
	CalendarID string         `json:"calendar_id"`
	Start      time.Time      `json:"start"`
	End        time.Time      `json:"end"`
	Busy       []BusyInterval `json:"busy"`
}

// Compute aggregates VEVENTs in [start,end) into BusyInterval and
// returns them sorted + merged. Adjacent / overlapping intervals
// collapse into one so external clients render a single "busy"
// block.
func (f *FreeBusyService) Compute(ctx context.Context, accountID, calendarID string, start, end time.Time) (*FreeBusyResult, error) {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return nil, fmt.Errorf("%w: start must be < end", ErrInvalidInput)
	}
	events, err := f.cal.GetEvents(ctx, accountID, calendarID, TimeRange{Start: start, End: end})
	if err != nil {
		return nil, err
	}
	intervals := make([]BusyInterval, 0, len(events))
	for _, e := range events {
		s, ee, ok := parseEventBounds(e.Start, e.End)
		if !ok {
			continue
		}
		if s.Before(start) {
			s = start
		}
		if ee.After(end) {
			ee = end
		}
		if !ee.After(s) {
			continue
		}
		intervals = append(intervals, BusyInterval{Start: s, End: ee})
	}
	merged := mergeIntervals(intervals)
	return &FreeBusyResult{
		AccountID:  accountID,
		CalendarID: calendarID,
		Start:      start,
		End:        end,
		Busy:       merged,
	}, nil
}

// AsICalendar renders an RFC 5545 VFREEBUSY component. The DTSTAMP
// is `now`; UID is deterministic per (accountID, calendarID,
// start..end) so re-published responses round-trip identically.
func (r *FreeBusyResult) AsICalendar() string {
	var b bytes.Buffer
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//KMail//Free-Busy 1.0//EN\r\n")
	b.WriteString("METHOD:REPLY\r\n")
	b.WriteString("BEGIN:VFREEBUSY\r\n")
	fmt.Fprintf(&b, "UID:%s-%s-%d-%d@kmail\r\n", r.AccountID, r.CalendarID, r.Start.Unix(), r.End.Unix())
	fmt.Fprintf(&b, "DTSTAMP:%s\r\n", time.Now().UTC().Format("20060102T150405Z"))
	fmt.Fprintf(&b, "DTSTART:%s\r\n", r.Start.UTC().Format("20060102T150405Z"))
	fmt.Fprintf(&b, "DTEND:%s\r\n", r.End.UTC().Format("20060102T150405Z"))
	for _, iv := range r.Busy {
		fmt.Fprintf(&b, "FREEBUSY;FBTYPE=BUSY:%s/%s\r\n",
			iv.Start.UTC().Format("20060102T150405Z"),
			iv.End.UTC().Format("20060102T150405Z"),
		)
	}
	b.WriteString("END:VFREEBUSY\r\n")
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// parseEventBounds parses Event.Start / End strings (RFC3339 or
// RFC5545 basic). Returns (start, end, true) when both bounds
// parse and are ordered; otherwise (_, _, false).
func parseEventBounds(startStr, endStr string) (time.Time, time.Time, bool) {
	formats := []string{time.RFC3339, "20060102T150405Z", "20060102T150405", "20060102"}
	parse := func(s string) (time.Time, bool) {
		for _, f := range formats {
			if t, err := time.Parse(f, s); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	s, ok := parse(startStr)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	e, ok := parse(endStr)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if !e.After(s) {
		return time.Time{}, time.Time{}, false
	}
	return s, e, true
}

// mergeIntervals sorts by Start then folds overlapping / abutting
// spans into a minimal cover.
func mergeIntervals(in []BusyInterval) []BusyInterval {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Start.Before(in[j].Start) })
	out := []BusyInterval{in[0]}
	for _, cur := range in[1:] {
		last := &out[len(out)-1]
		if !cur.Start.After(last.End) {
			if cur.End.After(last.End) {
				last.End = cur.End
			}
			continue
		}
		out = append(out, cur)
	}
	return out
}

// CalDAVDiscoveryDocument is the body served from
// `/.well-known/caldav` (per RFC 6764). KMail surfaces a static
// document that points to the BFF's calendar root and advertises
// the CalDAV REPORT endpoint for free/busy queries.
func CalDAVDiscoveryDocument(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = "/api/v1/calendars"
	} else {
		baseURL = baseURL + "/api/v1/calendars"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<service-document xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` + "\n")
	fmt.Fprintf(&b, `  <href>%s</href>`+"\n", baseURL)
	b.WriteString(`  <C:calendar-home-set><href>` + baseURL + `</href></C:calendar-home-set>` + "\n")
	b.WriteString(`  <C:supported-calendar-data>` + "\n")
	b.WriteString(`    <C:calendar-data content-type="text/calendar" version="2.0"/>` + "\n")
	b.WriteString(`  </C:supported-calendar-data>` + "\n")
	b.WriteString(`</service-document>` + "\n")
	return b.String()
}
