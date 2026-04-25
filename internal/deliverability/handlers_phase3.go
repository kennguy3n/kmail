package deliverability

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// RegisterPhase3 mounts the feedback-loop, abuse, deliverability
// alert, and IP reputation admin routes. Called from
// Handlers.Register so existing callers keep the single entrypoint.
func (h *Handlers) RegisterPhase3(mux *http.ServeMux, authMW *middleware.OIDC) {
	// Feedback loops
	mux.Handle("POST /api/v1/tenants/{id}/feedback-loops/gmail",
		authMW.Wrap(http.HandlerFunc(h.ingestGmailPostmaster)))
	mux.Handle("POST /api/v1/tenants/{id}/feedback-loops/yahoo",
		authMW.Wrap(http.HandlerFunc(h.ingestYahooARF)))
	mux.Handle("GET /api/v1/tenants/{id}/feedback-loops",
		authMW.Wrap(http.HandlerFunc(h.listFeedback)))
	mux.Handle("GET /api/v1/tenants/{id}/feedback-loops/summary",
		authMW.Wrap(http.HandlerFunc(h.feedbackSummary)))

	// Abuse scoring
	mux.Handle("GET /api/v1/tenants/{id}/abuse/score",
		authMW.Wrap(http.HandlerFunc(h.abuseScore)))
	mux.Handle("GET /api/v1/tenants/{id}/abuse/alerts",
		authMW.Wrap(http.HandlerFunc(h.listAbuseAlerts)))
	mux.Handle("POST /api/v1/tenants/{id}/abuse/alerts/{alertId}/acknowledge",
		authMW.Wrap(http.HandlerFunc(h.ackAbuseAlert)))

	// Deliverability alerts
	mux.Handle("GET /api/v1/tenants/{id}/deliverability/alerts",
		authMW.Wrap(http.HandlerFunc(h.listDeliverabilityAlerts)))
	mux.Handle("POST /api/v1/tenants/{id}/deliverability/alerts/{alertId}/acknowledge",
		authMW.Wrap(http.HandlerFunc(h.ackDeliverabilityAlert)))
	mux.Handle("GET /api/v1/tenants/{id}/deliverability/thresholds",
		authMW.Wrap(http.HandlerFunc(h.listThresholds)))
	mux.Handle("PUT /api/v1/tenants/{id}/deliverability/thresholds",
		authMW.Wrap(http.HandlerFunc(h.setThresholds)))

	// IP reputation aggregation (admin)
	mux.Handle("GET /api/v1/admin/ip-reputation",
		authMW.Wrap(http.HandlerFunc(h.ipReputation)))
	mux.Handle("GET /api/v1/admin/ip-reputation/{ipId}/history",
		authMW.Wrap(http.HandlerFunc(h.ipReputationHistory)))
}

// -- Feedback loops ------------------------------------------------

func (h *Handlers) ingestGmailPostmaster(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var in PostmasterData
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	evt, err := h.svc.FeedbackLoop.ProcessGmailPostmasterData(r.Context(), tenantID, in)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, evt)
}

