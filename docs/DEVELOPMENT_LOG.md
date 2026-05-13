# KMail — Development Log

Per-batch changelog of each task batch landing in KMail. For the
live status line and per-phase deliverable tables see
[`PROGRESS.md`](PROGRESS.md). The technical design and system
architecture live in [`PROPOSAL.md`](PROPOSAL.md) and
[`ARCHITECTURE.md`](ARCHITECTURE.md).

Entries are reverse-chronological (most recent batch first).

- **Last updated**: 2026-04-27 (Phase 8, batch 1) — Phase 8 GA-
  readiness ten-task batch flips Phase 8 from `PLANNED` to
  `IN PROGRESS`. Closes Phase 6 / 7 stubs, fills PROPOSAL.md
  roadmap gaps, and corrects long-standing documentation drift.
  Ships real KMIP TTLV wire traffic for HSM CMK envelope ops
  (`internal/cmk/{kmip,pkcs11}.go`, `EncryptDEK` / `DecryptDEK`,
  `last_used_at` column in migration 043, PKCS#11 path gated
  behind a `pkcs11` build tag); RFC 8030 Web Push transport with
  VAPID JWT signing (`internal/push/webpush.go`,
  `KMAIL_VAPID_PUBLIC_KEY` / `KMAIL_VAPID_PRIVATE_KEY` /
  `KMAIL_VAPID_SUBJECT`, browser registration helper at
  `web/src/api/push.ts`); TOTP fallback for WebAuthn
  (`internal/middleware/{totp,totp_store}.go`, RFC 6238 HMAC-SHA1,
  recovery codes, migration 044 `totp_credentials` with RLS, TOTP
  tab on `SecuritySettings.tsx`); ClamAV malware scanning adapter
  (`internal/malware/{scanner,clamav,handlers}.go`, INSTREAM over
  TCP, JMAP submit-time pre-delivery hook, `KMAIL_CLAMAV_ADDR`,
  optional `clamav` compose profile); free/busy publishing
  (`internal/calendarbridge/{freebusy,freebusy_handlers}.go`,
  `GET /.well-known/caldav` discovery, CalDAV `REPORT` with
  RFC 5545 VFREEBUSY, JSON `/api/v1/calendars/{accountID}/{calendarID}/freebusy`,
  "Check availability" UI in `EventCreate.tsx`); autoconfig /
  autodiscover XML endpoints
  (`internal/dns/{autoconfig,autoconfig_handlers}.go`, Thunderbird
  `mail/config-v1.1.xml`, Outlook `autodiscover.xml`,
  `.well-known/autoconfig/mail/config-v1.1.xml`, tenant-aware via
  the email's domain); Stripe subscription lifecycle wired on
  tenant signup / plan change / tenant delete
  (`internal/billing/{stripe,lifecycle}.go`,
  `KMAIL_STRIPE_SECRET_KEY` + `KMAIL_STRIPE_PRICE_*` plan→price
  map, `stripe_customer_id` / `stripe_subscription_id` columns in
  migration 045); shared-inbox MLS group key rotation
  (`internal/sharedinbox/mls.go` `MLSGroupManager` interface +
  `HTTPMLSGroupManager`, post-`AddSharedInboxMember` /
  `RemoveSharedInboxMember` rotation hook routed via
  `tenant.Service.WithSharedInboxMembershipHook`,
  `GET /api/v1/shared-inboxes/{inboxId}/mls/status`); Grafana
  dashboard provisioning
  (`deploy/grafana/dashboards/{kmail-overview,kmail-deliverability}.json`,
  `deploy/grafana/provisioning/dashboards.yml`, auto-loaded by
  `docker compose --profile loki up`, documented in
  `docs/DEVELOPMENT.md`); and a documentation reconciliation pass
  flipping Phase 4 to `COMPLETE`, Phase 7 to `COMPLETE`, and
  refreshing the README §Project Status block. New migrations
  043–045.

- **Last updated**: 2026-04-27 (Phase 7, batch 1) — Phase 7
  production-hardening ten-task batch flips Phase 7 from `PLANNED`
  to `IN PROGRESS`. Ships real APNs / FCM push transports with a
  TransportRouter that dispatches by `push_subscriptions.platform`
  (`internal/push/{apns,fcm,router,transport_test}.go`,
  `KMAIL_APNS_KEY_ID` / `KMAIL_APNS_TEAM_ID` /
  `KMAIL_APNS_KEY_PATH` / `KMAIL_FCM_CREDENTIALS_PATH`); a
  Stalwart v0 ↔ v1 compatibility shim
  (`internal/jmap/compat.go`, `scripts/stalwart-init-v1.sh`,
  `scripts/test-stalwart-upgrade.sh`, `docs/STALWART_UPGRADE.md`);
  per-tenant search backend abstraction
  (`internal/search/{service,meilisearch,opensearch,handlers}.go`,
  `migrations/039_search_backend.sql`, `SearchAdmin.tsx`); Loki +
  Promtail log shipping behind a compose profile
  (`internal/middleware/loki.go`, `deploy/loki/loki.yml`,
  `deploy/promtail/promtail.yml`, `deploy/grafana/datasources.yml`,
  `docker compose --profile loki up`,
  `docs/DEVELOPMENT.md` Loki section); DKIM key rotation
  automation (`internal/dns/{dkim_rotation,dkim_handlers}.go`,
  `migrations/040_dkim_keys.sql`, `DkimAdmin.tsx`); Helm chart for
  the kmail-api Deployment + Stalwart StatefulSet
  (`deploy/helm/kmail/{Chart,values,templates}.yaml`,
  `deploy/helm/README.md`, `make helm-lint`); Stripe REST client
  + dunning service + customer portal endpoint
  (`internal/billing/{stripe,dunning,portal_handler}.go`,
  `billing_dunning_events` table in migration 040, "Manage
  subscription" button on `PricingAdmin.tsx`); WebAuthn / FIDO2
  credential management (`internal/middleware/{webauthn,
  webauthn_store}.go`, `migrations/041_webauthn_credentials.sql`,
  `SecuritySettings.tsx`); per-tenant Sieve rule management
  (`internal/sieve/{service,handlers}.go`,
  `migrations/042_sieve_rules.sql`, `SieveAdmin.tsx`); load
  testing + chaos harness (`scripts/loadtest/load-jmap.go`,
  `load-smtp.sh`, `chaos-shard.sh`, `chaos-postgres.sh`,
  `chaos-valkey.sh`, `make loadtest` / `make chaos`,
  `docs/LOADTEST.md`). New migrations 039–042. Phase 6 status
  remains `IN PROGRESS` — the two deferred items (Exchange
  interop research, BIMI VMC) stay open per the do-not-do list.

- **Last updated**: 2026-04-26 (Phase 6, batch 1) — Phase 6
  enterprise-readiness ten-task batch flips Phase 6 from `PLANNED`
  to `IN PROGRESS`. Ships the SCIM 2.0 conformance harness
  (`scripts/test-scim.sh`, `make scim-test`,
  `docs/SCIM_CONFORMANCE.md` pass/fail matrix, plus discovery
  endpoints `ServiceProviderConfig` / `ResourceTypes` / `Schemas`
  in `internal/scim/discovery.go`); webhook HMAC v2 signing
  (timestamp + UUID nonce, `HMAC-SHA256(timestamp.nonce.body)`
  base64-encoded into `X-KMail-Signature: v2=<hex>` with
  `X-KMail-Webhook-Timestamp` and `X-KMail-Webhook-Nonce` headers,
  `signing_version` column on `webhook_endpoints`, admin selector
  + `updateWebhookSigningVersion` typed client); retention
  enforcement default flip (`KMAIL_RETENTION_DRY_RUN` defaults to
  `false`, four cumulative Prometheus counters, retention status
  card on `RetentionAdmin.tsx`, operator opt-out documented in
  `docs/DEVELOPMENT.md`); CardDAV Global Address List
  (`internal/contactbridge/gal.go` + `gal_entries` cache table,
  `GET /api/v1/contacts/gal` + `/search` + `/sync`, "Global
  Directory" tab in `ContactsView.tsx`, `getGlobalAddressList` /
  `searchGlobalAddressList` typed clients); onboarding auto-
  completion via webhook events (`internal/onboarding/auto_triggers.go`
  hooked into `webhooks.Service.AddListener`,
  `email.received` → "send_test_email", `domain.verified` →
  "verify_dns", `user.created` (count ≥ 2) → "invite_team",
  auto-completed badge on the checklist + `resetOnboardingChecklist`
  helper); admin-proxy session expiry watcher (`internal/adminproxy/
  expiry_worker.go` ticking every 60s, `expired_at` column on
  `admin_access_sessions`, `kmail_admin_sessions_expired_total`
  Prometheus counter); MLS group integration scaffolding
  (`internal/confidentialsend/mls.go` `MLSKeyDeriver` interface +
  `HTTPKeyDeriver`, `KChatMLSEndpoint` config, `mls/status` /
  `mls/wrap` / `mls/rekey` endpoints, graceful fallback when
  `KCHAT_MLS_ENDPOINT` is empty); BYOC HSM for CMK
  (`internal/cmk/hsm.go` `HSMKeyProvider` interface +
  `KMIPProvider` / `PKCS11Provider` stubs, `cmk_hsm_configs` table,
  `GET/POST /api/v1/tenants/{id}/cmk/hsm` + `/test`, HSM tab on
  `CmkAdmin.tsx`, privacy-plan gated, `registerHsmKey` /
  `listHsmConfigs` / `testHsmConnection` typed clients); ContactsView
  full CRUD + vCard 4.0 import / export endpoints
  (`POST /api/v1/contacts/{accountID}/{addressBookID}/import`,
  `GET .../export`), contact groups / labels via vCard
  `CATEGORIES`, photo URL field, delete confirmation modal;
  ScimAdmin / WebhookAdmin / OnboardingChecklist hardening
  (loading spinners, error toasts, empty states, token reveal-
  once UX, revocation confirmation modal, delivery health badge,
  test-fire button, signing-version selector, progress percentage
  bar, skip / reset confirmations). New migrations 034–038
  (`webhook_signing_v2`, `global_address_list`,
  `onboarding_auto_triggers`, `admin_session_expiry`, `cmk_hsm`).
  Phase 5 status text confirmed `COMPLETE` (no stale "IN PROGRESS"
  references for Phase 5 closeout work remain).

- **Last updated**: 2026-04-26 (Phase 5 closeout, batch 4) — Ten-task
  Phase 5 closeout PR lands the three remaining Phase 5 items
  (SCIM 2.0 provisioning endpoint at `/scim/v2/{Users,Groups}`
  backed by `internal/scim/`; reverse access proxy at
  `/api/v1/admin/proxy/{tenantId}/...` backed by
  `internal/adminproxy/` and gated by the existing approval
  workflow; compliance documentation pack under
  `docs/compliance/` with DPA, SOC 2 control mapping, Article 30
  records, sub-processor list, and customer-facing security
  overview), wires real JMAP/CalDAV/audit fan-out into
  `internal/export/runner.go` (`RealRunner` with `HTTPJMAPClient`,
  `CalendarClient`, `AuditClient`, and `Uploader` interfaces, with
  per-job tar.gz packaging and presigned upload via
  `jmap.AttachmentService`), turns the retention worker into a
  real enforcer (`internal/retention/worker.go` adds
  `EmailEnforcer` interface + `JMAPEnforcer` with
  `Email/query` + `Email/set` destroy fan-out and zk-object-fabric
  placement-API archive moves, dry-run guarded by
  `KMAIL_RETENTION_DRY_RUN`; `retention_enforcement_log` migration),
  routes calendar notifications per-resource via
  `internal/calendarbridge/channel_resolver.go` with the new
  `calendar_notification_channels` table and
  `GET/PUT /api/v1/calendars/{calendarId}/notification-channel` +
  tenant-default routes, generates BIMI TXT records in the DNS
  wizard (new `BIMILogoURL`/`BIMIVMCURL` config), adds a CardDAV
  contact bridge (`internal/contactbridge/`) with vCard 4.0 parser
  and `/api/v1/contacts/...` CRUD, tenant outbound webhooks
  (`internal/webhooks/` with HMAC-SHA256 signing, exponential-
  backoff retry worker, `webhook_endpoints` +
  `webhook_deliveries` migration, admin UI), and a guided
  onboarding checklist (`internal/onboarding/` with eight steps
  computed from existing tables, persistent skip flag in
  `onboarding_progress`, admin UI). New migrations 028–033
  (`scim_tokens`, `admin_access_sessions`,
  `retention_enforcement_log`, `calendar_notification_channels`,
  `webhooks`, `onboarding_progress`). Frontend adds
  `web/src/pages/Admin/{ScimAdmin,WebhookAdmin,OnboardingChecklist}.tsx`,
  `web/src/pages/Mail/ContactsView.tsx`, a
  `CalendarNotificationSettings` section to
  `ResourceCalendarAdmin.tsx`, a BIMI step to the DNS wizard, plus
  typed clients (`web/src/api/contacts.ts` and SCIM / webhook /
  onboarding / calendar-channel / admin-proxy helpers in
  `web/src/api/admin.ts`). Routes `/admin/scim`,
  `/admin/webhooks`, `/admin/onboarding`, `/contacts` wired in
  `App.tsx` + `Layout.tsx`.

- **Last updated**: 2026-04-25 (later, batch 3) — Phase 5 ten-task
  batch lands the Zero-Access Vault, Customer-managed keys,
  Protected folders, the Confidential Send portal, and the 99.95%
  availability hardening (multi-region SLO + graceful degradation
  middleware), plus a security-headers wrapper around the BFF and
  a 10-stage end-to-end smoke harness. Backend adds
  `internal/vault` (`service.go` + `protected.go` + handlers),
  `internal/cmk/service.go`, `internal/confidentialsend/service.go`,
  `internal/middleware/degradation.go`,
  `internal/middleware/security.go`,
  `internal/monitoring/multiregion.go` plus a 99.95%
  `HighAvailabilityTarget` constant, and four migrations
  (024 vault_folders, 025 customer_managed_keys,
  026 protected_folders, 027 confidential_send_links). HTTP
  surface adds `/api/v1/tenants/{id}/vault/folders[/{id}{,/encryption-meta}]`,
  `/api/v1/tenants/{id}/protected-folders[/{id}{,/share,/unshare,/access,/access-log}]`,
  `/api/v1/tenants/{id}/cmk[/active|/{id}/rotate|/{id}/revoke]`
  (plan-gated to privacy), `/api/v1/tenants/{id}/confidential-send`
  + the public `GET/POST /api/v1/secure/{token}` portal
  (rate-limited 5/15min via Valkey), and
  `/api/v1/admin/slo/regions` for the multi-region rollup.
  Frontend adds `VaultView`, `ProtectedFolderView`, `SecurePortal`
  under `web/src/pages/Mail/` and `CmkAdmin` under
  `web/src/pages/Admin/`; `Compose.tsx` gains expiry / password /
  max-views controls plus a copy-to-clipboard secure link when
  the privacy mode is "confidential-send". `SloAdmin` gains a
  region selector, the global rollup table, and a 99.95% target
  card. Typed clients `listVaultFolders` / `createVaultFolder` /
  `deleteVaultFolder` / `setVaultFolderEncryptionMeta`,
  `listCmkKeys` / `registerCmkKey` / `rotateCmkKey` /
  `revokeCmkKey` / `getActiveCmkKey`, `listProtectedFolders` /
  `createProtectedFolder` / `shareProtectedFolder` /
  `unshareProtectedFolder` / `getProtectedFolderAccessLog`,
  `getSloRegions` in `web/src/api/admin.ts`; new
  `web/src/api/confidentialSend.ts` exports `createSecureMessage`
  / `getSecureMessage` / `revokeSecureLink` / `listSecureMessages`.
  `scripts/test-e2e.sh` + `make e2e` exercise the 10 top user
  workflows (health, tenant CRUD, domain verification, JMAP
  session, JMAP query, calendar events, search, billing, audit,
  Confidential Send round-trip). Dependency: `golang.org/x/crypto`
  added for `bcrypt` password hashing on confidential-send links.

- **Last updated**: 2026-04-25 (later, batch 2) — Phase 4 / Phase
  5 ten-task batch wraps the remaining Phase 4 checklist items
  (Stalwart HA, per-tenant zk-object-fabric integration, calendar
  bridge, tenant-level billing, availability SLO) and starts four
  Phase 5 items (regional storage controls, retention, admin
  approval, eDiscovery export). Backend adds `internal/tenant/zkfabric.go`,
  `internal/billing/lifecycle.go`, `internal/billing/webhook.go`,
  `internal/calendarbridge/notifications.go` /
  `reminder_worker.go`, `internal/jmap/proxy.go` shard-aware
  failover, `internal/monitoring/slo.go`, `internal/tenant/placement.go`,
  `internal/retention`, `internal/approval`, `internal/export` and
  six new migrations (018–023). Operator templates land at
  `deploy/stalwart/ha-config.json` + `deploy/stalwart/README.md`.
  Frontend adds `PricingPage`, `SloAdmin`, `StoragePlacementAdmin`,
  `RetentionAdmin`, `ApprovalAdmin`, `ExportAdmin` plus
  `web/src/api/billing.ts` and Phase 4/5 typed clients in
  `web/src/api/admin.ts`. See PR for the full diff.

  Earlier 2026-04-25 — Migration wizard
  "Test connection" flow + Pricing & plan-management page round
  out the Phase 4 batch landed earlier today. Backend adds
  `migration.Service.TestConnection` (drives a real IMAP LOGIN
  with a 10 s deadline, supports implicit-TLS on 993 and plain
  TCP otherwise) and `POST /api/v1/migrations/test-connection`;
  `billing.Service.ChangePlan` validates the plan, updates
  `tenants.plan`, syncs `quotas.storage_limit_bytes` to the new
  per-seat default (preserving operator overrides), re-runs
  `EnforcePlanLimits`, and writes a `plan_changed` row to
  `billing_events`; `PATCH /api/v1/tenants/{id}/billing/plan`
  surfaces it to the admin console. Frontend adds a "Test
  connection" button to `MigrationAdmin.tsx` step 2 (success /
  failure inline) and a new `PricingAdmin.tsx` page with a
  three-column plan matrix ($3 / $6 / $9 per seat — Core /
  Pro / Privacy), current-plan highlight, seat × price monthly
  total, and upgrade / downgrade buttons. Typed client helpers
  `testMigrationConnection` and `changePlan` plus a static
  `PLAN_CATALOG` in `web/src/api/admin.ts`. Route
  `/admin/pricing` registered in `App.tsx`; nav link in
  `Layout.tsx`. Unit tests cover plan validation, IMAP
  LOGIN success / NO rejection / dial failure, and the
  IMAP-quote helper.
- **Last updated**: 2026-04-25 — Phase 3 / Phase 4 ten-task batch
  landed. Multi-tenant Stalwart shard routing, the DNS onboarding
  wizard, Gmail Postmaster + Yahoo ARF feedback-loop ingestion,
  abuse scoring, mobile push notifications, resource + shared-team
  calendars, the IP reputation dashboard, automated deliverability
  alerts, shared-mailbox workflows, and the migration wizard UI
  are all live in `main`. New Go packages `internal/push`,
  `internal/sharedinbox`, new `tenant.ShardService` /
  `tenant.HealthWorker`, and the deliverability sub-services
  `FeedbackLoopService`, `AbuseScorer`, and `AlertService` /
  `AlertEvaluator` land together. Migrations 011–017 add
  `feedback_loop_events`, `abuse_alerts` / `abuse_scores`,
  `push_subscriptions` / `notification_preferences`,
  `calendar_shares` / `resource_calendars`, `deliverability_alerts`
  / `alert_thresholds`, `shared_inbox_assignments` /
  `shared_inbox_notes`, and `stalwart_shards` /
  `tenant_shard_assignments`. Seven new React pages
  (`DnsWizard`, `IpReputationAdmin`, `NotificationPrefs`,
  `MigrationAdmin`, `ResourceCalendarAdmin`, `SharedCalendars`,
  `SharedInboxView`) wire into `App.tsx` / `Layout.tsx`. Earlier
  Phase 3 work (Billing / Quota Service, the Deliverability
  Control Plane — suppression, bounces, IP pools, send limits,
  warmup, DMARC — attachment-to-link conversion, shared-inbox
  seat exemption, Observability, and three admin console pages)
  remains live. Specifically:
  * Billing / Quota Service — `internal/billing/` Service with
    GetQuota / UpdateStorageUsage / CountSeats / EnforcePlanLimits
    / GetPlanPricing / CalculateInvoice; `billing_events` table
    (`migrations/005_billing.sql`) with RLS; handlers under
    `/api/v1/tenants/{id}/billing[/usage|/invoice]` + PATCH for
    admin limit overrides; per-seat pricing
    ($3 / $6 / $9 for core / pro / privacy); unit tests for plan
    pricing, quota enforcement, seat counting, and invoice math.
  * Pooled storage quotas — `internal/billing/quota_worker.go`
    background goroutine that polls the zk-object-fabric S3 API
    (`StorageScanner` interface, `StaticScanner` for CI) every
    `QuotaWorkerInterval` (default 5m) and rewrites
    `quotas.storage_used_bytes`. Quota is pooled at the tenant
    level; plan-based per-seat limits (5 / 15 / 50 GB) resolve
    into the tenant's `storage_limit_bytes` via
    `EnforcePlanLimits`. `internal/tenant/service.go` now enforces
    the seat counter on `CreateUser` / `DeleteUser` via a narrow
    `SeatAccounter` interface to avoid circular imports.
  * Suppression lists and bounce tracking —
    `internal/deliverability/suppression.go` and `bounce.go` own
    `suppression_list` + `bounce_events`
    (`migrations/006_suppression.sql`) with RLS. Hard bounces and
    complaints escalate to suppression immediately; soft bounces
    escalate after 3 within 72 h. `CheckRecipient` is the
    pre-send hook wired into the JMAP proxy path.
  * IP pool architecture — `internal/deliverability/ippool.go` +
    `migrations/007_ip_pools.sql` give us the five canonical pools
    (system_transactional, mature_trusted, new_warming,
    restricted, dedicated_enterprise), per-IP reputation +
    daily_volume + status, and a `SelectSendingIP` ranker that
    picks the best active IP from the tenant's highest-priority
    pool assignment. Admin HTTP surface under
    `/api/v1/admin/ip-pools[/{id}/ips]` and tenant-scoped
    `/api/v1/tenants/{id}/ip-pool`.
  * Tenant send limits + warmup — `sendlimit.go` + `warmup.go`
    provide daily / hourly cap enforcement (keyed in Valkey with
    TTL) and a 30-day warmup ramp anchored at 50 / 100 / 500 /
    1000 / 2000 / full on days 1 / 2 / 5 / 10 / 20 / 30.
    `CheckSendLimit` is wired into the JMAP proxy path; default
    plans are 500 / 2000 / 5000 per day with hourly = daily / 10.
  * DMARC report ingestion — `dmarc.go` + sample-backed unit
    tests parse RFC 7489 aggregate XML into `dmarc_reports`
    (`migrations/008_dmarc_reports.sql`) and expose list /
    summary / upload endpoints plus a per-domain 30-day pass-rate
    roll-up at `/api/v1/tenants/{id}/dmarc-reports/summary`.
  * Attachment-to-link conversion — `internal/jmap/attachment.go`
    implements a minimal SigV4 presigner (no aws-sdk-go-v2
    dependency) against the zk-object-fabric S3 endpoint,
    `internal/jmap/attachment_handlers.go` exposes
    `POST /api/v1/attachments/upload`,
    `GET /api/v1/attachments/{id}/link`, and
    `DELETE /api/v1/attachments/{id}`. Frontend
    `web/src/pages/Mail/Compose.tsx` detects files over 10 MB and
    routes them through the new endpoint, appending a presigned
    download link to the body. Metadata persists in
    `attachment_links` (`migrations/009_attachment_links.sql`)
    with a `revoked` flag for link revocation.
  * Shared inboxes without paid seats — `users.account_type`
    column (already present) is now enforced end-to-end:
    `billing.CountSeats` filters on
    `status = 'active' AND account_type = 'user'`, the Tenant
    Service rejects invalid account types, and the seat counter
    only increments for `user` rows. Shared inboxes and service
    accounts no longer consume billable seats.
  * Observability — `internal/middleware/metrics.go` registers
    the Prometheus collectors (`kmail_http_requests_total`,
    `kmail_http_request_duration_seconds`,
    `kmail_jmap_proxy_duration_seconds`, `kmail_active_tenants`,
    `kmail_seats_total{plan=...}`) and exposes `/metrics`
    unauthenticated; `tracing.go` initialises the
    OTLP/HTTP exporter against `OTEL_EXPORTER_OTLP_ENDPOINT` and
    registers the W3C `traceparent` propagator; `logger.go`
    emits structured JSON lines (with `tenant_id`, `user_id`,
    `trace_id`) when `KMAIL_LOG_FORMAT=json`. A new
    `prometheus` service in `docker-compose.yml` scrapes the BFF
    via `deploy/prometheus/prometheus.yml`.
  * Admin console completion — new `web/src/pages/Admin/`
    pages: `QuotaAdmin.tsx` (usage progress bars, seat + storage
    counters, per-seat price, monthly total, PATCH form),
    `AuditAdmin.tsx` (filterable audit-log table, JSON/CSV
    export, hash-chain verify), `DmarcAdmin.tsx` (per-domain
    pass-rate summary, per-report drill-down, manual XML
    upload). `web/src/api/admin.ts` gains the billing + DMARC
    helpers; `web/src/App.tsx` mounts `/admin/billing`,
    `/admin/audit`, `/admin/dmarc`; `web/src/components/Layout.tsx`
    gains the new nav links.

- **Previously (2026-04-24)** — Phase 2 remainder + early
  Phase 3 batch landed. Three more Phase 2 items (BFF auth
  hardening, email-to-chat bridge, benchmark harness) and two
  Phase 3 items (admin audit logs, admin console backend) are
  now live. Specifically:
  * OIDC JWT signature verification — `internal/middleware/auth.go`
    now verifies against the issuer's JWKS (in-process cached via
    `internal/middleware/jwks.go` with a configurable refresh),
    checks `iss` / `aud` / `exp` via
    `github.com/golang-jwt/jwt/v5`, and honours the new
    `KChatOIDCAudience` / `KCHAT_OIDC_AUDIENCE` config. Dev-bypass
    path kept intact so local flows are unaffected.
  * Valkey-backed rate limiting — `internal/middleware/ratelimit.go`
    keys a fixed-window counter per-tenant
    (`tenant:{id}:rpm`) and per-user (`user:{tid}:{uid}:rpm`),
    returns HTTP 429 with `Retry-After`, wired between OIDC and
    the JMAP proxy in `cmd/kmail-api/main.go`. Gated by
    `KMAIL_RATELIMIT_ENABLED` so local dev is not throttled.
  * CalDAV Go bridge — `internal/calendarbridge/` ListCalendars /
    GetEvents / CreateEvent / UpdateEvent / DeleteEvent /
    RespondToEvent over Stalwart's CalDAV surface, HTTP routes
    under `/api/v1/calendars/...`, minimal iCalendar parser for
    UID / SUMMARY / DTSTART / DTEND + PARTSTAT rewriter, unit
    tests against a fake Stalwart CalDAV server.
  * Email-to-chat bridge — `internal/chatbridge/` Service with
    ShareEmailToChannel, ConfigureAlertRoute, ListRoutes,
    DeleteRoute, ProcessInboundAlert; `chat_bridge_routes` table
    (`migrations/003_chat_bridge_routes.sql`) with RLS and a
    unique `(tenant_id, alias_address)`; HTTP surface under
    `/api/v1/chat-bridge/...`; `cmd/kmail-chat-bridge` boots a
    real listener.
  * Audit log service — `internal/audit/` Service with hash-
    chained rows, `audit_log` table
    (`migrations/004_audit_log.sql`) with RLS and
    `(tenant_id, created_at DESC)` index; paginated Query /
    JSON+CSV Export / VerifyChain walker; HTTP routes under
    `/api/v1/tenants/{id}/audit-log[/export|/verify]`;
    `cmd/kmail-audit` CLI exposes `serve | verify | export`.
  * Migration orchestrator Pause / Resume — `PauseJob` signals
    the in-flight worker's cancel func and flips the row to
    `paused`; `ResumeJob` runs through the existing `StartJob`
    path so imapsync picks up from its `--tmpdir` checkpoint.
    HTTP: `POST /api/v1/migrations/{jobId}/pause|resume`.
  * Admin console audit-log client — extends
    `web/src/api/admin.ts` (which was stood up in the earlier
    admin-UI batch below) with `AuditLogEntry` / `AuditLogQuery`
    types and `getAuditLog` / `exportAuditLog` /
    `verifyAuditChain` methods that front the new
    `/api/v1/tenants/{id}/audit-log` Go routes so admin pages
    can render and export the hash-chained log.
  * Benchmark harness — `scripts/bench/bench-jmap.go` (Mailbox /
    Email query / Email get P50/P95/P99, warm-up + concurrency),
    `bench-smtp.sh` (swaks DATA→250 OK), `bench-caldav.sh`
    (CalDAV PUT), `seed-data.sh`, `make bench` Makefile target,
    `docs/BENCHMARKS.md` with targets and baseline.
  * Spam config snapshot — `configs/stalwart/spam-config.json`
    pins the declarative shape of every `spam-filter.*` key the
    init script pushes, plus the Sieve Junk rule, so operators
    can diff the running config against source.
  * `docs/DEVELOPMENT.md` gains §5a (Thunderbird / Apple
    Mail / Calendar client setup, port matrix, Stalwart v0.16.0
    limitations) and §5b (spam filter scoring / DNSBL /
    Bayesian auto-learn / GTUBE smoke test).

