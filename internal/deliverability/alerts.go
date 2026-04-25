package deliverability

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Canonical alert metric names — kept in sync with the evaluator
// switch below and the default threshold seeding.
const (
	MetricBounceRate       = "bounce_rate"
	MetricComplaintRate    = "complaint_rate"
	MetricReputationDrop   = "reputation_drop"
	MetricDailyVolumeSpike = "daily_volume_spike"
)

// Deliverability alert severities. "info" is reserved for
// notifications that did not trip a threshold but still warrant a
// timeline entry; the evaluator only writes "warning" / "critical".
const (
	AlertSeverityInfo     = "info"
	AlertSeverityWarning  = "warning"
	AlertSeverityCritical = "critical"
)

// DeliverabilityAlert is the API + persisted shape of one alert.
type DeliverabilityAlert struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	AlertType      string    `json:"alert_type"`
	Severity       string    `json:"severity"`
	MetricName     string    `json:"metric_name"`
	MetricValue    float64   `json:"metric_value"`
	ThresholdValue float64   `json:"threshold_value"`
	Message        string    `json:"message"`
	Acknowledged   bool      `json:"acknowledged"`
	CreatedAt      time.Time `json:"created_at"`
}

// AlertThreshold overrides the plan-default warning / critical cut
// points for one metric.
type AlertThreshold struct {
	TenantID          string    `json:"tenant_id"`
	MetricName        string    `json:"metric_name"`
	WarningThreshold  float64   `json:"warning_threshold"`
	CriticalThreshold float64   `json:"critical_threshold"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ListDeliverabilityAlertsOptions filters the listing.
type ListDeliverabilityAlertsOptions struct {
	Severity     string
	Acknowledged *bool
	Limit        int
	Offset       int
}

// AlertService owns `deliverability_alerts` + `alert_thresholds`.
// It is read from by the admin UI and by the background evaluator
// goroutine wired up in `cmd/kmail-api/main.go`.
type AlertService struct {
	pool      *pgxpool.Pool
	bounce    *BounceProcessor
	sendLimit *SendLimitService
	logger    *log.Logger
}

// defaultThresholds matches the numbers called out in
// docs/PROPOSAL.md §9.4 and the task spec.
var defaultThresholds = map[string]AlertThreshold{
	MetricBounceRate:       {MetricName: MetricBounceRate, WarningThreshold: 0.05, CriticalThreshold: 0.10},
	MetricComplaintRate:    {MetricName: MetricComplaintRate, WarningThreshold: 0.001, CriticalThreshold: 0.003},
	MetricReputationDrop:   {MetricName: MetricReputationDrop, WarningThreshold: 20, CriticalThreshold: 40},
	MetricDailyVolumeSpike: {MetricName: MetricDailyVolumeSpike, WarningThreshold: 5.0, CriticalThreshold: 10.0},
}

// EvaluateThresholds recomputes every deliverability metric for
// the tenant and persists an alert for each threshold breach.
// Returns the new alerts raised on this pass.
func (a *AlertService) EvaluateThresholds(ctx context.Context, tenantID string) ([]DeliverabilityAlert, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	thresholds, err := a.getThresholds(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	metrics, err := a.sampleMetrics(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var raised []DeliverabilityAlert
	for name, value := range metrics {
		th, ok := thresholds[name]
		if !ok {
			th = defaultThresholds[name]
		}
		sev, crossed := evaluateSeverity(value, th)
		if !crossed {
			continue
		}
		alert := DeliverabilityAlert{
			TenantID:       tenantID,
			AlertType:      name,
			Severity:       sev,
			MetricName:     name,
			MetricValue:    value,
			ThresholdValue: chooseThreshold(sev, th),
			Message: fmt.Sprintf("%s at %.4f (threshold %.4f)",
				name, value, chooseThreshold(sev, th)),
		}
		if err := a.insertAlert(ctx, &alert); err != nil {
			return nil, err
		}
		raised = append(raised, alert)
	}
	return raised, nil
}

// ListAlerts paginates the alerts ordered by `created_at DESC`.
func (a *AlertService) ListAlerts(ctx context.Context, tenantID string, opts ListDeliverabilityAlertsOptions) ([]DeliverabilityAlert, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Limit > 500 {
		opts.Limit = 500
	}
	if a.pool == nil {
		return nil, nil
	}
	var out []DeliverabilityAlert
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		args := []any{tenantID}
		where := `tenant_id = $1::uuid`
		if opts.Severity != "" {
			args = append(args, opts.Severity)
			where += fmt.Sprintf(` AND severity = $%d`, len(args))
		}
		if opts.Acknowledged != nil {
			args = append(args, *opts.Acknowledged)
			where += fmt.Sprintf(` AND acknowledged = $%d`, len(args))
		}
		args = append(args, opts.Limit, opts.Offset)
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, alert_type, severity,
			       metric_name, metric_value, threshold_value,
			       message, acknowledged, created_at
			FROM deliverability_alerts
			WHERE `+where+`
			ORDER BY created_at DESC
			LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var al DeliverabilityAlert
			if err := rows.Scan(&al.ID, &al.TenantID, &al.AlertType, &al.Severity, &al.MetricName, &al.MetricValue, &al.ThresholdValue, &al.Message, &al.Acknowledged, &al.CreatedAt); err != nil {
				return err
			}
			out = append(out, al)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list deliverability alerts: %w", err)
	}
	return out, nil
}

// AcknowledgeAlert flips the `acknowledged` flag on a single alert.
func (a *AlertService) AcknowledgeAlert(ctx context.Context, tenantID, alertID string) error {
	if tenantID == "" || alertID == "" {
		return fmt.Errorf("%w: tenantID and alertID required", ErrInvalidInput)
	}
	if a.pool == nil {
		return nil
	}
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		cmd, err := tx.Exec(ctx, `
			UPDATE deliverability_alerts SET acknowledged = true
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, alertID)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ack deliverability alert: %w", err)
	}
	return nil
}

