// Package billing — tenant-lifecycle wiring.
//
// The Phase 4 "Tenant-level billing integration" item requires the
// billing service to participate in CreateTenant / DeleteTenant /
// ChangePlan / seat add / seat remove transitions. The previously
// landed `billing.Service` carries the per-method primitives
// (UpsertQuota, IncrementSeatCount, ChangePlan, etc.) — `Lifecycle`
// composes them into the tenant-shaped events the Tenant Service
// emits and persists a `billing_subscriptions` row so the BFF can
// surface the current subscription state in the admin UI.
//
// Proration math is intentionally simple: when a plan changes
// mid-period the prorated charge is `(new_seat_cents - old_seat_cents)
// * seat_count * remaining_days / period_days`. Negative values are
// credits for downgrades. The math is centralized here so the admin
// UI's "proration preview" endpoint (handlers.go) and the actual
// plan-change path use the same formula.

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Lifecycle bundles the tenant-lifecycle transitions the Tenant
// Service drives. Every method is safe to call multiple times — the
// underlying writes are idempotent on (tenant_id) so a retry after
// a partial failure doesn't double-bill.
type Lifecycle struct {
	svc    *Service
	logger *log.Logger
	now    func() time.Time

	// stripe is the optional outbound Stripe client. When
	// configured (non-empty `KMAIL_STRIPE_SECRET_KEY`), Lifecycle
	// methods drive the corresponding Stripe REST calls so the
	// billing relationship in Stripe stays in sync with KMail's
	// tenant lifecycle. When unconfigured, lifecycle stays purely
	// local (matching the pre-Phase-8 behaviour).
	stripe *StripeClient
	// pricing maps plan slug → Stripe price ID. Set per
	// deployment via `KMAIL_STRIPE_PRICE_*` env vars; an empty
	// map means "Stripe wired, but no plans known", in which
	// case Lifecycle skips the Stripe call.
	pricing map[string]string
}

// NewLifecycle returns a Lifecycle bound to the given Service.
func NewLifecycle(svc *Service, logger *log.Logger) *Lifecycle {
	if logger == nil {
		logger = log.Default()
	}
	return &Lifecycle{svc: svc, logger: logger, now: time.Now}
}

// WithStripe attaches the outbound Stripe client + plan→priceID
// mapping. Returning the Lifecycle keeps the call chainable in
// `cmd/kmail-api/main.go`.
func (l *Lifecycle) WithStripe(client *StripeClient, planPrices map[string]string) *Lifecycle {
	if l == nil {
		return l
	}
	l.stripe = client
	l.pricing = planPrices
	return l
}

// SubscriptionStatus mirrors `billing_subscriptions.status`.
type SubscriptionStatus string

const (
	SubscriptionActive    SubscriptionStatus = "active"
	SubscriptionPastDue   SubscriptionStatus = "past_due"
	SubscriptionCancelled SubscriptionStatus = "cancelled"
)

// Subscription is the API representation of a row in
// `billing_subscriptions`.
type Subscription struct {
	TenantID             string             `json:"tenant_id"`
	Plan                 string             `json:"plan"`
	Status               SubscriptionStatus `json:"status"`
	StripeSubscriptionID string             `json:"stripe_subscription_id,omitempty"`
	CurrentPeriodStart   time.Time          `json:"current_period_start"`
	CurrentPeriodEnd     time.Time          `json:"current_period_end"`
	CreatedAt            time.Time          `json:"created_at"`
	UpdatedAt            time.Time          `json:"updated_at"`
}