- **Previously (2026-04-24 earlier)**: Admin UI Phase 2 batch landed.
  The React admin pages stop being placeholders and start driving
  the existing Tenant Service + DNS Onboarding REST endpoints.
  Specifically:
  * `web/src/api/admin.ts` is a new typed REST client for the
    control-plane surface (`/api/v1/tenants/...`). It mirrors the
    `authHeaders()` pattern from `web/src/api/jmap.ts` — bearer
    token `kmail-dev` plus an optional `X-KMail-Dev-Tenant-Id`
    header so the dev-bypass middleware (`devClaimsFromHeaders`
    in `internal/middleware/auth.go`) resolves the same tenant ID
    the URL path carries, which satisfies `checkTenantScope` on
    the server side. Exposes typed `listTenants`, `listDomains`,
    `verifyDomain`, `getDomainRecords`, `listUsers`, `updateUser`,
    `deleteUser`, and an `AdminApiError` class.
  * `web/src/pages/Admin/useTenantSelection.ts` is a shared hook
    that loads the tenant list, tracks the selected tenant in
    `localStorage`, and is consumed by both admin pages so they
    agree on which tenant is being managed.
  * `web/src/pages/Admin/DomainAdmin.tsx` now lists the selected
    tenant's domains with the four persisted per-check flags
    (MX / SPF / DKIM / DMARC) plus an aggregate verified column,
    a **Verify** button that fires `POST .../domains/{id}/verify`
    and refreshes the row, and a **Show DNS records** expander
    that fetches `GET .../domains/{id}/dns-records` and renders
    the MX / SPF / DKIM / DMARC / MTA-STS / TLS-RPT record rows
    the tenant needs to publish.
  * `web/src/pages/Admin/UserAdmin.tsx` now lists the selected
    tenant's users (email, display name, role, status, quota),
    supports inline **Edit** that PATCHes only the changed fields
    through `PATCH .../users/{userId}`, and gates **Delete**
    behind a confirm button that fires `DELETE .../users/{userId}`
    and removes the row on success.
