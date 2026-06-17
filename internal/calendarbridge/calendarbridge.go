// Package calendarbridge hosts the Calendar Bridge business
// logic — the calendar side of the "KMail lives inside KChat"
// integration surface.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): meeting creation from chat threads,
// RSVP-as-chat, resource calendars, and scheduling assistants.
// Talks to Stalwart CalDAV and the KChat API.
//
// Stalwart v0.16.0 ships CalDAV but does not yet advertise a
// `urn:ietf:params:jmap:calendars` capability. The Go BFF proxies
// CalDAV requests to Stalwart on behalf of the React client, which
// speaks a JMAP-shaped surface layered on top. Once Stalwart
// exposes JMAP calendars natively the proxy can fall away without
// a client change.
package calendarbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// Config wires the CalDAV proxy.
type Config struct {
	// StalwartURL is the base URL for Stalwart's CalDAV endpoint
	// (e.g. `http://stalwart:8080`). The bridge appends the
	// per-principal calendar-home path on each request.
	StalwartURL string
	// AdminUser is the Basic-auth username the bridge presents to
	// Stalwart on behalf of the authenticated principal. Empty in
	// JMAP-first deployments that re-use the caller's bearer
	// token; populated when the bridge is hoisted onto a dedicated
	// service account.
	AdminUser string
	// AdminPassword pairs with AdminUser. Never logged.
	AdminPassword string
	// HTTPClient overrides the HTTP client used for CalDAV
	// requests (timeouts, transport tuning). Defaults to an
	// http.Client with a 15s timeout.
	HTTPClient *http.Client
	// Logger, when set, records CalDAV account-email resolution
	// failures (see davAccount). A hard lookup failure means the
	// bridge falls back to the raw JMAP id, which Stalwart cannot
	// resolve to a DAV principal — so every CalDAV call for that
	// principal 404s. Logging makes a misconfigured admin credential or an
	// unreachable Stalwart visible instead of silently degrading.
	// nil disables this logging (e.g. the JMAP-first prod path, where
	// davAccount is a no-op because no admin credential is set).
	Logger *log.Logger
}

// Service is the Calendar Bridge. One instance per process is
// enough — the proxy is stateless across requests.
type Service struct {
	cfg Config
	// emailCache memoises JMAP account id -> CalDAV account email
	// lookups (see davAccount). The mapping is effectively stable,
	// but the cache is both size- and time-bounded: the LRU size
	// caps memory on a BFF instance fronting very many principals,
	// and the TTL bounds how long a stale mapping survives if an
	// admin changes a principal's email mid-process (after which the
	// entry is re-resolved with one cheap x:Account/get). Only
	// successful resolutions are stored, so a transient lookup
	// failure is retried rather than poisoned. The expirable LRU is
	// internally synchronised, so no extra mutex.
	emailCache *lru.LRU[string, string]
	// emailSF collapses concurrent lookups for the same account id
	// into a single in-flight x:Account/get call.
	emailSF singleflight.Group
}

const (
	// emailCacheMaxEntries caps the id->email cache as a memory guard
	// for a BFF instance fronting a very large number of distinct
	// principals. At ~64 B per entry, 50,000 entries stays well
	// under ~4 MiB.
	emailCacheMaxEntries = 50_000
	// emailCacheTTL bounds staleness after a principal email change
	// and mirrors the JMAP proxy's accountCache / shard resolver TTL
	// so all three identity caches age out consistently. The
	// id->email mapping rarely changes, so re-resolving every 5 min
	// is a negligible extra x:Account/get per active principal.
	emailCacheTTL = 5 * time.Minute
)

