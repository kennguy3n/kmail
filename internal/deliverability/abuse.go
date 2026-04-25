package deliverability

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Abuse alert severity values.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// Abuse alert type identifiers.
const (
	AlertTypeVolumeSpike       = "volume_spike"
	AlertTypeRecipientAnomaly  = "recipient_anomaly"
	AlertTypeAuthFailureStorm  = "auth_failure_storm"
	AlertTypeHighBounceRate    = "high_bounce_rate"
	AlertTypeHighComplaintRate = "high_complaint_rate"
)

// AbuseAlert is the API + persisted shape of a single anomaly
// detection.
type AbuseAlert struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	UserID       string          `json:"user_id,omitempty"`
	AlertType    string          `json:"alert_type"`
	Severity     string          `json:"severity"`
	Score        int             `json:"score"`
	Details      json.RawMessage `json:"details"`
	Acknowledged bool            `json:"acknowledged"`
	CreatedAt    time.Time       `json:"created_at"`
}

// AbuseScore is the cached per-tenant (user_id empty) or per-user
// composite score.
type AbuseScore struct {
	TenantID  string          `json:"tenant_id"`
	UserID    string          `json:"user_id,omitempty"`
	Score     int             `json:"score"`
	Signals   json.RawMessage `json:"signals"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ListAlertsOptions filters the alerts listing.
type ListAlertsOptions struct {
	Severity     string
	Acknowledged *bool
	Limit        int
	Offset       int
}

// AbuseScorer computes per-tenant / per-user abuse scores and
// persists anomalies to `abuse_alerts`.
type AbuseScorer struct {
	pool      *pgxpool.Pool
	sendLimit *SendLimitService
}

// ScoreTenant recomputes the tenant-level score from the
// configured signals and writes the result into `abuse_scores`.
// Returns the refreshed score row.
func (a *AbuseScorer) ScoreTenant(ctx context.Context, tenantID string) (*AbuseScore, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	score := &AbuseScore{TenantID: tenantID}
	if a.pool == nil {
		score.Signals = json.RawMessage(`{}`)
		score.UpdatedAt = time.Now()
		return score, nil
	}
	signals, total, err := a.computeTenantSignals(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(signals)
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO abuse_scores (tenant_id, user_id, score, signals, updated_at)
			VALUES ($1::uuid, '00000000-0000-0000-0000-000000000000'::uuid, $2, $3::jsonb, now())
			ON CONFLICT (tenant_id, user_id)
			DO UPDATE SET score = EXCLUDED.score, signals = EXCLUDED.signals, updated_at = now()
			RETURNING score, signals, updated_at
		`, tenantID, total, payload).Scan(&score.Score, &score.Signals, &score.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("score tenant: %w", err)
	}
	return score, nil
}