- **Previously (2026-04-24 earlier)**: Phase 2 compatibility + spam +
  migration batch landed. Four Phase 2 checklist items graduate
  off the "planned" list: basic spam / phishing filtering via
  Stalwart, IMAP / SMTP compatibility testing, CalDAV
  compatibility testing, and the Gmail / IMAP migration
  orchestrator. Specifically:
  * `scripts/stalwart-init.sh` now drives Stalwart v0.16.0's
    built-in spam filter through the JMAP admin registry —
    toggles `spam-filter.enable`, pins spam / discard / reject
    score thresholds (5.0 / 10.0 / 15.0), wires the Bayesian
    classifier with JMAP `$junk` / `$notjunk` auto-learning,
    enables a representative DNSBL set (Spamhaus Zen / SpamCop /
    Spamhaus DBL / SURBL), and installs a Sieve script that
    files anything tagged `X-Spam-Status: Yes` into the
    per-principal Junk mailbox.
  * `web/src/api/jmap.ts` gains `markAsSpam(emailId, fromMailbox,
    junkMailbox, isSpam)` — an atomic JMAP `Email/set` patch that
    moves the message between Inbox and Junk and flips the
    `$junk` / `$notjunk` keywords in the same round-trip so the
    server-side classifier learns from user feedback.
  * `web/src/pages/Mail/Inbox.tsx` resolves the Junk mailbox by
    role, shows a ⚠ icon + amber styling next to it in the
    sidebar, adds a row-level `Spam` / `Not spam` button whose
    label flips depending on whether the message already lives
    in Junk, and paints a `SPAM` badge + amber background on
    rows currently filed as junk.
  * `scripts/test-imap-smtp.sh` asserts that SMTP :587 announces
    STARTTLS + AUTH, SMTP :465 completes an implicit TLS
    handshake, IMAP :143 accepts STARTTLS + `LOGIN` + `LIST`, and
    a `curl`-submitted RFC 5322 message round-trips through the
    recipient's INBOX within 10 s.
  * `scripts/test-caldav.sh` asserts that `OPTIONS
    /dav/calendars/` announces the `calendar-access` compliance
    class, `PROPFIND Depth:0/1` returns multistatus + at least
    one calendar collection, and a minimal VEVENT survives a
    PUT → GET → DELETE round-trip (the script re-reads the
    `SUMMARY` field to confirm payload fidelity).
  * `docs/COMPATIBILITY.md` is a new doc covering the third-
    party client contract: port matrix (25 / 465 / 587 / 143 /
    993 / 8080), Thunderbird + Apple Mail (IMAP / SMTP /
    CalDAV) manual setup, known limitations of Stalwart v0.16.0
    (XOAUTH2 / CONDSTORE / SORT / BURL / DSN), and a manual
    checklist for Mail + Spam / Junk + Calendar.
  * `internal/migration/` goes from a two-line placeholder to a
    full orchestrator: `Service` + `Config` + `MigrationJob`,
    `CreateJob` / `StartJob` / `GetJob` / `ListJobs` /
    `CancelJob`, an in-process worker goroutine pool capped by
    `MaxConcurrent` that shells out to `imapsync`, parses its
    `Messages N of M done` progress lines, and writes
    `progress_pct` / `messages_synced` / `messages_total`
    checkpoints back to Postgres; state transitions (`pending →
    running → completed|failed|cancelled`) and the worker
    cancel-func map are covered by unit tests, and the HTTP
    surface (`POST / GET / GET /{jobId} / DELETE /{jobId}`
    under `/api/v1/migrations`) is mounted alongside the tenant
    and DNS handlers in `cmd/kmail-api/main.go`, all tenant-
    scoped via OIDC + RLS.
  * `migrations/002_migration_jobs.sql` adds the `migration_jobs`
    table (tenant_id FK, `pending|running|paused|cancelled|failed|completed`
    status enum, progress / message counters, started / completed
    timestamps, encrypted source password, `kmail_set_updated_at`
    trigger, tenant + status indexes, and a tenant-isolating
    RLS policy against `app.tenant_id`).
