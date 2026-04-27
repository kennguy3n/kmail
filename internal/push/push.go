// Package push hosts the KMail mobile / web push-notification
// service. Subscriptions are persisted to Postgres (RLS-scoped),
// notification preferences are stored per user, and a JMAP
// EventSource fan-out worker translates Stalwart state changes
// into push messages.
//
// This package is intentionally transport-agnostic: actual
// delivery to APNs / FCM / Web Push lives behind the Transport
// interface so ops can swap providers without touching the
// Service.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ErrInvalidInput wraps caller-visible validation failures.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when a row lookup resolves nothing.
var ErrNotFound = errors.New("not found")

// Config wires NewService.
type Config struct {
	Pool        *pgxpool.Pool
	StalwartURL string
	HTTPClient  *http.Client
	Logger      *log.Logger
	// Transport is the outbound push provider. Nil uses a no-op
	// logger so development doesn't need FCM / APNs credentials.
	Transport Transport
}

// Subscription is the API + persisted shape of one push
// subscription.
type Subscription struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	UserID       string    `json:"user_id"`
	DeviceType   string    `json:"device_type"`
	PushEndpoint string    `json:"push_endpoint"`
	AuthKey      string    `json:"auth_key,omitempty"`
	P256DHKey    string    `json:"p256dh_key,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// NotificationPreference is the per-user toggle set.
type NotificationPreference struct {
	TenantID         string    `json:"tenant_id"`
	UserID           string    `json:"user_id"`
	NewEmail         bool      `json:"new_email"`
	CalendarReminder bool      `json:"calendar_reminder"`
	SharedInbox      bool      `json:"shared_inbox"`
	QuietHoursStart  string    `json:"quiet_hours_start"`
	QuietHoursEnd    string    `json:"quiet_hours_end"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Notification is the outbound payload SendNotification hands to
// the Transport.
type Notification struct {
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	Kind    string            `json:"kind"`
	Data    map[string]string `json:"data,omitempty"`
}

// Transport is the outbound push provider.
type Transport interface {
	Send(ctx context.Context, sub Subscription, n Notification) error
}

// Service is the root push service.
type Service struct {
	cfg Config
}

// NewService builds a Service with sensible defaults.
func NewService(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Transport == nil {
		cfg.Transport = loggingTransport{logger: cfg.Logger}
	}
	return &Service{cfg: cfg}
}

// Subscribe registers (or re-registers) a push subscription for
// the given user. Returns the persisted row.
func (s *Service) Subscribe(ctx context.Context, tenantID, userID string, sub Subscription) (*Subscription, error) {
	if tenantID == "" || userID == "" {
		return nil, fmt.Errorf("%w: tenantID and userID required", ErrInvalidInput)
	}
	if sub.PushEndpoint == "" {
		return nil, fmt.Errorf("%w: push_endpoint required", ErrInvalidInput)
	}
	if sub.DeviceType == "" {
		sub.DeviceType = "web"
	}
	switch sub.DeviceType {
	case "web", "ios", "android":
	default:
		return nil, fmt.Errorf("%w: device_type must be web, ios, or android", ErrInvalidInput)
	}
	out := Subscription{TenantID: tenantID, UserID: userID, DeviceType: sub.DeviceType, PushEndpoint: sub.PushEndpoint, AuthKey: sub.AuthKey, P256DHKey: sub.P256DHKey}
	if s.cfg.Pool == nil {
		out.ID = "stub"
		out.CreatedAt = time.Now()
		return &out, nil
	}
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO push_subscriptions (tenant_id, user_id, device_type, push_endpoint, auth_key, p256dh_key)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, user_id, push_endpoint)
			DO UPDATE SET device_type = EXCLUDED.device_type,
			              auth_key = EXCLUDED.auth_key,
			              p256dh_key = EXCLUDED.p256dh_key
			RETURNING id::text, created_at
		`, tenantID, userID, sub.DeviceType, sub.PushEndpoint, sub.AuthKey, sub.P256DHKey).Scan(&out.ID, &out.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	return &out, nil
}

// Unsubscribe removes a single subscription.
func (s *Service) Unsubscribe(ctx context.Context, tenantID, userID, subscriptionID string) error {
	if tenantID == "" || userID == "" || subscriptionID == "" {
		return fmt.Errorf("%w: tenantID, userID, subscriptionID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		cmd, err := tx.Exec(ctx, `
			DELETE FROM push_subscriptions
			WHERE tenant_id = $1::uuid AND user_id = $2 AND id = $3::uuid
		`, tenantID, userID, subscriptionID)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	return nil
}

// ListSubscriptions returns every subscription owned by the user.
func (s *Service) ListSubscriptions(ctx context.Context, tenantID, userID string) ([]Subscription, error) {
	if tenantID == "" || userID == "" {
		return nil, fmt.Errorf("%w: tenantID and userID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil, nil
	}
	var out []Subscription
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, user_id,
			       device_type, push_endpoint, auth_key, p256dh_key, created_at
			FROM push_subscriptions
			WHERE tenant_id = $1::uuid AND user_id = $2
			ORDER BY created_at DESC
		`, tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sub Subscription
			if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.UserID, &sub.DeviceType, &sub.PushEndpoint, &sub.AuthKey, &sub.P256DHKey, &sub.CreatedAt); err != nil {
				return err
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	return out, nil
}