// DevAdminConfig builds the calendar-bridge Config shared by
// kmail-api and kmail-worker, layering in the dev/CI-only Stalwart
// superuser credentials when devEnv is true and
// KMAIL_STALWART_ADMIN_USER is set. In production (devEnv false) it
// returns a bare config: davAccount stays a no-op and the bridge
// authenticates to Stalwart via mTLS upstream rather than Basic.
// Centralising the gating here keeps the two entrypoints from
// drifting — if they wired the credentials differently the bridge
// would resolve CalDAV emails in one process but 404 in the other.
func DevAdminConfig(stalwartURL string, devEnv bool, logger *log.Logger) Config {
	cfg := Config{StalwartURL: stalwartURL}
	if devEnv {
		if adminUser := os.Getenv("KMAIL_STALWART_ADMIN_USER"); adminUser != "" {
			cfg.AdminUser = adminUser
			cfg.AdminPassword = os.Getenv("KMAIL_STALWART_ADMIN_PASS")
			cfg.Logger = logger
		}
	}
	return cfg
}

// NewService builds a Service from the provided Config.
func NewService(cfg Config) *Service {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Service{
		cfg:        cfg,
		emailCache: lru.NewLRU[string, string](emailCacheMaxEntries, nil, emailCacheTTL),
	}
}

// Calendar describes a calendar collection as surfaced to the BFF.
// Shapes mirror the draft JMAP calendars spec (see
// `docs/JMAP-CONTRACT.md` §2.1).
type Calendar struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	IsDefault   bool   `json:"isDefault"`
}