- **Previously (2026-04-24)**: Phase 2 Mail + Calendar UI batch
  landed. The React Mail UI now has full-text search: a new
  `searchEmails(query, opts)` method on `web/src/api/jmap.ts`
  builds a JMAP `Email/query` with an RFC 8621 §4.4.1 `text`
  FilterCondition (wrapped in an `AND` against `inMailbox` when a
  mailbox is selected), hydrates results through a back-referenced
  `Email/get`, and powers a new search bar in
  `web/src/pages/Mail/Inbox.tsx` that submits on Enter, toggles
  between per-mailbox and "All mailboxes" scope via a checkbox,
  shows hit count + scope in the status line, and exposes a Clear
  button that reverts to the normal mailbox view. The React
  Calendar UI also ships: `web/src/types/index.ts` now exports
  `Calendar`, `CalendarEvent`, `CalendarEventDraft`,
  `EventParticipant`, `EventParticipantResponse`, `RecurrenceRule`,
  `EventDateRange`, and `SearchEmailsOptions`, plus a
  `JMAP_CALENDARS_CAPABILITY =
  "urn:ietf:params:jmap:calendars"` constant (Stalwart v0.16.0
  ships CalDAV but does not yet advertise the draft JMAP calendars
  capability, so the Go BFF surfaces JMAP on top of the CalDAV
  store — the React client only talks JMAP). `web/src/api/jmap.ts`
  gains `getCalendarAccountId()` (falls back to the Mail account
  when no separate Calendar account exists), a `calendarRequest()`
  private helper that scopes method calls with the Calendars
  capability, and typed `getCalendars()` / `getEvents()` /
  `getEvent()` / `createEvent()` / `updateEvent()` /
  `deleteEvent()` / `respondToEvent()` methods.
  `web/src/pages/Calendar/CalendarView.tsx` renders a Day / Week /
  Month toggle, a 24-hour time grid for day+week views, a 6x7
  month grid, a sidebar with per-calendar visibility checkboxes,
  an event detail panel with RSVP (Accept / Tentative / Decline)
  and Edit / Delete actions, and opens `/calendar/new` with
  `?start=&end=` pre-filled when an empty slot is clicked.
  `web/src/pages/Calendar/EventCreate.tsx` is a full create/edit
  form (Calendar picker, title, start/end `datetime-local`,
  location, participant list, RSVP-required toggle, status,
  description) driving `createEvent()` in create mode and
  `updateEvent()` in edit mode. `web/src/App.tsx` now routes
  `/calendar/:eventId` through `CalendarView` (deep link to the
  event detail panel via `useParams` + `getEvent`) and
  `/calendar/:eventId/edit` through `EventCreate`. No backend
  changes — everything in this batch is frontend-only and speaks
  the existing JMAP contract.
