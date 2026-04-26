package onboarding

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// EventToStep maps internal webhook event types onto onboarding
// step IDs. The webhook worker calls `Handle` whenever it
// observes one of these events on the internal bus; the
// auto-trigger row is inserted (idempotently) and the next
// `GetChecklist` call surfaces the step as auto-completed.
var EventToStep = map[string]string{
	"email.received":  "send_test_email",
	"domain.verified": "verify_dns",
}

// AutoTrigger is the public projection of one row.
type AutoTrigger struct {
	StepKey     string    `json:"step_key"`
	EventType   string    `json:"event_type"`
	CompletedAt time.Time `json:"completed_at"`
}

// AutoTriggerService records auto-completion events so the
// onboarding checklist UI can render a "completed automatically"
// badge.
type AutoTriggerService struct {
	pool *pgxpool.Pool
}

// NewAutoTriggerService returns a service.
func NewAutoTriggerService(pool *pgxpool.Pool) *AutoTriggerService {
	return &AutoTriggerService{pool: pool}
}

// OnWebhookEvent implements webhooks.EventListener so the
// auto-trigger service can be plugged into the webhook fan-out
// without taking a hard dependency on the webhooks package.
func (s *AutoTriggerService) OnWebhookEvent(ctx context.Context, tenantID, eventType string, _ map[string]any) error {
	return s.Handle(ctx, tenantID, eventType)
}

// Handle inspects an event and, if it maps to a step, records the
// auto-trigger row. user.created triggers `invite_team` only when
// the resulting active user count is >= 2.
func (s *AutoTriggerService) Handle(ctx context.Context, tenantID, eventType string) error {
	if s == nil || s.pool == nil || tenantID == "" || eventType == "" {
		return nil
	}
	step := ""
	switch eventType {
	case "user.created":
		// Multi-user signal — count active users; only mark
		// invite_team complete after the second user lands.
		ok, err := s.tenantHasMultipleUsers(ctx, tenantID)
		if err != nil {
			return err
		}
		if ok {
			step = "invite_team"
		}
	default:
		if mapped, ok := EventToStep[eventType]; ok {
			step = mapped
		}
	}
	if step == "" {
		return nil
	}
	return s.upsert(ctx, tenantID, step, eventType)
}

func (s *AutoTriggerService) upsert(ctx context.Context, tenantID, stepKey, eventType string) error {
	if strings.TrimSpace(stepKey) == "" {
		return errors.New("onboarding: step required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO onboarding_auto_triggers (tenant_id, step_key, event_type)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (tenant_id, step_key) DO NOTHING
		`, tenantID, stepKey, eventType)
		return err
	})
}

// List returns every auto-trigger row for a tenant.
func (s *AutoTriggerService) List(ctx context.Context, tenantID string) ([]AutoTrigger, error) {
	if s == nil || s.pool == nil || tenantID == "" {
		return nil, nil
	}
	var out []AutoTrigger
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT step_key, event_type, completed_at
			FROM onboarding_auto_triggers
			WHERE tenant_id = $1::uuid
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t AutoTrigger
			if err := rows.Scan(&t.StepKey, &t.EventType, &t.CompletedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// Reset clears every auto-trigger row for a tenant. Used by the
// "reset checklist" admin action.
func (s *AutoTriggerService) Reset(ctx context.Context, tenantID string) error {
	if s == nil || s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM onboarding_auto_triggers WHERE tenant_id = $1::uuid`, tenantID)
		return err
	})
}

func (s *AutoTriggerService) tenantHasMultipleUsers(ctx context.Context, tenantID string) (bool, error) {
	var n int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM users
			WHERE tenant_id = $1::uuid AND account_type = 'user' AND status = 'active'
		`, tenantID).Scan(&n)
	})
	return n >= 2, err
}