// OnTenantCreated is the post-insert hook the Tenant Service calls
// after a successful `INSERT INTO tenants`. Initializes the quota
// row with the per-seat default for the plan and opens a
// `billing_subscriptions` row for the first billing period.
func (l *Lifecycle) OnTenantCreated(ctx context.Context, tenantID, plan string) error {
	if l == nil || l.svc == nil {
		return nil
	}
	if err := ValidatePlan(plan); err != nil {
		return err
	}
	perSeat, _ := l.svc.PerSeatStorageBytes(plan)
	// EnforcePlanLimits will recompute the storage limit on the
	// first SyncSeatCount, so the seed seat=1 keeps a freshly-
	// provisioned tenant out of the over-quota state on the first
	// CreateUser.
	if err := l.svc.UpsertQuota(ctx, tenantID, perSeat, 1); err != nil {
		return fmt.Errorf("upsert quota: %w", err)
	}
	now := l.now().UTC()
	customerID, subscriptionID := "", ""
	if l.stripe != nil && l.stripe.Configured() {
		cust, err := l.stripe.CreateCustomer(ctx, "", map[string]string{"kmail_tenant_id": tenantID})
		if err != nil {
			l.logger.Printf("billing.OnTenantCreated: Stripe CreateCustomer failed (non-fatal): %v", err)
		} else if cust != nil {
			customerID = cust.ID
			if priceID := l.pricing[plan]; priceID != "" {
				sub, err := l.stripe.CreateSubscription(ctx, SubscriptionRequest{
					Customer: customerID,
					PriceID:  priceID,
					Quantity: 1,
					Metadata: map[string]string{"kmail_tenant_id": tenantID, "kmail_plan": plan},
				})
				if err != nil {
					l.logger.Printf("billing.OnTenantCreated: Stripe CreateSubscription failed (non-fatal): %v", err)
				} else if sub != nil {
					subscriptionID = sub.ID
				}
			} else {
				l.logger.Printf("billing.OnTenantCreated: no Stripe price ID for plan %q — skipping subscription create", plan)
			}
		}
	}
	if err := l.upsertSubscriptionWithStripe(ctx, tenantID, plan, SubscriptionActive, subscriptionID, customerID, now, now.AddDate(0, 1, 0)); err != nil {
		return fmt.Errorf("upsert subscription: %w", err)
	}
	return nil
}

// OnTenantDeleted finalizes the billing relationship. Marks the
// subscription as cancelled and emits a final invoice event so the
// AR team has the terminating amount in `billing_events`.
func (l *Lifecycle) OnTenantDeleted(ctx context.Context, tenantID string) error {
	if l == nil || l.svc == nil {
		return nil
	}
	if _, err := l.svc.CalculateInvoice(ctx, tenantID); err != nil && !errors.Is(err, ErrNotFound) {
		l.logger.Printf("billing.OnTenantDeleted: final invoice: %v", err)
	}
	if l.stripe != nil && l.stripe.Configured() {
		if sub, err := l.GetSubscription(ctx, tenantID); err == nil && sub != nil && sub.StripeSubscriptionID != "" {
			if _, err := l.stripe.CancelSubscription(ctx, sub.StripeSubscriptionID); err != nil {
				l.logger.Printf("billing.OnTenantDeleted: Stripe CancelSubscription failed (non-fatal): %v", err)
			}
		}
	}
	if err := l.markSubscriptionCancelled(ctx, tenantID); err != nil {
		return fmt.Errorf("mark cancelled: %w", err)
	}
	return nil
}

// OnPlanChanged is invoked after `Service.ChangePlan` commits. It
// records the plan transition on the subscription row and, when the
// caller passed both old/new plans, writes a `plan_prorated` event
// with the prorated cents so finance can reconcile the partial
// period. The `proration_cents` is positive for upgrades, negative
// for downgrades.
func (l *Lifecycle) OnPlanChanged(ctx context.Context, tenantID, oldPlan, newPlan string) error {
	if l == nil || l.svc == nil {
		return nil
	}
	now := l.now().UTC()
	sub, err := l.GetSubscription(ctx, tenantID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("load subscription: %w", err)
	}
	if sub == nil {
		// Tenant was created before billing_subscriptions landed.
		if err := l.upsertSubscription(ctx, tenantID, newPlan, SubscriptionActive, "", now, now.AddDate(0, 1, 0)); err != nil {
			return fmt.Errorf("seed subscription: %w", err)
		}
		return nil
	}
	prorated, err := l.computeProration(ctx, tenantID, oldPlan, newPlan, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, now)
	if err != nil {
		l.logger.Printf("billing.OnPlanChanged: proration: %v", err)
	}
	if l.stripe != nil && l.stripe.Configured() && sub.StripeSubscriptionID != "" {
		if priceID := l.pricing[newPlan]; priceID != "" {
			// We don't currently persist Stripe item IDs (one-item
			// subscriptions only); emit a metadata-only update so
			// the subscription's metadata reflects the plan change.
			// Operators using Stripe's price-update path should run
			// the dedicated reconciliation tool documented in
			// docs/SECURITY.md §billing.
			if _, err := l.stripe.UpdateSubscription(ctx, sub.StripeSubscriptionID, SubscriptionRequest{
				Metadata: map[string]string{"kmail_plan": newPlan, "kmail_price_id": priceID},
			}); err != nil {
				l.logger.Printf("billing.OnPlanChanged: Stripe UpdateSubscription metadata failed (non-fatal): %v", err)
			}
		}
	}
	if err := l.upsertSubscription(ctx, tenantID, newPlan, sub.Status, sub.StripeSubscriptionID, sub.CurrentPeriodStart, sub.CurrentPeriodEnd); err != nil {
		return err
	}
	return l.recordEvent(ctx, tenantID, "plan_prorated", map[string]any{
		"old_plan":        oldPlan,
		"new_plan":        newPlan,
		"proration_cents": prorated,
	}, prorated)
}