// ConfigureThresholds upserts the per-tenant threshold table with
// the provided rows. Rows with empty `metric_name` are skipped.
func (a *AlertService) ConfigureThresholds(ctx context.Context, tenantID string, thresholds []AlertThreshold) ([]AlertThreshold, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if a.pool == nil {
		return thresholds, nil
	}
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		for _, th := range thresholds {
			if th.MetricName == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO alert_thresholds (tenant_id, metric_name, warning_threshold, critical_threshold, updated_at)
				VALUES ($1::uuid, $2, $3, $4, now())
				ON CONFLICT (tenant_id, metric_name)
				DO UPDATE SET warning_threshold = EXCLUDED.warning_threshold,
				              critical_threshold = EXCLUDED.critical_threshold,
				              updated_at = now()
			`, tenantID, th.MetricName, th.WarningThreshold, th.CriticalThreshold); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("configure thresholds: %w", err)
	}
	return a.ListThresholds(ctx, tenantID)
}

// ListThresholds returns the merged plan defaults + per-tenant
// overrides.
func (a *AlertService) ListThresholds(ctx context.Context, tenantID string) ([]AlertThreshold, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	out := make(map[string]AlertThreshold, len(defaultThresholds))
	for k, v := range defaultThresholds {
		v.TenantID = tenantID
		out[k] = v
	}
	if a.pool == nil {
		return thresholdsToSlice(out), nil
	}
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT tenant_id::text, metric_name, warning_threshold,
			       critical_threshold, updated_at
			FROM alert_thresholds
			WHERE tenant_id = $1::uuid
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var th AlertThreshold
			if err := rows.Scan(&th.TenantID, &th.MetricName, &th.WarningThreshold, &th.CriticalThreshold, &th.UpdatedAt); err != nil {
				return err
			}
			out[th.MetricName] = th
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list thresholds: %w", err)
	}
	return thresholdsToSlice(out), nil
}

func thresholdsToSlice(m map[string]AlertThreshold) []AlertThreshold {
	keys := []string{MetricBounceRate, MetricComplaintRate, MetricReputationDrop, MetricDailyVolumeSpike}
	out := make([]AlertThreshold, 0, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out = append(out, v)
		}
	}
	return out
}

func (a *AlertService) getThresholds(ctx context.Context, tenantID string) (map[string]AlertThreshold, error) {
	list, err := a.ListThresholds(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]AlertThreshold, len(list))
	for _, th := range list {
		out[th.MetricName] = th
	}
	return out, nil
}

// sampleMetrics computes the current values of each tracked
// metric. Returns a map keyed by `metric_name`.
func (a *AlertService) sampleMetrics(ctx context.Context, tenantID string) (map[string]float64, error) {
	out := map[string]float64{
		MetricBounceRate:       0,
		MetricComplaintRate:    0,
		MetricReputationDrop:   0,
		MetricDailyVolumeSpike: 0,
	}
	if a.pool == nil {
		return out, nil
	}
	// Pull the 24h send count + 7-day average from the
	// SendLimitService Valkey counters so bounce / complaint
	// rates use the actual send total as the denominator (the
	// thresholds in docs/PROPOSAL.md §9.4 are calibrated for
	// `bounces / sent`, not `bounces / bounce_events`).
	sends24h, err := a.sendLimit.GetDailyVolume(ctx, tenantID, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("sample metrics: %w", err)
	}
	avgSends, err := a.sendLimit.AverageDailyVolume(ctx, tenantID, 7)
	if err != nil {
		return nil, fmt.Errorf("sample metrics: %w", err)
	}
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var hard, complaint int
		if err := tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE bounce_type = 'hard'),
				COUNT(*) FILTER (WHERE bounce_type = 'complaint')
			FROM bounce_events
			WHERE tenant_id = $1::uuid AND created_at >= now() - interval '24 hours'
		`, tenantID).Scan(&hard, &complaint); err != nil {
			return err
		}
		if sends24h > 0 {
			out[MetricBounceRate] = float64(hard) / float64(sends24h)
			out[MetricComplaintRate] = float64(complaint) / float64(sends24h)
		}
		// daily_volume_spike — today's send count vs. the 7-day
		// average from the SendLimitService history.
		if avgSends > 0 {
			out[MetricDailyVolumeSpike] = float64(sends24h) / avgSends
		}
		// reputation_drop — biggest drop across the tenant's IP
		// pool in the last 24 h. Today we derive it from the live
		// `ip_addresses.reputation_score` column; as the trend
		// store matures this switches to a proper time series.
		var drop float64
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(100 - reputation_score), 0)
			FROM ip_addresses ia
			JOIN tenant_pool_assignments tpa ON tpa.pool_id = ia.pool_id
			WHERE tpa.tenant_id = $1::uuid
		`, tenantID).Scan(&drop)
		out[MetricReputationDrop] = drop
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sample metrics: %w", err)
	}
	return out, nil
}

func (a *AlertService) insertAlert(ctx context.Context, alert *DeliverabilityAlert) error {
	if a.pool == nil {
		alert.ID = "stub"
		alert.CreatedAt = time.Now()
		return nil
	}
	return pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, alert.TenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO deliverability_alerts (
				tenant_id, alert_type, severity, metric_name,
				metric_value, threshold_value, message
			)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
			RETURNING id::text, created_at
		`, alert.TenantID, alert.AlertType, alert.Severity,
			alert.MetricName, alert.MetricValue, alert.ThresholdValue,
			alert.Message,
		).Scan(&alert.ID, &alert.CreatedAt)
	})
}

