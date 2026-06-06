-- Reverting drops any rows using the new event type so the narrower
-- constraint can be re-applied.
DELETE FROM billing_events WHERE event_type = 'plan_prorated';

ALTER TABLE billing_events DROP CONSTRAINT billing_events_event_type_check;

ALTER TABLE billing_events ADD CONSTRAINT billing_events_event_type_check
    CHECK (event_type IN (
        'seat_added', 'seat_removed',
        'storage_delta', 'storage_snapshot',
        'plan_changed', 'invoice_generated',
        'limit_adjusted'
    ));