- **Previously (2026-04-24)**: zk-object-fabric blob store is
  verified end-to-end through Stalwart, and the
  `docker compose up` path is fully hands-off again.
  `scripts/stalwart-init.sh` has been rewritten from the
  legacy REST `/api/settings*` surface (which Stalwart v0.16.0
  dropped) onto the JMAP admin registry — it POSTs
  `x:BlobStore/set` (zk-fabric via the `S3StoreRegion::Custom`
  endpoint/region pair), `x:InMemoryStore/set` (Valkey via the
  Redis URL), `x:SearchStore/set` (Meilisearch via a Bearer
  master key), and `x:Domain/set` (the dev tenant domain) with
  Basic auth against `/jmap`. Stalwart v0.16.0 auto-creates
  `Default` (Postgres-backed) singletons on first boot and only
  resolves the concrete backends at startup, so the script now
  also mounts `/var/run/docker.sock` and issues
  `POST /containers/kmail-stalwart/restart` against the Docker
  Engine API once the /set calls return — a one-time first-boot
  restart that swaps the live pointer over to zk-object-fabric.
  Verified from a fresh volume (`docker compose down -v` +
  `docker compose up`): the init container completes with
  `BlobStore configured` / `InMemoryStore configured` /
  `SearchStore configured` / `domain kmail.dev created`, Stalwart
  restarts, and a JMAP blob upload
  (`POST /jmap/upload/d333333`) lands in `s3://kmail-blobs/`
  visible via `aws s3api list-objects-v2 --bucket kmail-blobs`
  on the host. Downloading the same blob via
  `GET /jmap/download/d333333/{blobId}/...` returns the original
  bytes — upload and download both flow through zk-fabric. As
  part of the rewrite, `KMAIL_DEV_TENANT_DOMAIN` moved from
  `kmail.local` to `kmail.dev`: Stalwart v0.16.0's domain
  validator rejects the `.local` / `.test` /
  `localhost.localdomain` RFC 2606 mDNS suffixes, and `.dev` is
  a real TLD that passes validation without surprising the
  mail-server's hostname checks. `docker-compose.yml` now
  reflects the new domain and the socket mount.