// Event describes a calendar event in the shape the BFF surfaces
// to the client. iCalendar `ICalData` carries the full VEVENT so
// CRUD round-trips are byte-exact.
type Event struct {
	UID      string `json:"uid"`
	Summary  string `json:"summary,omitempty"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
	ICalData string `json:"icalData"`
}

// TimeRange narrows an event query to events overlapping the
// provided window. Both bounds are optional; an open-ended range
// on either side translates to a CalDAV REPORT without the
// corresponding time-range filter.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// ParticipantResponse mirrors the RFC 5545 PARTSTAT values the
// client can send back when accepting / declining a meeting.
type ParticipantResponse string

const (
	ResponseAccepted    ParticipantResponse = "accepted"
	ResponseTentative   ParticipantResponse = "tentative"
	ResponseDeclined    ParticipantResponse = "declined"
	ResponseNeedsAction ParticipantResponse = "needs-action"
)

// ErrInvalidInput wraps caller-visible validation failures so the
// HTTP handlers can surface them as 400 Bad Request.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when Stalwart answers with a 404 for the
// target calendar or event.
var ErrNotFound = errors.New("not found")

// ListCalendars enumerates the authenticated principal's calendar
// home by issuing a CalDAV PROPFIND with Depth:1 at
// `/dav/cal/{account}/`.
func (s *Service) ListCalendars(ctx context.Context, accountID string) ([]Calendar, error) {
	if accountID == "" {
		return nil, fmt.Errorf("%w: accountID required", ErrInvalidInput)
	}
	home := s.calendarHome(ctx, accountID)
	body := strings.NewReader(calendarHomePropfindBody)
	resp, err := s.do(ctx, "PROPFIND", home, body, map[string]string{
		"Depth":        "1",
		"Content-Type": "application/xml; charset=utf-8",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caldav PROPFIND: HTTP %d", resp.StatusCode)
	}
	return parseCalendarsMultistatus(resp.Body, home)
}

// GetEvents runs a CalDAV `calendar-query` REPORT against the
// calendar collection and parses the returned iCalendar payloads.
func (s *Service) GetEvents(ctx context.Context, accountID, calendarID string, r TimeRange) ([]Event, error) {
	if accountID == "" || calendarID == "" {
		return nil, fmt.Errorf("%w: accountID and calendarID required", ErrInvalidInput)
	}
	path := s.calendarPath(ctx, accountID, calendarID)
	body := buildCalendarQueryReport(r)
	resp, err := s.do(ctx, "REPORT", path, strings.NewReader(body), map[string]string{
		"Depth":        "1",
		"Content-Type": "application/xml; charset=utf-8",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caldav REPORT: HTTP %d", resp.StatusCode)
	}
	return parseEventsMultistatus(resp.Body)
}

// CreateEvent PUTs a VEVENT payload under `{calendar}/{uid}.ics`.
// Returns the event UID the caller can use for subsequent
// GET / UPDATE / DELETE calls.
func (s *Service) CreateEvent(ctx context.Context, accountID, calendarID, icalData string) (string, error) {
	uid, err := extractICalUID(icalData)
	if err != nil {
		return "", err
	}
	path := s.eventPath(ctx, accountID, calendarID, uid)
	resp, err := s.do(ctx, http.MethodPut, path, strings.NewReader(icalData), map[string]string{
		"Content-Type":  "text/calendar; charset=utf-8",
		"If-None-Match": "*",
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("caldav PUT create: HTTP %d", resp.StatusCode)
	}
	return uid, nil
}

// UpdateEvent replaces the VEVENT on Stalwart. The UID is
// extracted from `icalData` and must match the on-disk event; a
// body with a different UID is treated as a different event and
// created alongside.
func (s *Service) UpdateEvent(ctx context.Context, accountID, calendarID, eventUID, icalData string) error {
	if eventUID == "" {
		var err error
		eventUID, err = extractICalUID(icalData)
		if err != nil {
			return err
		}
	}
	path := s.eventPath(ctx, accountID, calendarID, eventUID)
	resp, err := s.do(ctx, http.MethodPut, path, strings.NewReader(icalData), map[string]string{
		"Content-Type": "text/calendar; charset=utf-8",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("caldav PUT update: HTTP %d", resp.StatusCode)
	}
	return nil
}

// DeleteEvent removes the event by UID.
func (s *Service) DeleteEvent(ctx context.Context, accountID, calendarID, eventUID string) error {
	if eventUID == "" {
		return fmt.Errorf("%w: eventUID required", ErrInvalidInput)
	}
	path := s.eventPath(ctx, accountID, calendarID, eventUID)
	resp, err := s.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("caldav DELETE: HTTP %d", resp.StatusCode)
	}
	return nil
}

// RespondToEvent updates the event's PARTSTAT for the authenticated
// participant by rewriting the VEVENT body and PUTting it back.
// Stalwart treats the ATTENDEE's PARTSTAT as authoritative for the
// RSVP state, so a well-formed re-PUT is sufficient.
func (s *Service) RespondToEvent(ctx context.Context, accountID, calendarID, eventUID, participantEmail string, response ParticipantResponse) error {
	if eventUID == "" || participantEmail == "" {
		return fmt.Errorf("%w: eventUID and participantEmail required", ErrInvalidInput)
	}
	// Fetch the event, rewrite the ATTENDEE line's PARTSTAT,
	// then PUT it back.
	ics, err := s.fetchEvent(ctx, accountID, calendarID, eventUID)
	if err != nil {
		return err
	}
	updated, err := rewritePartstat(ics, participantEmail, response)
	if err != nil {
		return err
	}
	return s.UpdateEvent(ctx, accountID, calendarID, eventUID, updated)
}

func (s *Service) fetchEvent(ctx context.Context, accountID, calendarID, eventUID string) (string, error) {
	path := s.eventPath(ctx, accountID, calendarID, eventUID)
	resp, err := s.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("caldav GET: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (s *Service) do(ctx context.Context, method, urlStr string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if s.cfg.AdminUser != "" {
		req.SetBasicAuth(s.cfg.AdminUser, s.cfg.AdminPassword)
	}
	return s.cfg.HTTPClient.Do(req)
}

func (s *Service) calendarHome(ctx context.Context, accountID string) string {
	return strings.TrimRight(s.cfg.StalwartURL, "/") + "/dav/cal/" + url.PathEscape(s.davAccount(ctx, accountID)) + "/"
}

func (s *Service) calendarPath(ctx context.Context, accountID, calendarID string) string {
	return strings.TrimRight(s.cfg.StalwartURL, "/") + "/dav/cal/" + url.PathEscape(s.davAccount(ctx, accountID)) + "/" + url.PathEscape(calendarID) + "/"
}

func (s *Service) eventPath(ctx context.Context, accountID, calendarID, eventUID string) string {
	return s.calendarPath(ctx, accountID, calendarID) + url.PathEscape(eventUID) + ".ics"
}

// davAccount maps a JMAP account id to the account *email* Stalwart
// uses to key CalDAV collections (`/dav/cal/{email}/`). Stalwart
// resolves a DAV path's principal segment via the account's email
// address (crates/dav/src/common/uri.rs -> account_id_from_email),
// so a bare login name 404s; the BFF resolves callers to their
// JMAP account id, which is a third, distinct identifier. When the
// bridge holds admin credentials it asks Stalwart for the email via
// the `x:Account/get` management method and memoises the result;
// without credentials — or on any lookup failure — it returns the
// id unchanged (in production the id is already the email).
func (s *Service) davAccount(ctx context.Context, accountID string) string {
	if accountID == "" || s.cfg.AdminUser == "" {
		return accountID
	}
	if cached, ok := s.emailCache.Get(accountID); ok {
		return cached
	}
	// Dedupe concurrent lookups and only memoise a *resolved* email.
	// On any failure lookupAccountEmail returns ""; we then fall back
	// to the raw id for this call without caching it, so the next
	// request retries once Stalwart recovers. This also removes the
	// race where a failing goroutine could overwrite another's
	// successful result, since failures never touch the cache. A
	// hard lookup error (transport / non-2xx / malformed response,
	// as opposed to a clean not-found) is logged once per attempt
	// inside the deduped closure so a persistent misconfiguration is
	// observable rather than silently 404-ing every CalDAV call.
	v, _, _ := s.emailSF.Do(accountID, func() (interface{}, error) {
		email, lookupErr := s.lookupAccountEmail(ctx, accountID)
		if email != "" {
			s.emailCache.Add(accountID, email)
		} else if lookupErr != nil && s.cfg.Logger != nil {
			s.cfg.Logger.Printf("calendarbridge: resolve CalDAV email for account %q failed (falling back to id, CalDAV calls will 404): %v", accountID, lookupErr)
		}
		return email, nil
	})
	if email, _ := v.(string); email != "" {
		return email
	}
	return accountID
}

// lookupAccountEmail issues a single `x:Account/get` JMAP call as the
// admin principal and returns the resolved account email address —
// the identifier Stalwart keys DAV collections by. It returns a
// non-nil error for *hard* failures (transport, non-2xx, or a
// malformed response) so the caller can surface a misconfiguration;
// a successful call that simply doesn't contain the requested id
// (e.g. an unprovisioned principal) returns ("", nil) since that is
// an expected, non-alarming outcome. Stalwart ignores the
// `accountId` envelope field for this management method, so an
// empty value is fine.
func (s *Service) lookupAccountEmail(ctx context.Context, accountID string) (string, error) {
	ids, err := json.Marshal([]string{accountID})
	if err != nil {
		return "", err
	}
	body := `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["x:Account/get",{"accountId":"","ids":` + string(ids) + `},"0"]]}`
	endpoint := strings.TrimRight(s.cfg.StalwartURL, "/") + "/jmap"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.cfg.AdminUser, s.cfg.AdminPassword)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("x:Account/get: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var envelope struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	if len(envelope.MethodResponses) == 0 || len(envelope.MethodResponses[0]) < 2 {
		return "", fmt.Errorf("x:Account/get: malformed response envelope")
	}
	// A JMAP method-level error arrives as an HTTP 200 whose first
	// tuple element is the literal "error" (e.g.
	// `["error",{"type":"unknownMethod"},"0"]`) rather than the
	// echoed method name. Without this check the error object would
	// unmarshal cleanly into result with an empty List and look like
	// a benign not-found, hiding a real misconfiguration (wrong
	// method capability, server bug) from the caller's logging.
	var methodName string
	if err := json.Unmarshal(envelope.MethodResponses[0][0], &methodName); err != nil {
		return "", fmt.Errorf("x:Account/get: malformed method name: %w", err)
	}
	if methodName == "error" {
		var jmapErr struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(envelope.MethodResponses[0][1], &jmapErr)
		return "", fmt.Errorf("x:Account/get: JMAP error %q", jmapErr.Type)
	}
	var result struct {
		List []struct {
			ID    string `json:"id"`
			Email string `json:"emailAddress"`
		} `json:"list"`
	}
	if err := json.Unmarshal(envelope.MethodResponses[0][1], &result); err != nil {
		return "", err
	}
	// Match strictly on the requested id. The request passes
	// `ids:[accountID]`, so Stalwart returns at most that one
	// account; we never fall back to an arbitrary list entry,
	// which could otherwise cache a *different* principal's email
	// and route every subsequent CalDAV call for this account to
	// the wrong calendar home.
	for _, a := range result.List {
		if a.ID == accountID && a.Email != "" {
			return a.Email, nil
		}
	}
	return "", nil
}

// ------------------------------------------------------------------
// CalDAV XML body builders + parsers
// ------------------------------------------------------------------

const calendarHomePropfindBody = `<?xml version="1.0" encoding="UTF-8"?>
<propfind xmlns="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/">
  <prop>
    <displayname/>
    <resourcetype/>
    <c:calendar-description/>
    <cs:getctag/>
    <c:supported-calendar-component-set/>
  </prop>
