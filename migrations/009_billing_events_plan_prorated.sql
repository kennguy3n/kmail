-- billing.Lifecycle.OnPlanChanged records a 'plan_prorated' event
-- (lifecycle.go) carrying the prorated charge/credit for a mid-period
-- plan change. The original billing_events CHECK constraint
-- (001_baseline.sql) never listed 'plan_prorated', so every proration
-- record INSERT failed with a check-constraint violation and the
-- proration history was silently lost. Widen the allowed set to match
-- the event types the application actually emits.

ALTER TABLE billing_events DROP CONSTRAINT billing_events_event_type_check;

ALTER TABLE billing_events ADD CONSTRAINT billing_events_event_type_check
    CHECK (event_type IN (
        'seat_added', 'seat_removed',
        'storage_delta', 'storage_snapshot',
        'plan_changed', 'plan_prorated', 'invoice_generated',
        'limit_adjusted'
    ));