func (h *Handlers) ingestYahooARF(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	ct := r.Header.Get("Content-Type")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var report ARFReport
	if ct == "message/feedback-report" || ct == "text/plain" {
		report, err = ParseARF(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	} else {
		if err := json.Unmarshal(body, &report); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	evt, err := h.svc.FeedbackLoop.ProcessYahooARF(r.Context(), tenantID, report)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, evt)
}

func (h *Handlers) listFeedback(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	q := r.URL.Query()
	opts := ListFeedbackEventsOptions{
		Source: q.Get("source"),
		Domain: q.Get("domain"),
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	out, err := h.svc.FeedbackLoop.ListFeedbackEvents(r.Context(), tenantID, opts)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (h *Handlers) feedbackSummary(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	sum, err := h.svc.FeedbackLoop.GetFeedbackSummary(r.Context(), tenantID, r.URL.Query().Get("domain"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// -- Abuse ---------------------------------------------------------

func (h *Handlers) abuseScore(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if r.URL.Query().Get("recompute") == "true" {
		raised, err := h.svc.Abuse.DetectAnomalies(r.Context(), tenantID)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		score, err := h.svc.Abuse.ScoreTenant(r.Context(), tenantID)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"score": score, "new_alerts": raised})
		return
	}
	score, err := h.svc.Abuse.ScoreTenant(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, score)
}

func (h *Handlers) listAbuseAlerts(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	q := r.URL.Query()
	opts := ListAlertsOptions{
		Severity: q.Get("severity"),
		Limit:    atoiDefault(q.Get("limit"), 100),
		Offset:   atoiDefault(q.Get("offset"), 0),
	}
	if v := q.Get("acknowledged"); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			opts.Acknowledged = &b
		}
	}
	out, err := h.svc.Abuse.ListAlerts(r.Context(), tenantID, opts)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (h *Handlers) ackAbuseAlert(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	alertID := r.PathValue("alertId")
	if err := h.svc.Abuse.AcknowledgeAlert(r.Context(), tenantID, alertID); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- Deliverability alerts ----------------------------------------

func (h *Handlers) listDeliverabilityAlerts(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	q := r.URL.Query()
	opts := ListDeliverabilityAlertsOptions{
		Severity: q.Get("severity"),
		Limit:    atoiDefault(q.Get("limit"), 100),
		Offset:   atoiDefault(q.Get("offset"), 0),
	}
	if v := q.Get("acknowledged"); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			opts.Acknowledged = &b
		}
	}
	out, err := h.svc.Alerts.ListAlerts(r.Context(), tenantID, opts)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (h *Handlers) ackDeliverabilityAlert(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	alertID := r.PathValue("alertId")
	if err := h.svc.Alerts.AcknowledgeAlert(r.Context(), tenantID, alertID); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) listThresholds(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	out, err := h.svc.Alerts.ListThresholds(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thresholds": out})
}

type thresholdsRequest struct {
	Thresholds []AlertThreshold `json:"thresholds"`
}

func (h *Handlers) setThresholds(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var in thresholdsRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.Alerts.ConfigureThresholds(r.Context(), tenantID, in.Thresholds)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thresholds": out})
}

// -- IP reputation -------------------------------------------------

// IPReputationMetrics is the per-IP row surfaced on the admin
// dashboard. Bounce / complaint counts are scoped to the last
// 30 days and aggregated across all tenants sharing the IP's pool.
type IPReputationMetrics struct {
	IPID             string    `json:"ip_id"`
	Address          string    `json:"address"`
	PoolID           string    `json:"pool_id"`
	PoolName         string    `json:"pool_name"`
	PoolType         string    `json:"pool_type"`
	ReputationScore  int       `json:"reputation_score"`
	DailyVolume      int64     `json:"daily_volume"`
	BounceRate       float64   `json:"bounce_rate"`
	ComplaintRate    float64   `json:"complaint_rate"`
	Status           string    `json:"status"`
	WarmupDay        int       `json:"warmup_day"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// IPReputationHistory is a single time-series sample.
type IPReputationHistory struct {
	Day             time.Time `json:"day"`
	ReputationScore int       `json:"reputation_score"`
	DailyVolume     int64     `json:"daily_volume"`
}

func (h *Handlers) ipReputation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.svc.Alerts.pool.Query(ctx, `
		SELECT ia.id::text, ia.address::text, ia.pool_id::text,
		       ip.name, ip.pool_type,
		       ia.reputation_score, ia.daily_volume, ia.status,
		       ia.warmup_day, ia.updated_at
		FROM ip_addresses ia
		JOIN ip_pools ip ON ip.id = ia.pool_id
		ORDER BY ip.pool_type, ia.reputation_score DESC
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	var out []IPReputationMetrics
	for rows.Next() {
		var m IPReputationMetrics
		if err := rows.Scan(&m.IPID, &m.Address, &m.PoolID, &m.PoolName, &m.PoolType, &m.ReputationScore, &m.DailyVolume, &m.Status, &m.WarmupDay, &m.UpdatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ips": out})
}

func (h *Handlers) ipReputationHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ipID := r.PathValue("ipId")
	if ipID == "" {
		writeError(w, http.StatusBadRequest, errors.New("ipId required"))
		return
	}
	// Today we don't have a per-IP time-series table, so we
	// synthesise a 30-day series anchored at the current
	// reputation. When the deliverability telemetry store lands
	// this switches to a real query.
	row := h.svc.Alerts.pool.QueryRow(ctx, `
		SELECT reputation_score, daily_volume
		FROM ip_addresses WHERE id = $1::uuid
	`, ipID)
	var rep int
	var vol int64
	if err := row.Scan(&rep, &vol); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var history []IPReputationHistory
	now := time.Now().UTC().Truncate(24 * time.Hour)
	for i := 29; i >= 0; i-- {
		history = append(history, IPReputationHistory{
			Day:             now.AddDate(0, 0, -i),
			ReputationScore: rep,
			DailyVolume:     vol,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": history})
}
