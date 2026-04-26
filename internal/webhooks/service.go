// Package webhooks — Phase 5 tenant outbound webhooks.
//
// Service publishes events (`email.received`, `email.bounced`,
// `email.complaint`, `calendar.event_created`,
// `calendar.event_updated`, `migration.completed`) to tenant-
// registered HTTP endpoints. Deliveries are signed with
// HMAC-SHA256 (`X-KMail-Signature: t=<unix>,v1=<hex>`) and
// retried with exponential backoff (3 attempts) by the worker.
//
// Endpoints carry a `secret_hash` (SHA-256) so plaintext secrets
// only exist at registration time. Event filtering is per-endpoint
// via the `events` JSONB column — empty array means "deliver every
// event".
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Event types.
const (
	EventEmailReceived     = "email.received"
	EventEmailBounced      = "email.bounced"
	EventEmailComplaint    = "email.complaint"
	EventCalendarCreated   = "calendar.event_created"
	EventCalendarUpdated   = "calendar.event_updated"
	EventMigrationDone     = "migration.completed"
)

// MaxAttempts is the max number of delivery attempts before
// a delivery row is marked failed.
const MaxAttempts = 3

// Signing versions.
const (
	SigningV1 = "v1"
	SigningV2 = "v2"
)

// Endpoint is the public view of a `webhook_endpoints` row.
type Endpoint struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	URL            string    `json:"url"`
	Events         []string  `json:"events"`
	Active         bool      `json:"active"`
	SigningVersion string    `json:"signing_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Delivery is the public view of a `webhook_deliveries` row.
type Delivery struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	EndpointID   string     `json:"endpoint_id"`
	EventType    string     `json:"event_type"`
	Status       string     `json:"status"`
	Attempts     int        `json:"attempts"`
	LastError    string     `json:"last_error,omitempty"`
	LastStatus   int        `json:"last_status,omitempty"`
	NextRetryAt  time.Time  `json:"next_retry_at"`
	CreatedAt    time.Time  `json:"created_at"`
	DeliveredAt  *time.Time `json:"delivered_at,omitempty"`
}

// EventListener receives every internal event the BFF dispatches
// via DeliverEvent. Listeners run inline (best-effort, errors
// logged) so they observe the same firehose as the external HTTP
// fan-out.
type EventListener interface {
	OnWebhookEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) error
}

// Service implements the webhook public API.
type Service struct {
	pool      *pgxpool.Pool
	listeners []EventListener
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// AddListener registers an in-process listener that observes every
// DeliverEvent call.
func (s *Service) AddListener(l EventListener) {
	if l == nil {
		return
	}
	s.listeners = append(s.listeners, l)
}

// RegisterWebhook inserts a new endpoint, returning the plaintext
// secret (only available once).
func (s *Service) RegisterWebhook(ctx context.Context, tenantID, urlStr string, events []string, signingVersion string) (*Endpoint, string, error) {
	if tenantID == "" || urlStr == "" {
		return nil, "", errors.New("webhooks: tenant + url required")
	}
	if signingVersion == "" {
		signingVersion = SigningV1
	}
	if signingVersion != SigningV1 && signingVersion != SigningV2 {
		return nil, "", errors.New("webhooks: signing_version must be v1 or v2")
	}
	secret, err := randSecret()
	if err != nil {
		return nil, "", err
	}
	hash := hashSecret(secret)
	if events == nil {
		events = []string{}
	}
	eventsJSON, _ := json.Marshal(events)
	var ep Endpoint
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var rawEvents []byte
		err := tx.QueryRow(ctx, `
			INSERT INTO webhook_endpoints (tenant_id, url, events, secret_hash, signing_version)
			VALUES ($1::uuid, $2, $3::jsonb, $4, $5)
			RETURNING id::text, tenant_id::text, url, events, active, signing_version, created_at, updated_at
		`, tenantID, urlStr, string(eventsJSON), hash, signingVersion).Scan(
			&ep.ID, &ep.TenantID, &ep.URL, &rawEvents, &ep.Active, &ep.SigningVersion, &ep.CreatedAt, &ep.UpdatedAt,
		)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(rawEvents, &ep.Events)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return &ep, secret, nil
}

// UpdateSigningVersion flips the signing version on an endpoint.
func (s *Service) UpdateSigningVersion(ctx context.Context, tenantID, id, version string) error {
	if s.pool == nil {
		return errors.New("webhooks: pool not configured")
	}
	if version != SigningV1 && version != SigningV2 {
		return errors.New("webhooks: signing_version must be v1 or v2")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE webhook_endpoints SET signing_version = $3, updated_at = now()
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, id, tenantID, version)
		return err
	})
}

// ListWebhooks returns all endpoints for a tenant.
func (s *Service) ListWebhooks(ctx context.Context, tenantID string) ([]Endpoint, error) {
	if s.pool == nil || tenantID == "" {
		return nil, nil
	}
	var out []Endpoint
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, url, events, active, signing_version, created_at, updated_at
			FROM webhook_endpoints WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ep Endpoint
			var rawEvents []byte
			if err := rows.Scan(&ep.ID, &ep.TenantID, &ep.URL, &rawEvents, &ep.Active, &ep.SigningVersion, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(rawEvents, &ep.Events)
			out = append(out, ep)
		}
		return rows.Err()
	})
	return out, err
}

// TestFire enqueues a synthetic `webhook.ping` delivery for the
// endpoint so admins can verify connectivity from the UI.
func (s *Service) TestFire(ctx context.Context, tenantID, endpointID string) (int, error) {
	if s.pool == nil || tenantID == "" || endpointID == "" {
		return 0, errors.New("webhooks: tenant + endpoint required")
	}
	body, _ := json.Marshal(map[string]any{
		"event": "webhook.ping",
		"sent_at": time.Now().UTC().Format(time.RFC3339),
		"tenant_id": tenantID,
	})
	var enqueued int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM webhook_endpoints
				WHERE id = $1::uuid AND tenant_id = $2::uuid AND active = true)
		`, endpointID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errors.New("webhooks: endpoint not found or inactive")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_type, payload)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
		`, tenantID, endpointID, "webhook.ping", string(body)); err != nil {
			return err
		}
		enqueued = 1
		return nil
	})
	return enqueued, err
}

// DeleteWebhook removes an endpoint.
func (s *Service) DeleteWebhook(ctx context.Context, tenantID, id string) error {
	if s.pool == nil {
		return errors.New("webhooks: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM webhook_endpoints WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, id, tenantID)
		return err
	})
}

// DeliverEvent enqueues a delivery for every endpoint subscribed
// to the event type. Returns the number of deliveries enqueued.
func (s *Service) DeliverEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) (int, error) {
	if s.pool == nil || tenantID == "" || eventType == "" {
		return 0, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var enqueued int
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Filter: events array is empty (-> all) OR contains the event.
		rows, err := tx.Query(ctx, `
			SELECT id::text FROM webhook_endpoints
			WHERE tenant_id = $1::uuid AND active = true
			  AND (jsonb_array_length(events) = 0 OR events ? $2)
		`, tenantID, eventType)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `
				INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_type, payload)
				VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
			`, tenantID, id, eventType, string(body)); err != nil {
				return err
			}
			enqueued++
		}
		return nil
	})
	if err == nil {
		for _, l := range s.listeners {
			_ = l.OnWebhookEvent(ctx, tenantID, eventType, payload)
		}
	}
	return enqueued, err
}

// ListDeliveries returns recent deliveries for a tenant.
func (s *Service) ListDeliveries(ctx context.Context, tenantID string, limit int) ([]Delivery, error) {
	if s.pool == nil || tenantID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Delivery
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, endpoint_id::text, event_type, status,
			       attempts, last_error, last_status, next_retry_at, created_at, delivered_at
			FROM webhook_deliveries
			WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
			LIMIT $2
		`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Delivery
			if err := rows.Scan(
				&d.ID, &d.TenantID, &d.EndpointID, &d.EventType, &d.Status,
				&d.Attempts, &d.LastError, &d.LastStatus, &d.NextRetryAt, &d.CreatedAt, &d.DeliveredAt,
			); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// SignPayload returns the `t=<unix>,v1=<hex>` signature header
// value for a payload + secret. Exposed so middleware tests can
// verify the format.
func SignPayload(secret string, ts time.Time, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// SignPayloadV2 returns the `v2=<hex>` signature header value for
// the v2 scheme. The signed string is `<unix>.<nonce>.<body>` so
// the nonce participates in the MAC and the receiver can dedupe
// replays by nonce inside the timestamp window. Tenants verify by
// recomputing HMAC-SHA256 with the secret hash they stored at
// registration time.
func SignPayloadV2(secret string, ts time.Time, nonce string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s.", ts.Unix(), nonce)
	mac.Write(payload)
	return fmt.Sprintf("v2=%s", hex.EncodeToString(mac.Sum(nil)))
}

// NewNonce returns a fresh URL-safe nonce for v2 signatures.
func NewNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