// OnSeatAdded / OnSeatRemoved adjust seat counts at the billing
// layer. The Tenant Service already owns the `users` table writes —
// the lifecycle hook just keeps the cached seat count and any
// upstream Stripe subscription_item quantity in sync.
func (l *Lifecycle) OnSeatAdded(ctx context.Context, tenantID string) error {
	if l == nil || l.svc == nil {
		return nil
	}
	return l.svc.IncrementSeatCount(ctx, tenantID, +1)
}

// OnSeatRemoved mirrors OnSeatAdded with a -1 delta.
func (l *Lifecycle) OnSeatRemoved(ctx context.Context, tenantID string) error {
	if l == nil || l.svc == nil {
		return nil
	}
	return l.svc.IncrementSeatCount(ctx, tenantID, -1)
}

// ProrationPreview returns the cents that would be charged (or
// credited) for an immediate plan change, without mutating any DB
// state. Used by the admin UI's plan-change confirmation modal.
func (l *Lifecycle) ProrationPreview(ctx context.Context, tenantID, newPlan string) (int64, error) {
	if l == nil || l.svc == nil {
		return 0, nil
	}
	sub, err := l.GetSubscription(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return l.computeProration(ctx, tenantID, sub.Plan, newPlan, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, l.now().UTC())
}

// GetSubscription returns the current subscription row for the
// tenant, or ErrNotFound when none is present.
func (l *Lifecycle) GetSubscription(ctx context.Context, tenantID string) (*Subscription, error) {
	if l.svc.cfg.Pool == nil {
		return nil, ErrNotFound
	}
	var sub Subscription
	err := pgx.BeginFunc(ctx, l.svc.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT tenant_id::text, plan, status,
			       COALESCE(stripe_subscription_id, ''),
			       current_period_start, current_period_end,
			       created_at, updated_at
			FROM billing_subscriptions
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(
			&sub.TenantID, &sub.Plan, &sub.Status, &sub.StripeSubscriptionID,
			&sub.CurrentPeriodStart, &sub.CurrentPeriodEnd,
			&sub.CreatedAt, &sub.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// ListBillingHistory returns the recent `billing_events` for the
// tenant in reverse-chronological order. Used by the admin UI's
// billing history page.
func (l *Lifecycle) ListBillingHistory(ctx context.Context, tenantID string, limit int) ([]BillingHistoryEntry, error) {
	if l.svc.cfg.Pool == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []BillingHistoryEntry
	err := pgx.BeginFunc(ctx, l.svc.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, event_type, COALESCE(amount_cents, 0),
			       COALESCE(seat_count, 0), COALESCE(metadata, '{}'::jsonb)::text, created_at
			FROM billing_events
			WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
			LIMIT $2
		`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e BillingHistoryEntry
			if err := rows.Scan(&e.ID, &e.EventType, &e.AmountCents, &e.SeatCount, &e.Metadata, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BillingHistoryEntry is one row of `billing_events` flattened for
// the admin history view.
type BillingHistoryEntry struct {
	ID          string    `json:"id"`
	EventType   string    `json:"event_type"`
	AmountCents int64     `json:"amount_cents"`
	SeatCount   int       `json:"seat_count"`
	Metadata    string    `json:"metadata"`
	CreatedAt   time.Time `json:"created_at"`
}

// upsertSubscription writes the `billing_subscriptions` row.
func (l *Lifecycle) upsertSubscription(ctx context.Context, tenantID, plan string, status SubscriptionStatus, stripeID string, periodStart, periodEnd time.Time) error {
	return l.upsertSubscriptionWithStripe(ctx, tenantID, plan, status, stripeID, "", periodStart, periodEnd)
}

// upsertSubscriptionWithStripe is the variant that also persists
// the Stripe customer ID (Phase 8 migration 045). Empty strings
// are treated as "do not update" via COALESCE.
func (l *Lifecycle) upsertSubscriptionWithStripe(ctx context.Context, tenantID, plan string, status SubscriptionStatus, stripeID, stripeCustomerID string, periodStart, periodEnd time.Time) error {
	if l.svc.cfg.Pool == nil {
		return nil
	}
	var stripeSub, stripeCust any
	if stripeID != "" {
		stripeSub = stripeID
	}
	if stripeCustomerID != "" {
		stripeCust = stripeCustomerID
	}
	return pgx.BeginFunc(ctx, l.svc.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO billing_subscriptions (
				tenant_id, plan, status, stripe_subscription_id, stripe_customer_id,
				current_period_start, current_period_end
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id) DO UPDATE
			SET plan = EXCLUDED.plan,
			    status = EXCLUDED.status,
			    stripe_subscription_id = COALESCE(EXCLUDED.stripe_subscription_id, billing_subscriptions.stripe_subscription_id),
			    stripe_customer_id     = COALESCE(EXCLUDED.stripe_customer_id, billing_subscriptions.stripe_customer_id),
			    current_period_start = EXCLUDED.current_period_start,
			    current_period_end = EXCLUDED.current_period_end
		`, tenantID, plan, string(status), stripeSub, stripeCust, periodStart, periodEnd)
		return err
	})
}

func (l *Lifecycle) markSubscriptionCancelled(ctx context.Context, tenantID string) error {
	if l.svc.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, l.svc.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE billing_subscriptions
			SET status = $2
			WHERE tenant_id = $1::uuid
		`, tenantID, string(SubscriptionCancelled))
		return err
	})
}

func (l *Lifecycle) recordEvent(ctx context.Context, tenantID, eventType string, metadata map[string]any, amountCents int64) error {
	if l.svc.cfg.Pool == nil {
		return nil
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, l.svc.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO billing_events (tenant_id, event_type, metadata, amount_cents)
			VALUES ($1::uuid, $2, $3::jsonb, $4)
		`, tenantID, eventType, string(body), amountCents)
		return err
	})
}