</propfind>`

// multistatusResponse is the subset of DAV:response KMail reads.
type multistatusResponse struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []struct {
		Href     string `xml:"DAV: href"`
		Propstat []struct {
			Prop struct {
				DisplayName  string `xml:"DAV: displayname"`
				ResourceType struct {
					Calendar *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
				} `xml:"DAV: resourcetype"`
				CalendarDescription string `xml:"urn:ietf:params:xml:ns:caldav calendar-description"`
				CalendarData        string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
			} `xml:"DAV: prop"`
			Status string `xml:"DAV: status"`
		} `xml:"DAV: propstat"`
	} `xml:"DAV: response"`
}

func parseCalendarsMultistatus(r io.Reader, homeHref string) ([]Calendar, error) {
	var ms multistatusResponse
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, fmt.Errorf("parse multistatus: %w", err)
	}
	var out []Calendar
	// Normalise homeHref: compare on path only so
	// host-relative and absolute href forms match.
	for _, resp := range ms.Responses {
		isCalendar := false
		var disp, desc string
		for _, ps := range resp.Propstat {
			if ps.Prop.ResourceType.Calendar != nil {
				isCalendar = true
			}
			if ps.Prop.DisplayName != "" {
				disp = ps.Prop.DisplayName
			}
			if ps.Prop.CalendarDescription != "" {
				desc = ps.Prop.CalendarDescription
			}
		}
		if !isCalendar {
			continue
		}
		id := calendarIDFromHref(resp.Href)
		if id == "" {
			continue
		}
		out = append(out, Calendar{
			ID:          id,
			Name:        disp,
			Description: desc,
		})
	}
	return out, nil
}

func calendarIDFromHref(href string) string {
	// href looks like /dav/cal/<account>/<id>/ — pluck the
	// trailing collection segment.
	trimmed := strings.TrimRight(href, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return ""
	}
	return trimmed[idx+1:]
}

func buildCalendarQueryReport(r TimeRange) string {
	var timeRange string
	if !r.Start.IsZero() || !r.End.IsZero() {
		var attrs []string
		if !r.Start.IsZero() {
			attrs = append(attrs, `start="`+r.Start.UTC().Format("20060102T150405Z")+`"`)
		}
		if !r.End.IsZero() {
			attrs = append(attrs, `end="`+r.End.UTC().Format("20060102T150405Z")+`"`)
		}
		timeRange = "<c:time-range " + strings.Join(attrs, " ") + "/>"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:prop>
    <d:getetag/>
    <c:calendar-data/>
  </d:prop>
  <c:filter>
    <c:comp-filter name="VCALENDAR">
      <c:comp-filter name="VEVENT">` + timeRange + `
      </c:comp-filter>
    </c:comp-filter>
  </c:filter>
</c:calendar-query>`
}