- **Previously (2026-04-24 earlier)**: zk-object-fabric blob
  store smoke test partially verified against the local compose
  stack. Brought the full stack up (`docker compose up`);
  `zk-fabric`, `postgres`, `valkey`, `meilisearch`, and
  `stalwart` all come up healthy and the one-shot
  `zk-fabric-init` creates the `kmail-blobs` bucket as expected.
  Verified from the host with the dev `kmail-access-key`
  credentials that the gateway accepts S3 `PutObject` /
  `ListObjectsV2` / `HeadObject` / `DeleteObject` against
  `s3://kmail-blobs/` — i.e. the blob path Stalwart is pointed
  at is a working S3 endpoint. Did *not* exercise a round-trip
  through Stalwart itself because `scripts/stalwart-init.sh`
  targeted the legacy REST `/api/settings*` surface that
  Stalwart v0.16.0 dropped; the JMAP rewrite above closes that
  gap.
- **Previously (2026-04-23)**: Third Phase 2 batch landed. Mail
  UI is now end-to-end functional against the JMAP client:
  `web/src/pages/Mail/Compose.tsx` is a fully working composer
  (To / Cc / Bcc / Subject / Body, From-identity selector, privacy
  mode selector, Reply / Reply-All / Forward pre-fill via router
  state, Send + Save draft + Cancel) that drives
  `jmapClient.sendEmail` (batches `Email/set create` +
  `EmailSubmission/set`) and `jmapClient.createDraft`;
  `web/src/pages/Mail/Inbox.tsx` now supports per-row Mark
  read/unread and Move to trash / Delete actions; and
  `web/src/pages/Mail/MessageView.tsx` marks messages as read on
  open, renders the JMAP `attachments` list, and ships
  Reply / Reply-All / Forward buttons that navigate into Compose
  with the quoted body pre-filled. Under the hood,
  `web/src/api/jmap.ts` centralises the dev-bypass bearer token
  (`Authorization: Bearer kmail-dev`) through an `authHeaders()`
  helper on every `fetch`, adds `markRead(emailId, read)` (JMAP
  `keywords/$seen` patch-path) and `createDraft(draft)` helpers,
  factors the shared draft-payload construction into a
  `buildEmailCreate()` so `sendEmail` and `createDraft` cannot
  drift, and asks `Email/get` for `attachments` alongside the
  existing body properties. The previous Phase 2 batch (below)
  remains accurate for the three pieces it landed.