// ScoreUser recomputes the per-user score from the configured
// signals. Passing an empty user ID returns the tenant-level score
// instead.
func (a *AbuseScorer) ScoreUser(ctx context.Context, tenantID, userID string) (*AbuseScore, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if userID == "" {
		return a.ScoreTenant(ctx, tenantID)
	}
	score := &AbuseScore{TenantID: tenantID, UserID: userID}
	if a.pool == nil {
		score.Signals = json.RawMessage(`{}`)
		score.UpdatedAt = time.Now()
		return score, nil
	}
	signals, total, err := a.computeUserSignals(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(signals)
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO abuse_scores (tenant_id, user_id, score, signals, updated_at)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, now())
			ON CONFLICT (tenant_id, user_id)
			DO UPDATE SET score = EXCLUDED.score, signals = EXCLUDED.signals, updated_at = now()
			RETURNING score, signals, updated_at
		`, tenantID, userID, total, payload).Scan(&score.Score, &score.Signals, &score.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("score user: %w", err)
	}
	return score, nil
}

// DetectAnomalies runs the full signal pack and persists an
// AbuseAlert for each threshold breach. Returns the alerts that
// were newly raised on this evaluation pass.
func (a *AbuseScorer) DetectAnomalies(ctx context.Context, tenantID string) ([]AbuseAlert, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if a.pool == nil {
		return nil, nil
	}
	signals, _, err := a.computeTenantSignals(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var raised []AbuseAlert
	now := time.Now()
	for _, sig := range signals.toAlerts(tenantID, now) {
		alert, err := a.insertAlert(ctx, tenantID, sig)
		if err != nil {
			return nil, err
		}
		raised = append(raised, *alert)
	}
	return raised, nil
}

// ListAlerts returns a page of alerts ordered by `created_at DESC`.
func (a *AbuseScorer) ListAlerts(ctx context.Context, tenantID string, opts ListAlertsOptions) ([]AbuseAlert, error) {
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
	var out []AbuseAlert
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
			SELECT id::text, tenant_id::text, COALESCE(user_id::text,''),
			       alert_type, severity, score, details,
			       acknowledged, created_at
			FROM abuse_alerts
			WHERE `+where+`
			ORDER BY created_at DESC
			LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var al AbuseAlert
			var details []byte
			if err := rows.Scan(&al.ID, &al.TenantID, &al.UserID, &al.AlertType, &al.Severity, &al.Score, &details, &al.Acknowledged, &al.CreatedAt); err != nil {
				return err
			}
			al.Details = json.RawMessage(details)
			out = append(out, al)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list abuse alerts: %w", err)
	}
	return out, nil
}

// AcknowledgeAlert flips the `acknowledged` flag on a single alert.
func (a *AbuseScorer) AcknowledgeAlert(ctx context.Context, tenantID, alertID string) error {
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
			UPDATE abuse_alerts SET acknowledged = true
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
		return fmt.Errorf("ack abuse alert: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------
// Signal computation
// ----------------------------------------------------------------

// abuseSignals captures the raw numbers that feed the composite
// score and the per-alert thresholds.
type abuseSignals struct {
	VolumeLast24h       int     `json:"volume_last_24h"`
	VolumeAverage7d     float64 `json:"volume_average_7d"`
	VolumeSpikeRatio    float64 `json:"volume_spike_ratio"`
	NewDomainsLast24h   int     `json:"new_domains_last_24h"`
	TotalDomainsLast24h int     `json:"total_domains_last_24h"`
	NewDomainsRatio     float64 `json:"new_domains_ratio"`
	AuthFailuresLast5m  int     `json:"auth_failures_last_5m"`
	HardBouncesLast24h  int     `json:"hard_bounces_last_24h"`
	SendsLast24h        int     `json:"sends_last_24h"`
	BounceRate          float64 `json:"bounce_rate"`
	ComplaintsLast24h   int     `json:"complaints_last_24h"`
	ComplaintRate       float64 `json:"complaint_rate"`
}

func (s abuseSignals) compositeScore() int {
	var score int
	if s.VolumeSpikeRatio >= 3.0 {
		score += 20
	}
	if s.NewDomainsRatio >= 0.5 {
		score += 15
	}
	if s.AuthFailuresLast5m >= 10 {
		score += 25
	}
	if s.BounceRate >= 0.05 {
		score += 20
	}
	if s.ComplaintRate >= 0.001 {
		score += 20
	}
	if score > 100 {
		score = 100
	}
	return score
}

// signalAlert describes one raisable alert.
type signalAlert struct {
	alertType string
	severity  string
	score     int
	details   map[string]any
}

func (s abuseSignals) toAlerts(tenantID string, _ time.Time) []signalAlert {
	var out []signalAlert
	if s.VolumeSpikeRatio >= 3.0 {
		out = append(out, signalAlert{
			alertType: AlertTypeVolumeSpike,
			severity:  severityForRatio(s.VolumeSpikeRatio, 3.0, 10.0),
			score:     int(s.VolumeSpikeRatio * 10),
			details: map[string]any{
				"volume_last_24h":   s.VolumeLast24h,
				"volume_average_7d": s.VolumeAverage7d,
				"ratio":             s.VolumeSpikeRatio,
			},
		})
	}
	if s.NewDomainsRatio >= 0.5 && s.TotalDomainsLast24h > 0 {
		out = append(out, signalAlert{
			alertType: AlertTypeRecipientAnomaly,
			severity:  severityForRatio(s.NewDomainsRatio, 0.5, 0.9),
			score:     int(s.NewDomainsRatio * 100),
			details: map[string]any{
				"new_domains":   s.NewDomainsLast24h,
				"total_domains": s.TotalDomainsLast24h,
				"ratio":         s.NewDomainsRatio,
			},
		})
	}
	if s.AuthFailuresLast5m >= 10 {
		out = append(out, signalAlert{
			alertType: AlertTypeAuthFailureStorm,
			severity:  severityForRatio(float64(s.AuthFailuresLast5m), 10, 100),
			score:     s.AuthFailuresLast5m,
			details: map[string]any{
				"auth_failures_last_5m": s.AuthFailuresLast5m,
			},
		})
	}
	if s.BounceRate >= 0.05 {
		out = append(out, signalAlert{
			alertType: AlertTypeHighBounceRate,
			severity:  severityForRatio(s.BounceRate, 0.05, 0.15),
			score:     int(s.BounceRate * 100),
			details: map[string]any{
				"bounce_rate":           s.BounceRate,
				"hard_bounces_last_24h": s.HardBouncesLast24h,
				"sends_last_24h":        s.SendsLast24h,
			},
		})
	}
	if s.ComplaintRate >= 0.001 {
		out = append(out, signalAlert{
			alertType: AlertTypeHighComplaintRate,
			severity:  severityForRatio(s.ComplaintRate, 0.001, 0.005),
			score:     int(s.ComplaintRate * 1000),
			details: map[string]any{
				"complaint_rate":     s.ComplaintRate,
				"complaints_last_24h": s.ComplaintsLast24h,
				"sends_last_24h":     s.SendsLast24h,
			},
		})
	}
	_ = tenantID
	return out
}

func severityForRatio(value, warn, crit float64) string {
	switch {
	case value >= crit*1.5:
		return SeverityCritical
	case value >= crit:
		return SeverityHigh
	case value >= warn:
		return SeverityMedium
	default:
		return SeverityLow
	}
}

// computeTenantSignals samples the deliverability tables for the
// current tenant. Missing tables fail the transaction; that is the
// caller's signal to run the migrations.
func (a *AbuseScorer) computeTenantSignals(ctx context.Context, tenantID string) (abuseSignals, int, error) {
	sig := abuseSignals{}
	// 24h send count + 7-day average come from the
	// SendLimitService Valkey counters; bounce / complaint counts
	// come from the deliverability tables. Using the send count as
	// the denominator is what the thresholds in
	// docs/PROPOSAL.md §9.4 are calibrated for.
	sends24h, err := a.sendLimit.GetDailyVolume(ctx, tenantID, time.Now().UTC())
	if err != nil {
		return sig, 0, fmt.Errorf("compute tenant signals: %w", err)
	}
	avgSends, err := a.sendLimit.AverageDailyVolume(ctx, tenantID, 7)
	if err != nil {
		return sig, 0, fmt.Errorf("compute tenant signals: %w", err)
	}
	sig.SendsLast24h = int(sends24h)
	sig.VolumeLast24h = int(sends24h)
	sig.VolumeAverage7d = avgSends
	if avgSends > 0 {
		sig.VolumeSpikeRatio = float64(sends24h) / avgSends
	}
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE bounce_type = 'hard'),
				COUNT(*) FILTER (WHERE bounce_type = 'complaint')
			FROM bounce_events
			WHERE tenant_id = $1::uuid AND created_at >= now() - interval '24 hours'
		`, tenantID).Scan(&sig.HardBouncesLast24h, &sig.ComplaintsLast24h); err != nil {
			return err
		}
		if sig.SendsLast24h > 0 {
			sig.BounceRate = float64(sig.HardBouncesLast24h) / float64(sig.SendsLast24h)
			sig.ComplaintRate = float64(sig.ComplaintsLast24h) / float64(sig.SendsLast24h)
		}
		return nil
	})
	if err != nil {
		return sig, 0, fmt.Errorf("compute tenant signals: %w", err)
	}
	return sig, sig.compositeScore(), nil
}

func (a *AbuseScorer) computeUserSignals(ctx context.Context, tenantID, userID string) (abuseSignals, int, error) {
	// User-scoped signals re-use the tenant query today. When the
	// per-user audit-log extraction lands this is where the
	// actor-filtered counts will live.
	_ = userID
	return a.computeTenantSignals(ctx, tenantID)
}

func (a *AbuseScorer) insertAlert(ctx context.Context, tenantID string, sig signalAlert) (*AbuseAlert, error) {
	details, _ := json.Marshal(sig.details)
	alert := &AbuseAlert{TenantID: tenantID, AlertType: sig.alertType, Severity: sig.severity, Score: sig.score, Details: details}
	if a.pool == nil {
		alert.ID = "stub"
		alert.CreatedAt = time.Now()
		return alert, nil
	}
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO abuse_alerts (tenant_id, alert_type, severity, score, details)
			VALUES ($1::uuid, $2, $3, $4, $5::jsonb)
			RETURNING id::text, created_at
		`, tenantID, sig.alertType, sig.severity, sig.score, details).Scan(&alert.ID, &alert.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insert abuse alert: %w", err)
	}
	return alert, nil
}
