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

// Feedback-loop source values — kept in sync with the CHECK
// constraint on `feedback_loop_events.source`.
const (
	FeedbackSourceGmailPostmaster = "gmail_postmaster"
	FeedbackSourceYahooARF        = "yahoo_arf"
)

// PostmasterData is the shape the Gmail Postmaster Tools JSON
// export lands in. Only the fields the UI needs are modelled; the
// rest are preserved as raw JSON inside the persisted `data` blob
// so future UI changes do not require another migration.
type PostmasterData struct {
	Domain             string  `json:"domain"`
	SpamRate           float64 `json:"spam_rate"`
	IPReputation       string  `json:"ip_reputation"`
	DomainReputation   string  `json:"domain_reputation"`
	DeliveryErrors     float64 `json:"delivery_errors"`
	AuthenticationRate float64 `json:"authentication_rate,omitempty"`
	EncryptionRate     float64 `json:"encryption_rate,omitempty"`
	Date               string  `json:"date,omitempty"`
}

// ARFReport mirrors the headers of an RFC 5965 Abuse Reporting
// Format message-report MIME part (the feedback-report/third part
// Yahoo ships). Callers that receive the raw ARF blob can unmarshal
// it through ParseARF below.
type ARFReport struct {
	FeedbackType    string    `json:"feedback_type"`
	SourceIP        string    `json:"source_ip"`
	OriginalRcptTo  string    `json:"original_rcpt_to"`
	ArrivalDate     time.Time `json:"arrival_date"`
	OriginalMailID  string    `json:"original_mail_id,omitempty"`
	ReportingMTA    string    `json:"reporting_mta,omitempty"`
	UserAgent       string    `json:"user_agent,omitempty"`
	AuthResults     string    `json:"auth_results,omitempty"`
	SourceDomain    string    `json:"source_domain,omitempty"`
}

// FeedbackEvent is the API + persisted shape of a normalized
// feedback-loop event.
type FeedbackEvent struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenant_id"`
	Source    string          `json:"source"`
	EventType string          `json:"event_type"`
	Domain    string          `json:"domain"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}

// FeedbackSummary is the per-domain aggregate surfaced to the admin
// dashboard.
type FeedbackSummary struct {
	TenantID            string  `json:"tenant_id"`
	Domain              string  `json:"domain,omitempty"`
	GmailEvents         int     `json:"gmail_events"`
	YahooEvents         int     `json:"yahoo_events"`
	AverageSpamRate     float64 `json:"average_spam_rate"`
	LatestIPReputation  string  `json:"latest_ip_reputation"`
	LatestDomainReputation string `json:"latest_domain_reputation"`
	LastEventAt         *time.Time `json:"last_event_at,omitempty"`
	WindowDays          int     `json:"window_days"`
}

// ListFeedbackEventsOptions filters the paginated listing.
type ListFeedbackEventsOptions struct {
	Source string
	Domain string
	Limit  int
	Offset int
}

// FeedbackLoopService owns the `feedback_loop_events` table.
type FeedbackLoopService struct {
	pool *pgxpool.Pool
}

// ProcessGmailPostmasterData normalises a PostmasterData payload
// and persists it as a single feedback event. Multiple daily rows
// (one per export window) can be ingested in sequence.
func (s *FeedbackLoopService) ProcessGmailPostmasterData(ctx context.Context, tenantID string, data PostmasterData) (*FeedbackEvent, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if data.Domain == "" {
		return nil, fmt.Errorf("%w: domain required", ErrInvalidInput)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal postmaster data: %w", err)
	}
	eventType := "daily_report"
	if data.SpamRate >= 0.003 {
		eventType = "high_spam_rate"
	}
	return s.insertEvent(ctx, tenantID, FeedbackSourceGmailPostmaster, eventType, data.Domain, payload)
}

// ProcessYahooARF persists an ARF report. Spec-conformant reports
// come in as RFC 5965 MIME parts; use ParseARF to translate the raw
// headers bytes into an ARFReport before calling this method.
func (s *FeedbackLoopService) ProcessYahooARF(ctx context.Context, tenantID string, report ARFReport) (*FeedbackEvent, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if report.FeedbackType == "" {
		report.FeedbackType = "abuse"
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("marshal arf report: %w", err)
	}
	domain := report.SourceDomain
	if domain == "" {
		domain = deriveDomainFromEmail(report.OriginalRcptTo)
	}
	return s.insertEvent(ctx, tenantID, FeedbackSourceYahooARF, report.FeedbackType, domain, payload)
}

// GetFeedbackSummary returns a 30-day aggregate for the tenant,
// optionally scoped to a specific domain.
func (s *FeedbackLoopService) GetFeedbackSummary(ctx context.Context, tenantID, domainID string) (*FeedbackSummary, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	sum := &FeedbackSummary{TenantID: tenantID, WindowDays: 30}
	if s.pool == nil {
		return sum, nil
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var args []any
		where := `tenant_id = $1::uuid AND created_at >= $2`
		args = append(args, tenantID, since)
		if domainID != "" {
			where += ` AND domain = $3`
			args = append(args, domainID)
			sum.Domain = domainID
		}
		row := tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE source = 'gmail_postmaster')::int,
				COUNT(*) FILTER (WHERE source = 'yahoo_arf')::int,
				COALESCE(AVG((data->>'spam_rate')::float)
					FILTER (WHERE source = 'gmail_postmaster'
						AND data ? 'spam_rate'), 0),
				MAX(created_at)
			FROM feedback_loop_events WHERE `+where, args...,
		)
		var last *time.Time
		if err := row.Scan(&sum.GmailEvents, &sum.YahooEvents, &sum.AverageSpamRate, &last); err != nil {
			return err
		}
		sum.LastEventAt = last
		// Latest reputation strings — consulted only when we have a
		// gmail row, otherwise the column is empty.
		if sum.GmailEvents > 0 {
			latest := tx.QueryRow(ctx, `
				SELECT COALESCE(data->>'ip_reputation', ''),
				       COALESCE(data->>'domain_reputation', '')
				FROM feedback_loop_events
				WHERE `+where+` AND source = 'gmail_postmaster'
				ORDER BY created_at DESC LIMIT 1`, args...)
			_ = latest.Scan(&sum.LatestIPReputation, &sum.LatestDomainReputation)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("feedback summary: %w", err)
	}
	return sum, nil
}

