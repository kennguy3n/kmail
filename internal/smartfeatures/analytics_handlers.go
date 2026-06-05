package smartfeatures

import (
	"log"
	"net/http"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// AnalyticsHandlers serves the Email Analytics admin dashboard.
//
// Route (behind the OIDC middleware):
//
//	GET /api/v1/email-analytics?days=30&tz=America/New_York
//
// The report is computed from the acting user's Sent + Inbox
// windows over the requested number of days. (Tenant-wide
// aggregation across every mailbox, plus audit-log / deliverability
// joins, is a documented follow-up — those data sources are owned
// by other workstreams.)
type AnalyticsHandlers struct {
	source AnalyticsSource
	logger *log.Logger
	now    func() time.Time
}

// NewAnalyticsHandlers constructs the analytics surface. A nil
// source is a wiring bug and panics, matching NewHandlers.
func NewAnalyticsHandlers(source AnalyticsSource, logger *log.Logger) *AnalyticsHandlers {
	if source == nil {
		panic("smartfeatures.NewAnalyticsHandlers: source is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &AnalyticsHandlers{source: source, logger: logger, now: time.Now}
}

// Register wires the analytics route behind the OIDC middleware.
func (h *AnalyticsHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/email-analytics", authMW.Wrap(http.HandlerFunc(h.report)))
}

const (
	defaultAnalyticsDays = 30
	maxAnalyticsDays     = 365
	maxAnalyticsWindow   = 1000
)

func (h *AnalyticsHandlers) report(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	userID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || userID == "" {
		writeJSON(w, http.StatusInternalServerError, errBody("missing auth context"))
		return
	}

	days := clampLimit(r.URL.Query().Get("days"), defaultAnalyticsDays, maxAnalyticsDays)
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}

	now := h.now()
	since := now.AddDate(0, 0, -days)
	sent, received, err := h.source.Window(r.Context(), tenantID, userID, since, maxAnalyticsWindow)
	if err != nil {
		h.logger.Printf("smartfeatures: analytics window: %v", err)
		writeJSON(w, http.StatusBadGateway, errBody("could not load mailbox analytics"))
		return
	}

	report := Aggregate(sent, received, loc, now)
	report.RangeStart = since.In(loc).Format("2006-01-02")
	writeJSON(w, http.StatusOK, report)
}