- **Previously (2026-04-23 earlier)**: Second Phase 2 batch landed.
  This update finishes the Tenant Service CRUD surface with
  shared-inbox membership (`ListSharedInboxes`,
  `AddSharedInboxMember`, `RemoveSharedInboxMember` in
  `internal/tenant/service.go` and matching `/shared-inboxes` and
  `/shared-inboxes/{inboxId}/members` routes), adds `PATCH` verbs
  alongside `PUT` for the tenant and user update endpoints, lifts
  the DNS wizard HTTP surface into its own package
  (`internal/dns/handlers.go`, `dns.NewHandlers(...)`,
  `POST /api/v1/tenants/{id}/domains/{domainId}/verify` +
  `GET .../dns-records`) so it can evolve independently of tenant
  CRUD, introduces `dns.GetExpectedRecords` /
  `dns.LookupDomainName` for the new records endpoint (RLS-scoped
  domain lookup; no more routing through the tenant service for a
  single field), deletes the duplicated DNS handler code that used
  to live in `cmd/kmail-dns` and `internal/tenant/handlers.go`,
  and adds input-validation unit tests for every new method. On
  the frontend, `web/src/api/jmap.ts` now has a real `JMAPClient`
  class (session fetch + caching, `request(methodCalls)` with
  Mail + Submission capability, typed `getMailboxes` /
  `getEmails` / `getEmail` / `sendEmail` / `moveEmail` /
  `deleteEmail`), `web/src/types/index.ts` exports RFC 8621–
  shaped `Mailbox` / `Email` / `EmailAddress` / `EmailBodyPart`
  types, and the Inbox + MessageView pages in `web/src/pages/Mail/`
  render a mailbox sidebar, an email list (sender / subject / date
  with unread styling), and a single-message reading pane against
  that client. The previous Phase 2 batch (below) remains
  accurate for the three pieces it landed.