// ListFeedbackEvents paginates the raw event timeline.
func (s *FeedbackLoopService) ListFeedbackEvents(ctx context.Context, tenantID string, opts ListFeedbackEventsOptions) ([]FeedbackEvent, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Limit > 500 {
		opts.Limit = 500
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []FeedbackEvent
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		args := []any{tenantID}
		where := `tenant_id = $1::uuid`
		if opts.Source != "" {
			args = append(args, opts.Source)
			where += fmt.Sprintf(` AND source = $%d`, len(args))
		}
		if opts.Domain != "" {
			args = append(args, opts.Domain)
			where += fmt.Sprintf(` AND domain = $%d`, len(args))
		}
		args = append(args, opts.Limit, opts.Offset)
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, source, event_type, domain,
			       data, created_at
			FROM feedback_loop_events
			WHERE `+where+`
			ORDER BY created_at DESC
			LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e FeedbackEvent
			var data []byte
			if err := rows.Scan(&e.ID, &e.TenantID, &e.Source, &e.EventType, &e.Domain, &data, &e.CreatedAt); err != nil {
				return err
			}
			e.Data = json.RawMessage(data)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list feedback: %w", err)
	}
	return out, nil
}

func (s *FeedbackLoopService) insertEvent(ctx context.Context, tenantID, source, eventType, domain string, payload []byte) (*FeedbackEvent, error) {
	evt := &FeedbackEvent{TenantID: tenantID, Source: source, EventType: eventType, Domain: domain, Data: payload}
	if s.pool == nil {
		return evt, nil
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO feedback_loop_events (tenant_id, source, event_type, domain, data)
			VALUES ($1::uuid, $2, $3, $4, $5::jsonb)
			RETURNING id::text, created_at
		`, tenantID, source, eventType, domain, payload).Scan(&evt.ID, &evt.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insert feedback event: %w", err)
	}
	return evt, nil
}

// ParseARF parses a textual RFC 5965 feedback-report block into an
// ARFReport. Callers that ingest an entire MIME message should feed
// only the `message/feedback-report` part. Missing fields are left
// as zero values; unknown fields are ignored.
func ParseARF(body []byte) (ARFReport, error) {
	lines := splitARFLines(body)
	var r ARFReport
	for _, ln := range lines {
		name, value := splitHeader(ln)
		switch name {
		case "feedback-type":
			r.FeedbackType = value
		case "source-ip":
			r.SourceIP = value
		case "original-rcpt-to":
			r.OriginalRcptTo = value
		case "arrival-date":
			if t, err := time.Parse(time.RFC1123Z, value); err == nil {
				r.ArrivalDate = t
			} else if t, err := time.Parse(time.RFC1123, value); err == nil {
				r.ArrivalDate = t
			}
		case "original-mail-from":
			r.OriginalMailID = value
		case "reporting-mta":
			r.ReportingMTA = value
		case "user-agent":
			r.UserAgent = value
		case "authentication-results":
			r.AuthResults = value
		case "source":
			r.SourceDomain = value
		}
	}
	return r, nil
}