// evaluateSeverity returns ("critical", true) when the metric
// exceeds the critical threshold, ("warning", true) when only the
// warning threshold is exceeded, and ("", false) otherwise.
func evaluateSeverity(value float64, th AlertThreshold) (string, bool) {
	if th.CriticalThreshold > 0 && value >= th.CriticalThreshold {
		return AlertSeverityCritical, true
	}
	if th.WarningThreshold > 0 && value >= th.WarningThreshold {
		return AlertSeverityWarning, true
	}
	return "", false
}

func chooseThreshold(severity string, th AlertThreshold) float64 {
	if severity == AlertSeverityCritical {
		return th.CriticalThreshold
	}
	return th.WarningThreshold
}

// AlertEvaluator is the background loop that periodically runs
// EvaluateThresholds across every active tenant. It mirrors the
// QuotaWorker pattern in `internal/billing/quota_worker.go`.
type AlertEvaluator struct {
	Service  *AlertService
	Pool     *pgxpool.Pool
	Interval time.Duration
	Logger   *log.Logger
}

// Run loops until ctx is cancelled.
func (e *AlertEvaluator) Run(ctx context.Context) {
	if e.Service == nil || e.Pool == nil {
		return
	}
	if e.Interval <= 0 {
		e.Interval = 15 * time.Minute
	}
	logger := e.Logger
	if logger == nil {
		logger = log.Default()
	}
	ticker := time.NewTicker(e.Interval)
	defer ticker.Stop()
	if err := e.tick(ctx); err != nil {
		logger.Printf("alert evaluator first tick: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.tick(ctx); err != nil {
				logger.Printf("alert evaluator tick: %v", err)
			}
		}
	}
}

func (e *AlertEvaluator) tick(ctx context.Context) error {
	rows, err := e.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE status <> 'deleted'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := e.Service.EvaluateThresholds(ctx, id); err != nil {
			if e.Logger != nil {
				e.Logger.Printf("evaluate tenant %s: %v", id, err)
			}
		}
	}
	return nil
}