- **Previously (2026-04-23 earlier)**: Phase 2 engineering work
  kicked off. That update landed three pieces of the Phase 2
  checklist:
  (1) the full Tenant Service CRUD surface in
  `internal/tenant/service.go` (`ListTenants`, `UpdateTenant`,
  `DeleteTenant`, `ListUsers`, `GetUser`, `UpdateUser`,
  `DeleteUser`, `GetDomain`) backed by the `app.tenant_id` GUC
  for RLS-scoped calls, with the matching `GET` / `PUT` /
  `DELETE` routes registered under `/api/v1/tenants/...` in
  `internal/tenant/handlers.go` and validation unit tests in
  `service_test.go`; (2) the DNS Onboarding Service in
  `internal/dns/dns.go` — a `Resolver` interface makes MX / SPF /
  DKIM / DMARC lookups mockable, `VerifyDomain` runs all four
  checks inside an RLS-scoped pgx transaction and writes the
  resulting flags to `domains`, and `GenerateRecords` returns the
  MX / SPF / DKIM / DMARC / MTA-STS / TLS-RPT / autoconfig /
  autodiscover records a tenant must publish; the service is
  mounted in-process by `cmd/kmail-api` under
  `POST /api/v1/tenants/{id}/domains/{domainId}/verify` and
  `GET .../records`, and `cmd/kmail-dns` now has a working
  standalone HTTP entrypoint for deployments that want to scale
  the DNS service independently; unit tests cover the DNS logic
  with an in-memory fake resolver; (3) the Stalwart v0.16.0
  automated bootstrap — `configs/stalwart-bootstrap.json` is the
  minimal JSON config that points Stalwart at Postgres and sets
  the admin password from `STALWART_ADMIN_PASSWORD`,
  `scripts/stalwart-init.sh` configures blob store →
  zk-object-fabric (MinIO locally), search → Meilisearch,
  in-memory → Valkey, SMTP / IMAP / JMAP listeners, and the
  `kmail-dev` tenant through the admin API, and
  `docker-compose.yml` mounts the JSON bootstrap as
  `/etc/stalwart/config.json` and adds a `stalwart-init` one-shot
  service so `docker compose up` is now hands-off. The earlier
  `configs/stalwart.toml` is retained as a reference cheat-sheet
  with a clear deprecation header. Phase 1 remains `IN PROGRESS`
  because the decision gate still requires external
  confirmations — see the decision gate section below. Those are
  process gates, not code gates; no additional KMail code changes
  are required to close them out.
- **Previously (2026-04-23 earlier)**: All eleven Phase 1
  checklist items below were delivered in code and docs: the Go
  module layout, Stalwart docker-compose wiring, schema
  migrations, JMAP contract doc, `cmd/kmail-api` BFF binary with
  health / readiness / graceful shutdown / `/jmap` reverse
  proxy / `/api/v1/tenants` CRUD, the `internal/config` loader,
  the `internal/middleware` OIDC stub with dev-bypass token and
  the `app.tenant_id` GUC helper, and the initial
  `internal/tenant` service+handlers backed by RLS. The GitHub
  Actions CI workflow at `.github/workflows/ci.yml` runs
  Go 1.25 `make vet / build / test` (with `-race`) on push and
  pull-request.

This document is a phase-gated tracker. Each phase has an explicit
checklist and a decision gate. Do not skip to the next phase until
the current phase's gate has been met.

For the technical design, see [PROPOSAL.md](PROPOSAL.md). For the
