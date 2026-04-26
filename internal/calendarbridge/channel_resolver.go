package calendarbridge

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// DBChannelResolver resolves notification channels from the
// `calendar_notification_channels` table. The lookup order is:
//
//  1. Per-calendar override (calendar_id = ev.CalendarID)
//  2. Tenant default (calendar_id IS NULL)
//  3. Static fallback supplied at construction time
//
// This lets tenants route bookable-resource notifications (one
// channel per meeting room) without losing the env-configured
// catch-all.
type DBChannelResolver struct {
	Pool     *pgxpool.Pool
	Fallback string
}

// NewDBChannelResolver returns a DBChannelResolver.
func NewDBChannelResolver(pool *pgxpool.Pool, fallback string) *DBChannelResolver {
	return &DBChannelResolver{Pool: pool, Fallback: fallback}
}

// ResolveChannel implements ChannelResolver.
func (r *DBChannelResolver) ResolveChannel(ctx context.Context, tenantID, calendarID string) (string, error) {
	if r == nil || r.Pool == nil {
		if r != nil && r.Fallback != "" {
			return r.Fallback, nil
		}
		return "", errors.New("calendarbridge: no notification channel configured")
	}
	var channel string
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Calendar-specific row first.
		if calendarID != "" {
			err := tx.QueryRow(ctx, `
				SELECT channel_id FROM calendar_notification_channels
				WHERE tenant_id = $1::uuid AND calendar_id = $2
			`, tenantID, calendarID).Scan(&channel)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		// Tenant default.
		err := tx.QueryRow(ctx, `
			SELECT channel_id FROM calendar_notification_channels
			WHERE tenant_id = $1::uuid AND calendar_id IS NULL
		`, tenantID).Scan(&channel)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		// Fail open to the static fallback so a transient DB
		// outage does not silence calendar notifications.
		if r.Fallback != "" {
			return r.Fallback, nil
		}
		return "", err
	}
	if channel == "" {
		if r.Fallback != "" {
			return r.Fallback, nil
		}
		return "", errors.New("calendarbridge: no notification channel configured")
	}
	return channel, nil
}

// CalendarChannelMapping is the public view of one row in
// `calendar_notification_channels`.
type CalendarChannelMapping struct {
	TenantID   string    `json:"tenant_id"`
	CalendarID string    `json:"calendar_id,omitempty"`
	ChannelID  string    `json:"channel_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SetCalendarChannel upserts a mapping. Pass an empty calendarID
// to update the tenant default.
func (r *DBChannelResolver) SetCalendarChannel(ctx context.Context, tenantID, calendarID, channelID string) (*CalendarChannelMapping, error) {
	if r.Pool == nil {
		return nil, errors.New("calendarbridge: pool not configured")
	}
	if tenantID == "" || channelID == "" {
		return nil, errors.New("calendarbridge: tenant + channel required")
	}
	var out CalendarChannelMapping
	var calRef any
	if calendarID != "" {
		calRef = calendarID
	}
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO calendar_notification_channels (tenant_id, calendar_id, channel_id)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (tenant_id, COALESCE(calendar_id, ''))
			DO UPDATE SET channel_id = EXCLUDED.channel_id, updated_at = now()
			RETURNING tenant_id::text, COALESCE(calendar_id, ''), channel_id, created_at, updated_at
		`, tenantID, calRef, channelID).Scan(
			&out.TenantID, &out.CalendarID, &out.ChannelID, &out.CreatedAt, &out.UpdatedAt,
		)
	})
	return &out, err
}

// GetCalendarChannel returns the mapping for a given calendar (or
// the tenant default when calendarID is empty). Returns
// (nil, nil) when no mapping exists.
func (r *DBChannelResolver) GetCalendarChannel(ctx context.Context, tenantID, calendarID string) (*CalendarChannelMapping, error) {
	if r.Pool == nil {
		return nil, nil
	}
	var out CalendarChannelMapping
	var calRef any
	if calendarID != "" {
		calRef = calendarID
	}
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT tenant_id::text, COALESCE(calendar_id, ''), channel_id, created_at, updated_at
			FROM calendar_notification_channels
			WHERE tenant_id = $1::uuid AND COALESCE(calendar_id, '') = COALESCE($2, '')
		`, tenantID, calRef).Scan(
			&out.TenantID, &out.CalendarID, &out.ChannelID, &out.CreatedAt, &out.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteCalendarChannel removes a mapping.
func (r *DBChannelResolver) DeleteCalendarChannel(ctx context.Context, tenantID, calendarID string) error {
	if r.Pool == nil {
		return errors.New("calendarbridge: pool not configured")
	}
	var calRef any
	if calendarID != "" {
		calRef = calendarID
	}
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM calendar_notification_channels
			WHERE tenant_id = $1::uuid AND COALESCE(calendar_id, '') = COALESCE($2, '')
		`, tenantID, calRef)
		return err
	})
}
