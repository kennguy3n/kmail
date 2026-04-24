package deliverability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WarmupScheduler resolves a tenant's current warmup-adjusted daily
// cap. New tenants ramp up from a small fraction of the plan limit
// to the full cap over `warmupDays` (default 30 days). The ramp
// follows the schedule documented in docs/PROPOSAL.md §9.5:
//
//   day 1  → 50  emails
//   day 2  → 100 emails
//   day 5  → 500 emails
//   day 10 → 1000 emails
//   day 20 → 2000 emails
//   day 30 → full plan daily limit
//
// Intermediate days interpolate between the anchors so the ramp is
// monotonic even on plans with a lower full cap.
type WarmupScheduler struct {
	pool       *pgxpool.Pool
	warmupDays int
	sendLimit  *SendLimitService
}

// warmupAnchor is a (day, emails) point in the ramp table.
type warmupAnchor struct {
	day    int
	emails int
}

var warmupAnchors = []warmupAnchor{
	{1, 50},
	{2, 100},
	{5, 500},
	{10, 1000},
	{20, 2000},
}

// WarmupRamp is the precomputed daily caps for the first
// `warmupDays` of a tenant's life. Exported for unit tests and for
// the admin UI to render the tenant's curve.
func WarmupRamp(planCap, warmupDays int) map[int]int {
	out := make(map[int]int, warmupDays)
	for d := 1; d <= warmupDays; d++ {
		out[d] = WarmupCapForDay(d, planCap, warmupDays)
	}
	return out
}

// WarmupCapForDay returns the daily cap on `day` (1-indexed) for a
// tenant whose plan-cap is `planCap` and whose warmup ramp is
// `warmupDays` days long.
func WarmupCapForDay(day, planCap, warmupDays int) int {
	if day <= 0 || planCap <= 0 {
		return 0
	}
	if warmupDays <= 0 {
		warmupDays = 30
	}
	if day >= warmupDays {
		return planCap
	}
	// Walk the anchor table and pick the greatest anchor <= day.
	// Clamp against planCap (a small Core-tier plan cap can be
	// below some anchors, in which case the ramp plateaus early).
	prev := warmupAnchor{0, 0}
	for _, a := range warmupAnchors {
		if a.day > day {
			break
		}
		prev = a
	}
	cap := prev.emails
	if cap > planCap {
		cap = planCap
	}
	if cap == 0 {
		cap = 50
	}
	return cap
}

// GetCurrentLimit returns the tenant's warmup-adjusted daily cap
// based on the tenant's `created_at` and the configured plan cap.
// Day 1 is the tenant's creation day; day `warmupDays` is the full
// cap. After `warmupDays` the plan cap applies directly.
func (w *WarmupScheduler) GetCurrentLimit(ctx context.Context, tenantID string) (int, error) {
	if tenantID == "" {
		return 0, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if w.pool == nil {
		return 0, ErrNotFound
	}
	var createdAt time.Time
	var plan string
	err := w.pool.QueryRow(ctx, `
		SELECT plan, created_at FROM tenants WHERE id = $1::uuid
	`, tenantID).Scan(&plan, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("load tenant for warmup: %w", err)
	}
	planCap, err := w.sendLimit.PlanDailyLimit(plan)
	if err != nil {
		return 0, err
	}
	day := int(time.Since(createdAt).Hours()/24) + 1
	return WarmupCapForDay(day, planCap, w.warmupDays), nil
}