func parseEventsMultistatus(r io.Reader) ([]Event, error) {
	var ms multistatusResponse
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, fmt.Errorf("parse multistatus: %w", err)
	}
	var out []Event
	for _, resp := range ms.Responses {
		for _, ps := range resp.Propstat {
			data := strings.TrimSpace(ps.Prop.CalendarData)
			if data == "" {
				continue
			}
			uid, _ := extractICalUID(data)
			out = append(out, Event{
				UID:      uid,
				Summary:  extractICalField(data, "SUMMARY"),
				Start:    extractICalField(data, "DTSTART"),
				End:      extractICalField(data, "DTEND"),
				ICalData: data,
			})
		}
	}
	return out, nil
}

// ------------------------------------------------------------------
// iCalendar helpers
// ------------------------------------------------------------------

// extractICalUID returns the UID property value of the first VEVENT
// in the provided iCalendar payload.
func extractICalUID(ical string) (string, error) {
	uid := extractICalField(ical, "UID")
	if uid == "" {
		return "", fmt.Errorf("%w: iCalendar missing UID", ErrInvalidInput)
	}
	return uid, nil
}

// extractICalField returns the first value of the named property
// ignoring any parameter components. iCalendar property lines use
// `NAME[;PARAM=VAL]:VALUE` — the regex-free parser scans line by
// line with CRLF + LF support.
func extractICalField(ical, name string) string {
	for _, line := range splitICalLines(ical) {
		// Parameter split: NAME or NAME;PARAM=... — everything
		// before the first `;` or `:` is the property name.
		split := strings.IndexAny(line, ";:")
		if split < 0 {
			continue
		}
		if !strings.EqualFold(line[:split], name) {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		return strings.TrimSpace(line[colon+1:])
	}
	return ""
}

// splitICalLines folds continuation lines (RFC 5545 §3.1) and
// splits on CRLF or LF.
func splitICalLines(ical string) []string {
	norm := strings.ReplaceAll(ical, "\r\n", "\n")
	raw := strings.Split(norm, "\n")
	var out []string
	for _, line := range raw {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(out) > 0 {
			// Continuation: append to previous line with leading
			// whitespace stripped per spec.
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

// rewritePartstat rewrites the ATTENDEE line matching
// `participantEmail` to carry the provided PARTSTAT. Returns the
// rewritten iCalendar payload.
func rewritePartstat(ical, participantEmail string, response ParticipantResponse) (string, error) {
	partstat := strings.ToUpper(string(response))
	switch ParticipantResponse(strings.ToLower(string(response))) {
	case ResponseAccepted:
		partstat = "ACCEPTED"
	case ResponseTentative:
		partstat = "TENTATIVE"
	case ResponseDeclined:
		partstat = "DECLINED"
	case ResponseNeedsAction:
		partstat = "NEEDS-ACTION"
	default:
		return "", fmt.Errorf("%w: unknown participant response %q", ErrInvalidInput, response)
	}

	lines := splitICalLines(ical)
	var out bytes.Buffer
	found := false
	mailto := "mailto:" + strings.ToLower(participantEmail)
	for _, line := range lines {
		if strings.HasPrefix(strings.ToUpper(line), "ATTENDEE") && strings.Contains(strings.ToLower(line), mailto) {
			line = rewriteAttendeeLine(line, partstat)
			found = true
		}
		out.WriteString(line)
		out.WriteString("\r\n")
	}
	if !found {
		return "", fmt.Errorf("%w: no ATTENDEE matching %s", ErrInvalidInput, participantEmail)
	}
	return out.String(), nil
}

func rewriteAttendeeLine(line, partstat string) string {
	// Replace existing PARTSTAT= value; inject PARTSTAT= before
	// the `:` if none is present.
	lower := strings.ToLower(line)
	if idx := strings.Index(lower, "partstat="); idx >= 0 {
		// Find the next `;` or `:` after the `=`.
		end := strings.IndexAny(line[idx:], ";:")
		if end < 0 {
			end = len(line) - idx
		}
		return line[:idx] + "PARTSTAT=" + partstat + line[idx+end:]
	}
	// No existing PARTSTAT — inject before the `:`.
	colon := strings.Index(line, ":")
	if colon < 0 {
		return line
	}
	return line[:colon] + ";PARTSTAT=" + partstat + line[colon:]
}