// SendNotification delivers a notification to every subscription
// the user has registered, respecting the preferences row.
func (s *Service) SendNotification(ctx context.Context, tenantID, userID string, n Notification) error {
	prefs, err := s.GetPreferences(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	if !prefs.allowsKind(n.Kind) {
		return nil
	}
	if inQuietHours(time.Now(), prefs.QuietHoursStart, prefs.QuietHoursEnd) && n.Kind != "calendar_reminder" {
		return nil
	}
	subs, err := s.ListSubscriptions(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if err := s.cfg.Transport.Send(ctx, sub, n); err != nil {
			s.cfg.Logger.Printf("push: send to %s failed: %v", sub.ID, err)
		}
	}
	return nil
}

// GetPreferences returns the user's preference row, filling in
// defaults (everything enabled, no quiet hours) when no row exists.
func (s *Service) GetPreferences(ctx context.Context, tenantID, userID string) (NotificationPreference, error) {
	prefs := NotificationPreference{TenantID: tenantID, UserID: userID, NewEmail: true, CalendarReminder: true, SharedInbox: true}
	if tenantID == "" || userID == "" {
		return prefs, fmt.Errorf("%w: tenantID and userID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return prefs, nil
	}
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			SELECT tenant_id::text, user_id, new_email,
			       calendar_reminder, shared_inbox,
			       quiet_hours_start, quiet_hours_end, updated_at
			FROM notification_preferences
			WHERE tenant_id = $1::uuid AND user_id = $2
		`, tenantID, userID)
		err := row.Scan(&prefs.TenantID, &prefs.UserID, &prefs.NewEmail, &prefs.CalendarReminder, &prefs.SharedInbox, &prefs.QuietHoursStart, &prefs.QuietHoursEnd, &prefs.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return prefs, fmt.Errorf("get preferences: %w", err)
	}
	return prefs, nil
}

// UpdatePreferences upserts the preferences row.
func (s *Service) UpdatePreferences(ctx context.Context, tenantID, userID string, prefs NotificationPreference) (NotificationPreference, error) {
	if tenantID == "" || userID == "" {
		return prefs, fmt.Errorf("%w: tenantID and userID required", ErrInvalidInput)
	}
	prefs.TenantID = tenantID
	prefs.UserID = userID
	if s.cfg.Pool == nil {
		prefs.UpdatedAt = time.Now()
		return prefs, nil
	}
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO notification_preferences (
				tenant_id, user_id, new_email, calendar_reminder,
				shared_inbox, quiet_hours_start, quiet_hours_end, updated_at
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, now())
			ON CONFLICT (tenant_id, user_id)
			DO UPDATE SET new_email = EXCLUDED.new_email,
			              calendar_reminder = EXCLUDED.calendar_reminder,
			              shared_inbox = EXCLUDED.shared_inbox,
			              quiet_hours_start = EXCLUDED.quiet_hours_start,
			              quiet_hours_end = EXCLUDED.quiet_hours_end,
			              updated_at = now()
			RETURNING updated_at
		`, tenantID, userID, prefs.NewEmail, prefs.CalendarReminder, prefs.SharedInbox, prefs.QuietHoursStart, prefs.QuietHoursEnd).Scan(&prefs.UpdatedAt)
	})
	if err != nil {
		return prefs, fmt.Errorf("update preferences: %w", err)
	}
	return prefs, nil
}

// StalwartURL returns the configured JMAP endpoint used by the
// EventSource fan-out worker.
func (s *Service) StalwartURL() string { return s.cfg.StalwartURL }

func (p NotificationPreference) allowsKind(kind string) bool {
	switch kind {
	case "new_email":
		return p.NewEmail
	case "calendar_reminder":
		return p.CalendarReminder
	case "shared_inbox":
		return p.SharedInbox
	default:
		return true
	}
}

// inQuietHours returns true when `now` falls between the start /
// end times expressed as "HH:MM". An empty start or end disables
// the quiet-hours window.
func inQuietHours(now time.Time, start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	sH, sM, ok1 := parseHM(start)
	eH, eM, ok2 := parseHM(end)
	if !ok1 || !ok2 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	s := sH*60 + sM
	e := eH*60 + eM
	if s == e {
		return false
	}
	if s < e {
		return cur >= s && cur < e
	}
	return cur >= s || cur < e
}

func parseHM(v string) (int, int, bool) {
	if len(v) != 5 || v[2] != ':' {
		return 0, 0, false
	}
	h := int(v[0]-'0')*10 + int(v[1]-'0')
	m := int(v[3]-'0')*10 + int(v[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// loggingTransport is the no-op development transport.
type loggingTransport struct{ logger *log.Logger }

func (t loggingTransport) Send(_ context.Context, sub Subscription, n Notification) error {
	payload, _ := json.Marshal(n)
	t.logger.Printf("push: subscription=%s device=%s payload=%s", sub.ID, sub.DeviceType, payload)
	return nil
}

// NewLoggingTransport returns the no-op transport that logs each
// notification instead of sending it. Useful as a Default on the
// TransportRouter so missing platform-specific transports degrade
// to logging rather than failing.
func NewLoggingTransport(logger *log.Logger) Transport {
	if logger == nil {
		logger = log.Default()
	}
	return loggingTransport{logger: logger}
}
