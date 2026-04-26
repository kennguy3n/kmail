-- KMail — Phase 6: Webhook HMAC v2 signing scheme.
--
-- v1 (default) signs `t=<unix>,v1=<hex>` over `<unix>.<body>`.
-- v2 adds a replay-protection nonce and emits per-delivery
-- `X-KMail-Webhook-Nonce` and `X-KMail-Webhook-Timestamp`
-- headers. Endpoints opt into v2 per-row; existing endpoints stay
-- on v1 until the tenant flips them.

BEGIN;

ALTER TABLE webhook_endpoints
    ADD COLUMN signing_version TEXT NOT NULL DEFAULT 'v1'
        CHECK (signing_version IN ('v1', 'v2'));

COMMIT;