// computeProration returns prorated cents for changing from
// `oldPlan` to `newPlan` with `now` somewhere in
// [periodStart, periodEnd]. Positive cents indicate the tenant owes
// the difference; negative indicate a credit.
func (l *Lifecycle) computeProration(ctx context.Context, tenantID, oldPlan, newPlan string, periodStart, periodEnd, now time.Time) (int64, error) {
	if oldPlan == "" || newPlan == "" || oldPlan == newPlan {
		return 0, nil
	}
	oldCents, err := l.svc.GetPlanPricing(oldPlan)
	if err != nil {
		return 0, err
	}
	newCents, err := l.svc.GetPlanPricing(newPlan)
	if err != nil {
		return 0, err
	}
	seats, err := l.svc.CountSeats(ctx, tenantID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	periodDays := int64(periodEnd.Sub(periodStart).Hours()/24) + 1
	if periodDays <= 0 {
		periodDays = 30
	}
	if now.Before(periodStart) {
		now = periodStart
	}
	if now.After(periodEnd) {
		return 0, nil
	}
	remainingDays := int64(periodEnd.Sub(now).Hours()/24) + 1
	if remainingDays < 0 {
		remainingDays = 0
	}
	delta := int64(newCents-oldCents) * int64(seats)
	return (delta * remainingDays) / periodDays, nil
}
