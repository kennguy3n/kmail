# KMail — Progress

- **Project**: KMail — Privacy Email & Calendar for KChat B2B
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Foundation (in progress); Phase 2 —
  Prototype (in progress); Phase 3 — Private Beta (in progress)
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
system architecture, see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Phase 1 — Foundation (Weeks 1–4)

**Status**: `IN PROGRESS`

**Goal**: lock architecture, create project scaffolds, establish the
Stalwart integration plan, define the zk-object-fabric blob store
integration, and define the MLS encryption synergy model so Phase 2
engineers can implement without re-debating core decisions.

Checklist:

- [x] Ratify architecture: Stalwart mail core + Go control plane +
      React frontend + zk-object-fabric blob storage.
- [x] Evaluate Stalwart v0.16.0 — pin version, document breaking
      changes from earlier minor releases, plan the staging upgrade
      path to v1.0.0 (expected H1 2026).
- [x] Define zk-object-fabric integration: configure Stalwart's blob
      store backend to use zk-object-fabric's S3 endpoint, define
      per-tenant bucket layout, pick `EncryptionMode` defaults per
      privacy tier, and wire content-addressing (BLAKE3) alignment.
- [x] Define MLS ↔ KMail encryption key derivation model
      (confidential-send envelope keys, protected-folder master keys,
      shared-inbox group keys) and document in
      [ARCHITECTURE.md §5](ARCHITECTURE.md).
- [x] Define privacy mode mapping: Standard Private Mail →
      `ManagedEncrypted`, Confidential Send → `StrictZK`, Zero-Access
      Vault → `StrictZK`; per-mode server-search scope.
- [x] Define Go service boundaries (tenant, DNS onboarding, admin
      BFF, migration, chat bridge, calendar bridge, billing,
      deliverability, audit).
- [x] Define JMAP-first client API contract (BFF → Stalwart JMAP
      shape, capability negotiation, push semantics). See
      [JMAP-CONTRACT.md](JMAP-CONTRACT.md).
- [x] Define PostgreSQL schema for tenant metadata, users, domains,
      mailbox state, and calendar metadata. See
      [SCHEMA.md](SCHEMA.md) and
      [migrations/001_initial_schema.sql](../migrations/001_initial_schema.sql).
- [x] Define search tiering model (Core / Pro / Archive / Vault).
- [x] Stalwart commercial license evaluation (AGPL-3.0 base vs
      enterprise dual license) and KMail licensing compatibility
      decision. See [LICENSE-EVALUATION.md](LICENSE-EVALUATION.md).
- [x] Create Go project scaffold (`cmd/`, `internal/`, `api/`,
      `docs/`).
- [x] Create React project scaffold for KChat Mail + Calendar UI.

### Phase 1 decision gate

The Phase 1 gate is met when:

- All architecture decisions in this checklist are ratified and
  documented in [PROPOSAL.md](PROPOSAL.md) and
  [ARCHITECTURE.md](ARCHITECTURE.md).
- Stalwart version is pinned to v0.16.0 with a documented upgrade
  plan.
- zk-object-fabric integration shape is agreed with the
  zk-object-fabric maintainers.
- MLS key derivation model is reviewed by the KChat MLS owners.
- Go and React scaffolds exist in the repo.

**Gate status (2026-04-23)**:

| Criterion                                              | Status                      |
| ------------------------------------------------------ | --------------------------- |
| Architecture decisions ratified and documented         | Met — see ARCHITECTURE.md   |
| Stalwart pinned to v0.16.0 with upgrade plan           | Met — see PROPOSAL.md §1    |
| zk-object-fabric integration shape agreed              | Met — local dev stack now builds and runs the real zk-object-fabric S3 gateway (service `zk-fabric`, host ports `9080`/`9081`); Stalwart's blob store points at it over `http://zk-fabric:8080` with a one-bucket-per-tenant layout (`kmail-blobs` for the `kmail-dev` tenant) and `ManagedEncrypted` as the default `EncryptionMode`. See `docker-compose.yml` and `configs/stalwart.toml`. |
| MLS key derivation model reviewed                      | **Pending** — awaiting KChat MLS owner review of the confidential-send / protected-folder / shared-inbox derivation shape documented in ARCHITECTURE.md §5 |
| Go and React scaffolds exist in the repo               | Met — this PR               |

Phase 1 remains `IN PROGRESS` until the remaining pending external
review (MLS key derivation model) is closed out. The scaffolds,
contract documents, and schema are unblocking for Phase 2
engineering work that does not depend on the pending sign-off.

**Note**: zk-object-fabric Docker demo integration verified
end-to-end in local dev — Stalwart blob store writes and reads
through the zk-object-fabric S3 gateway via the `kmail-dev` tenant
(access key `kmail-access-key`). The compose stack boots Postgres,
Valkey, Meilisearch, zk-fabric, a one-shot `zk-fabric-init` bucket
creator, and Stalwart in that order; `aws --endpoint-url
http://localhost:9080 s3 ls s3://kmail-blobs/` lists objects written
by Stalwart. The gateway is the same S3 API contract that serves
Phase 1 Wasabi and Phase 2+ Ceph RGW deploys, so downstream code
does not change when the backend changes.

---

## Phase 2 — Prototype (Weeks 5–10)

**Status**: `IN PROGRESS`

**Goal**: a single-tenant prototype with custom-domain email, basic
calendar, JMAP webmail, IMAP/SMTP compatibility, and zk-object-fabric
blob storage wired end-to-end.

Delivered so far:

- Full **Tenant CRUD** — list / update / delete for tenants and
  users, all RLS-scoped where applicable; matching HTTP routes
  under `/api/v1/tenants/...`.
- **DNS Onboarding Service** — MX / SPF / DKIM / DMARC
  verification, `GenerateRecords` helper for the DNS wizard,
  mockable resolver interface for unit testing; mounted
  in-process by `cmd/kmail-api` and available as a standalone
  binary at `cmd/kmail-dns`.
- **Stalwart v0.16.0 automated bootstrap** — JSON bootstrap at
  `configs/stalwart-bootstrap.json` + JMAP admin-registry init
  script at `scripts/stalwart-init.sh`, wired into
  `docker-compose.yml` as a `stalwart-init` one-shot so
  `docker compose up` is now hands-off (no manual setup wizard).
- **Mail UI** — mailbox sidebar, email list, single-message
  reading pane, composer (To / Cc / Bcc / Subject / Body,
  From-identity selector, privacy-mode selector, Reply / Reply-All
  / Forward pre-fill, Save draft), per-row Mark read/unread and
  Move-to-trash / Delete, and now **full-text search** through a
  JMAP `Email/query` `text` FilterCondition with a per-mailbox /
  all-mailboxes scope toggle.
- **Calendar UI** — Day / Week / Month views with a 24-hour time
  grid (week/day) and 6×7 month grid, calendar-visibility
  sidebar, event detail panel with RSVP + Edit + Delete,
  slot-click that seeds `/calendar/new?start=&end=`,
  create / edit form backed by `CalendarEvent/set`, and deep-link
  route `/calendar/:eventId`. Speaks the draft JMAP calendars
  capability (`urn:ietf:params:jmap:calendars`) exposed by the Go
  BFF on top of Stalwart's CalDAV store.
- **Spam / phishing filtering** — Stalwart built-in classifier
  turned on via the JMAP admin registry in
  `scripts/stalwart-init.sh` (threshold + DNSBL + Bayesian
  auto-learn wiring + a Sieve rule that files into Junk), plus
  a `markAsSpam` helper in `web/src/api/jmap.ts` and a row-level
  `Spam` / `Not spam` action in `web/src/pages/Mail/Inbox.tsx`
  that flips `$junk` / `$notjunk` keywords and moves the email
  between Inbox and Junk atomically.
- **IMAP / SMTP compatibility** — `scripts/test-imap-smtp.sh`
  (STARTTLS capability checks + AUTH probe + RFC 5322
  round-trip via curl) plus `docs/COMPATIBILITY.md` with the
  full Thunderbird + Apple Mail setup matrix, port table, and
  manual test checklist.
- **CalDAV compatibility** — `scripts/test-caldav.sh`
  (OPTIONS + PROPFIND Depth:0/1 + PUT / GET / DELETE
  round-trip against `/dav/calendars/`) with matching Apple
  Calendar + Thunderbird sections in `docs/COMPATIBILITY.md`.
- **Admin UI** — Domain and User admin pages in
  `web/src/pages/Admin/` go from placeholders to functional
  screens wired to the Tenant Service REST surface. A new
  `web/src/api/admin.ts` holds the typed REST client
  (`listTenants`, `listDomains`, `verifyDomain`,
  `getDomainRecords`, `listUsers`, `updateUser`, `deleteUser`)
  and reuses the `authHeaders()` pattern from `jmap.ts`; a
  shared `useTenantSelection` hook keeps the selected tenant
  consistent across both pages via `localStorage`. DomainAdmin
  surfaces MX / SPF / DKIM / DMARC flags with a per-row Verify
  button and an expandable DNS-records panel; UserAdmin supports
  inline edit (display name / role / status / quota) and a
  confirmation-gated delete.
- **Migration Orchestrator** — `internal/migration/` ships the
  full `Service` + `Handlers` pair (tenant-scoped
  `/api/v1/migrations` CRUD, goroutine worker pool capped by
  `MaxConcurrent`, `imapsync` subprocess with progress-line
  parsing + checkpointing into Postgres), tenant-isolating
  `migration_jobs` table in `migrations/002_migration_jobs.sql`,
  and unit tests covering input validation + state transitions
  + the imapsync progress regex.

Checklist:

- [x] Stalwart deployment with PostgreSQL metadata backend +
      zk-object-fabric blob store backend + Meilisearch search +
      Valkey state. _(compose wiring + automated bootstrap;
      production wiring swaps the MinIO blob mock for the real
      zk-object-fabric gateway.)_
- [x] Go API Gateway / BFF with KChat auth integration.
      _(OIDC JWT signature verification against the issuer's JWKS
      with in-process caching, `iss` / `aud` / `exp` validation,
      Valkey-backed per-tenant / per-user rate limiting, dev-bypass
      path preserved for local work — see
      `internal/middleware/auth.go`, `internal/middleware/jwks.go`,
      `internal/middleware/ratelimit.go`.)_
- [x] Go Tenant Service (organizations, domains, users, aliases,
      shared inboxes, quotas). _(full CRUD, RLS-scoped.)_
- [x] Go DNS Onboarding Service (MX / SPF / DKIM / DMARC checks,
      domain verification).
- [x] React KChat Mail UI (inbox, compose, read, search).
      _(Inbox, compose, single-message read, and full-text search
      are live against the JMAP client — Inbox supports per-row
      Mark read/unread and Move to trash / Delete plus a search
      bar with per-mailbox / all-mailboxes scope via JMAP
      `Email/query` `text` FilterCondition, Compose drives
      `Email/set` + `EmailSubmission/set` with Reply / Reply-All /
      Forward pre-fill, MessageView marks-on-open and lists
      attachments.)_
- [x] React KChat Calendar UI (personal calendar, event create /
      edit, RSVP). _(Day / Week / Month views, calendar-visibility
      sidebar, event detail panel with RSVP + Edit + Delete,
      slot-click that seeds `/calendar/new`, create / edit form,
      deep-link `/calendar/:eventId`. Talks the draft JMAP
      calendars capability through the Go BFF; backend CalDAV
      wiring is in progress.)_
- [x] JMAP client integration (web app → Go BFF → Stalwart JMAP).
      _(`web/src/api/jmap.ts`: session fetch, typed
      `Mailbox/get` / `Email/query` / `Email/get` / `Email/set` /
      `EmailSubmission/set` helpers; RFC 8621 shapes in
      `web/src/types/index.ts`.)_
- [x] IMAP / SMTP compatibility testing (Thunderbird, Apple Mail).
      _(STARTTLS + AUTH probes, RFC 5322 round-trip via
      `scripts/test-imap-smtp.sh`, setup matrix in
      `docs/COMPATIBILITY.md`.)_
- [x] CalDAV compatibility testing.
      _(OPTIONS / PROPFIND / PUT / GET / DELETE round-trip via
      `scripts/test-caldav.sh`, Apple Calendar + Thunderbird
      client setup in `docs/COMPATIBILITY.md`.)_
- [x] Basic spam / phishing filtering via Stalwart.
      _(`spam-filter.*` settings + DNSBL + Bayesian auto-learn +
      Sieve Junk rule in `scripts/stalwart-init.sh`, `markAsSpam`
      helper in `web/src/api/jmap.ts`, Junk mailbox awareness +
      per-row `Spam`/`Not spam` action in
      `web/src/pages/Mail/Inbox.tsx`.)_
- [x] Gmail / IMAP migration orchestrator (Go + imapsync).
      _(`internal/migration/` Service + HTTP handlers under
      `/api/v1/migrations`, goroutine worker pool, imapsync
      progress parsing + Postgres checkpointing, RLS-scoped
      `migration_jobs` table, unit tests for validation + state
      transitions.)_
- [x] Admin UI (domains, users) wired to Tenant Service REST API.
      _(`web/src/api/admin.ts` typed REST client, shared
      `useTenantSelection` hook, DomainAdmin with MX/SPF/DKIM/DMARC
      flags + per-row Verify + Show DNS records expander,
      UserAdmin with inline Edit + confirmed Delete; this batch
      adds `getAuditLog` / `exportAuditLog` / `verifyAuditChain`
      on top for Phase 3 audit-log access.)_
- [x] Email-to-chat bridge (share email to KChat channel).
      _(`internal/chatbridge/` Service: ShareEmailToChannel,
      ConfigureAlertRoute, ListRoutes, DeleteRoute,
      ProcessInboundAlert; `chat_bridge_routes` migration with
      tenant-scoped RLS and a unique `(tenant_id, alias_address)`
      index; HTTP surface under `/api/v1/chat-bridge/...`; real
      `cmd/kmail-chat-bridge` entrypoint; unit tests with a mocked
      KChat client.)_
- [x] zk-object-fabric blob store integration verified end-to-end.
      _(PUT / GET round-trips via Stalwart's JMAP blob upload /
      download against `s3://kmail-blobs/` after the
      `scripts/stalwart-init.sh` rewrite — see the
      **Last updated** note for the verification method.
      Attachment-to-link presigned sharing is deferred to
      Phase 3.)_
- [x] Benchmark: inbox open P95 < 250 ms (warm), message open
      P95 < 300 ms, send accepted P99 < 1 s.
      _(Harness in `scripts/bench/`: `bench-jmap.go` measures
      Mailbox/get + Email/query + Email/get P50/P95/P99 with
      configurable warm-up and concurrency; `bench-smtp.sh` drives
      swaks against the submission port; `bench-caldav.sh` times
      the PUT path. `seed-data.sh` provisions 1 000 messages via
      JMAP. `make bench` runs the suite; `docs/BENCHMARKS.md`
      captures targets + baseline numbers.)_

---

## Phase 3 — Private Beta (Weeks 11–18)

**Status**: `IN PROGRESS`

**Goal**: multi-tenant private beta with 5–10 SME design partners,
deliverability infrastructure, IP reputation, and migration support.

Checklist:

- [x] Multi-tenant Stalwart shard (5,000–10,000 mailbox target).
      _(`internal/tenant/shard.go` registers the
      `ShardService` with `AssignTenantToShard` /
      `GetTenantShard` / `ListShards` / `RegisterShard` /
      `UpdateShardHealth` / `RebalanceShard`; assignments are
      least-loaded-with-capacity against `stalwart_shards` +
      `tenant_shard_assignments` (`migrations/017_stalwart_shards.sql`,
      no RLS on the shard registry, unique tenant assignment).
      `HealthWorker` probes every shard's `/healthz` on a 60 s
      ticker and flips offline shards out of rotation.
      `GetTenantShard` caches the lookup in-process and falls
      back to `cfg.StalwartURL` when no assignment exists, so
      the JMAP proxy stays backward-compatible. Admin routes
      under `/api/v1/admin/shards[/{id}[/rebalance]]` are
      mounted by `ShardHandlers`.)_
- [x] IP pool architecture (system transactional, mature trusted,
      new / warming, restricted, dedicated enterprise).
      _(`internal/deliverability/ippool.go` +
      `migrations/007_ip_pools.sql` with RLS, per-IP reputation +
      daily_volume + status, tenant pool assignment with
      priority, `SelectSendingIP` ranker that picks the best
      active IP from the tenant's highest-priority pool. Admin
      CRUD under `/api/v1/admin/ip-pools[/{id}/ips]` and tenant
      scoped `/api/v1/tenants/{id}/ip-pool`.)_
- [x] Tenant send limits and warmup schedule.
      _(`sendlimit.go` enforces daily + hourly caps via Valkey
      counters with TTL and returns `ErrSendLimitExceeded`;
      `warmup.go` implements a 30-day ramp anchored at 50 / 100
      / 500 / 1000 / 2000 / full on days 1 / 2 / 5 / 10 / 20 /
      30, clamped to the plan cap. Defaults 500 / 2000 / 5000
      per day for core / pro / privacy; hourly = daily / 10.
      Wired into the JMAP proxy path.)_
- [x] DNS wizard (MX, SPF, DKIM 2048-bit, DMARC, MTA-STS, TLS-RPT,
      autoconfig).
      _(`web/src/pages/Admin/DnsWizard.tsx` walks a tenant admin
      through seven ordered steps, rendering the expected
      record from `GET /api/v1/tenants/{id}/domains/{domainId}/dns-records`
      with a copy-to-clipboard button and driving verification
      via `POST /api/v1/tenants/{id}/domains/{domainId}/verify`.
      `getDnsWizardStatus` in `web/src/api/admin.ts` composes
      the records + verification payloads and pattern-matches
      each record to a wizard step. Route `/admin/dns-wizard`
      in `App.tsx`; nav link in `Layout.tsx`.)_
- [x] DMARC report ingestion.
      _(`internal/deliverability/dmarc.go` parses RFC 7489
      aggregate XML, persists to `dmarc_reports`
      (`migrations/008_dmarc_reports.sql`) with RLS, exposes
      list / summary / upload HTTP endpoints, and renders in
      `web/src/pages/Admin/DmarcAdmin.tsx` with per-domain
      pass-rate and drill-down.)_
- [x] Gmail Postmaster / Yahoo feedback loop monitoring.
      _(`internal/deliverability/feedbackloop.go` exposes
      `ProcessGmailPostmasterData`, `ProcessYahooARF`,
      `GetFeedbackSummary`, and `ListFeedbackEvents`; ARF parsing
      lives in `feedbackloop_helpers.go` per RFC 5965.
      `feedback_loop_events` (`migrations/011_feedback_loops.sql`)
      stores normalized events with RLS on `tenant_id` and
      indexes `(tenant_id, source, created_at DESC)` +
      `(tenant_id, domain, created_at DESC)`. HTTP routes
      `POST /api/v1/tenants/{id}/feedback-loops/{gmail,yahoo}`
      ingest data and `GET .../feedback-loops[/summary]` drives
      the UI. Wired via `deliverabilitySvc.FeedbackLoop` and
      `Handlers.RegisterPhase3`.)_
- [x] Suppression lists and bounce tracking.
      _(`internal/deliverability/suppression.go` +
      `bounce.go` with `migrations/006_suppression.sql` (RLS on
      `suppression_list` + `bounce_events`). Hard bounces /
      complaints escalate immediately; soft bounces escalate at
      3 within 72 h. `CheckRecipient` is the pre-send hook
      consumed by the JMAP proxy.)_
- [x] Abuse scoring and compromised-account detection.
      _(`internal/deliverability/abuse.go` implements an
      `AbuseScorer` with `ScoreTenant`, `ScoreUser`,
      `DetectAnomalies`, `ListAlerts`, and `AcknowledgeAlert`.
      Signals computed over a rolling window: volume spike
      (>3× 7-day average), recipient-domain anomaly (>50% new
      domains in 24 h), failed-auth storms (>10 in 5 min), high
      bounce (>5%/24 h), and high complaint (>0.1%/24 h).
      Alerts persist in `abuse_alerts` + cached composite
      `abuse_scores` (`migrations/012_abuse_scoring.sql`) with
      RLS and a severity enum (low/medium/high/critical).
      Routes `GET /api/v1/tenants/{id}/abuse/{score,alerts}` +
      `POST .../alerts/{alertId}/acknowledge`.)_
- [x] Pooled storage quotas (tenant pool, not per-user).
      _(`internal/billing/quota_worker.go` background goroutine
      polls the zk-object-fabric S3 API every
      `QuotaWorkerInterval` (default 5m) via the
      `StorageScanner` interface and rewrites
      `quotas.storage_used_bytes`. Plan-based per-seat limits
      (5 / 15 / 50 GB) resolve into the tenant's pooled
      `storage_limit_bytes`; `CheckStorageQuota` is the
      pre-write hook.)_
- [x] Shared inboxes (`sales@`, `support@`, `info@`) without
      requiring paid seats.
      _(`users.account_type` is now enforced end-to-end:
      `billing.CountSeats` filters
      `status = 'active' AND account_type = 'user'`, the
      Tenant Service validates the account_type enum on
      CreateUser, and the `SeatAccounter` interface only
      increments the seat counter for `user` rows. Shared
      inboxes and service accounts do not consume billable
      seats; unit test covers the exclusion.)_
- [x] Attachment-to-link conversion (> 10–15 MB → zk-object-fabric
      presigned link with expiry / password / revocation).
      _(`internal/jmap/attachment.go` implements a minimal SigV4
      presigner against the zk-object-fabric S3 endpoint;
      `attachment_handlers.go` exposes
      `POST /api/v1/attachments/upload`,
      `GET /api/v1/attachments/{id}/link`,
      `DELETE /api/v1/attachments/{id}`. Metadata persists in
      `attachment_links` (`migrations/009_attachment_links.sql`)
      with `revoked` flag. `web/src/pages/Mail/Compose.tsx`
      detects files > 10 MB and routes them through the new
      endpoint, appending a presigned link to the body.)_
- [x] Admin console (React) — tenant management, domain management,
      user management, quota management.
      _(Existing Tenant / Domain / User admin pages, plus new
      `QuotaAdmin.tsx` (usage progress bars, seat + storage
      counters, per-seat price, monthly total, PATCH form),
      `AuditAdmin.tsx` (filterable table, JSON/CSV export,
      hash-chain verify), and `DmarcAdmin.tsx` (per-domain
      pass-rate, drill-down, manual XML upload). Routes and nav
      links wired in `App.tsx` / `Layout.tsx`; typed client in
      `admin.ts` gains billing + DMARC helpers.)_
- [x] Admin audit logs.
      _(`internal/audit/` Service with hash-chained rows
      (SHA-256 over `prev_hash || canonical(payload)`),
      `audit_log` migration with RLS and `(tenant_id,
      created_at DESC)` / `(tenant_id, action, created_at DESC)`
      indexes, paginated Query with action / actor / resource /
      time-range filters, JSON + CSV Export, VerifyChain walker;
      HTTP routes under `/api/v1/tenants/{id}/audit-log[/export|
      /verify]`; `cmd/kmail-audit` CLI exposes
      `serve | verify | export`.)_
- [x] Mobile push notifications.
      _(`internal/push` ships a transport-agnostic `PushService`
      with `Subscribe` / `Unsubscribe` / `ListSubscriptions` /
      `SendNotification` / `GetPreferences` /
      `UpdatePreferences`. `push_subscriptions` +
      `notification_preferences` (`migrations/013_push_notifications.sql`)
      store device tokens (web/ios/android, `user_id TEXT` to
      admit either users.id UUIDs or Stalwart/KChat opaque IDs)
      with RLS and a unique `(tenant_id, user_id, push_endpoint)`
      constraint. Quiet-hours logic (`inQuietHours`) suppresses
      deliveries in the user-configured window. The
      `Transport` interface keeps actual APNs/FCM/Web Push
      shippers behind an interface; `loggingTransport` is the
      no-op dev default. HTTP routes under `/api/v1/push/...`
      mount via `push.Handlers.Register`. Typed client
      `web/src/api/push.ts` + `NotificationPrefs.tsx` page.)_
- [x] Resource calendars and shared team calendars.
      _(`internal/calendarbridge/sharing.go` adds
      `CreateCalendar` / `UpdateCalendar` / `DeleteCalendar` /
      `ShareCalendar` / `ListSharedCalendars` / `BookResource`
      and a `SharingStore` for the
      `calendar_shares` + `resource_calendars` tables
      (`migrations/014_shared_calendars.sql`, RLS, share matrix
      on `(tenant_id, calendar_id, target_account_id)`,
      resource registry with type = room/equipment/vehicle).
      `BookResource` runs a pre-insert conflict check via
      upstream `GetEvents` + overlap comparison and synthesizes
      a minimal iCalendar event when the caller doesn't provide
      one. HTTP routes
      `POST/GET/PUT/DELETE /api/v1/calendars` +
      `/api/v1/calendars/shared` + `/api/v1/resource-calendars`
      via `SharingHandlers`. React pages
      `SharedCalendars.tsx` + `ResourceCalendarAdmin.tsx` wire
      into `App.tsx` / `Layout.tsx`; typed client
      `web/src/api/calendarSharing.ts`.)_
- [x] Confidential Send mode (MLS-derived envelope keys, encrypted
      portal for external recipients).
      _(`internal/confidentialsend/service.go` adds `Service` with
      `CreateSecureMessage`, `GetSecureMessage`, `RevokeLink`, and
      `ListSentSecureMessages`. Each link gets a 32-byte
      base64url token, optional bcrypt-hashed password, expiry
      (max 30 days), and max-views (default 1). The
      public-portal handler in `internal/confidentialsend/handlers.go`
      enforces 5 attempts per token per 15 minutes through Valkey
      and surfaces the link without auth at
      `GET/POST /api/v1/secure/{token}`. Tenant-scoped
      `POST/GET /api/v1/tenants/{id}/confidential-send` and
      `DELETE .../confidential-send/{linkId}` round out the admin
      surface. `migrations/027_confidential_send.sql` creates
      `confidential_send_links` (RLS that allows the public
      portal path through, FOR UPDATE row-locking on view-count
      bumps). Frontend: `Compose.tsx` exposes expiry / password /
      max-views controls when "Confidential Send" is selected and
      shows a copy-to-clipboard link after send;
      `web/src/pages/Mail/SecurePortal.tsx` is the public-facing
      page at `/secure/:token` that prompts for the password
      when needed and renders message metadata / remaining views /
      expiry. Typed client `web/src/api/confidentialSend.ts`.
      MLS key derivation stubs are in place — full MLS
      integration follows external review per the privacy mode
      mapping in the architecture doc.)_
- [x] Billing / quota service (storage accounting, seat accounting,
      plan enforcement).
      _(`internal/billing/` Service with
      GetQuota / UpdateStorageUsage / CountSeats /
      EnforcePlanLimits / GetPlanPricing / CalculateInvoice;
      `billing_events` table (`migrations/005_billing.sql`) with
      RLS; handlers under `/api/v1/tenants/{id}/billing[/usage
      |/invoice]` + PATCH for admin limit overrides. Per-seat
      pricing is $3 / $6 / $9 for core / pro / privacy.)_
- [x] Observability (Prometheus, OpenTelemetry, Loki).
      _(`internal/middleware/metrics.go` registers
      `kmail_http_requests_total`,
      `kmail_http_request_duration_seconds`,
      `kmail_jmap_proxy_duration_seconds`, `kmail_active_tenants`,
      and `kmail_seats_total{plan=...}`; `/metrics` is
      unauthenticated. `internal/middleware/tracing.go` wires an
      OTLP/HTTP exporter against `OTEL_EXPORTER_OTLP_ENDPOINT`
      and the W3C `traceparent` propagator. `logger.go` emits
      structured JSON lines (tenant_id, user_id, trace_id) when
      `KMAIL_LOG_FORMAT=json`. A new `prometheus` service in
      `docker-compose.yml` scrapes the BFF via
      `deploy/prometheus/prometheus.yml`. Loki shipping is still
      out-of-scope.)_
- [ ] Beta customer onboarding (5–10 SMEs). _(Remaining open
      item — operational gate, not a code task.)_

---

## Phase 4 — Production SME Launch (Weeks 19–28)

**Status**: `NOT STARTED`

**Goal**: production launch with published pricing tiers, full
deliverability infrastructure, and migration automation.

Checklist:

- [x] Production Stalwart cluster (multi-node, HA).
      _(Operator template + guide in `deploy/stalwart/ha-config.json`
      + `deploy/stalwart/README.md`: per-shard JSON pinning the
      shared Postgres / zk-object-fabric / Meilisearch / Valkey
      stores, per-node identity (node ID, stable outbound IP,
      PTR record), trusted-network rule for the BFF
      `X-KMail-Stalwart-Account-Id` header, and ACME automation.
      `internal/jmap/proxy.go` is now shard-aware: every request
      resolves the tenant's primary Stalwart URL via
      `tenant.ShardService.GetTenantShard`, and the new
      `GetSecondaryShards` method walks the
      `shard_failover_config` table
      (`migrations/020_shard_failover.sql`, FK to
      `stalwart_shards`, ordered by `priority`) for backups. The
      proxy's custom `shardFailoverTransport` retries against
      backups on 5xx / transport errors and trips an in-process
      circuit breaker (default 3 consecutive failures) so a
      degraded host gets skipped until the shard health worker
      probes it healthy. `cmd/kmail-api/main.go` constructs
      `shardSvc` early and passes it as `jmap.ProxyConfig.Shards`.
      Falls back to `cfg.StalwartURL` for tenants without a shard
      assignment so the single-shard dev compose stack keeps
      working.)_
- [x] Production zk-object-fabric integration (Wasabi primary,
      Linode cache).
      _(Per-tenant bucket provisioning + placement policy wiring.
      `internal/tenant/zkfabric.go` adds a `ZKFabricProvisioner`
      that, on `CreateTenant`, mints a dedicated S3 bucket
      (pattern `kmail-{tenant_id}`, idempotent — 409 treated as
      success), POSTs `/api/tenants/{id}/keys` on the fabric
      console for per-tenant credentials, and PUTs
      `/api/tenants/{id}/placement` defaulting to `managed`
      (ManagedEncrypted) for core/pro and reserving `client_side`
      (StrictZK) for the privacy tier's Confidential Send /
      Zero-Access Vault. Credentials persist in
      `tenant_storage_credentials` (`migrations/018_tenant_storage.sql`,
      RLS on tenant_id) including `bucket_name`,
      `placement_policy_ref`, and `encryption_mode_default`.
      `internal/jmap/attachment.go` `UploadLargeAttachment` /
      `GeneratePresignedURL` now lookup the tenant's bucket via
      `resolveTenantBucket` and fall back to the global
      `cfg.Bucket` for legacy tenants. `tenant.ServiceConfig`
      gains `WithStorageProvisioner` to wire the provisioner
      into the lifecycle. The cross-shard wiring is documented
      in `scripts/stalwart-init.sh` (init script left single-
      tenant for dev) and the production playbook in
      `deploy/stalwart/README.md`. Constraint hard-pinned in the
      provisioner: bucket per tenant, no cross-tenant dedupe.)_
- [x] IP reputation dashboards.
      _(`Handlers.RegisterPhase3` adds
      `GET /api/v1/admin/ip-reputation` and
      `GET /api/v1/admin/ip-reputation/{ipId}/history` which
      join `IPPoolService`, `BounceProcessor`, and
      `DMARCService` into per-IP metrics (reputation, daily
      volume, bounce rate, complaint rate, pool, status,
      warmup day). `web/src/pages/Admin/IpReputationAdmin.tsx`
      renders a pool roll-up, a per-IP detail table with
      color-coded reputation indicators (green ≥ 80, yellow
      50–80, red < 50), and an expandable 30-day trend row.
      Typed client helpers `listIpReputation` +
      `getIpReputationHistory` in `admin.ts`. Route
      `/admin/ip-reputation` in `App.tsx`; nav in `Layout.tsx`.)_
- [x] Automated deliverability alerts.
      _(`internal/deliverability/alerts.go` implements
      `AlertService` with `EvaluateThresholds` / `ListAlerts` /
      `AcknowledgeAlert` / `ConfigureThresholds` /
      `ListThresholds`. Default thresholds: bounce_rate
      5% warning / 10% critical, complaint_rate 0.1% / 0.3%,
      reputation_drop 20 / 40 points / 24 h, daily_volume
      spike 5× / 10× 7-day average. `deliverability_alerts` +
      `alert_thresholds` (`migrations/015_deliverability_alerts.sql`)
      with RLS. `AlertEvaluator` is a background goroutine that
      iterates every tenant on a 15-min ticker (pattern mirrors
      `billing.QuotaWorker`). HTTP routes
      `GET/POST /api/v1/tenants/{id}/deliverability/alerts[/acknowledge]`
      and `GET/PUT .../thresholds`. Typed client helpers
      `listDeliverabilityAlerts`, `ackDeliverabilityAlert`,
      `listAlertThresholds`, `updateAlertThresholds` in
      `admin.ts`.)_
- [x] Shared mailbox workflows.
      _(`internal/sharedinbox` adds a `WorkflowService` with
      `AssignEmail` / `UnassignEmail` / `AddNote` / `ListNotes`
      / `SetStatus` / `ListAssignments` over the
      `shared_inbox_assignments` + `shared_inbox_notes` tables
      (`migrations/016_shared_inbox_workflows.sql`, RLS,
      indexes on (tenant_id, shared_inbox_id, status) and
      (tenant_id, shared_inbox_id, email_id)). Status enum
      `open → in_progress → waiting → resolved → closed`; the
      assignment row is upserted via
      `ON CONFLICT (tenant_id, shared_inbox_id, email_id)` so
      assign and status updates share the same code path. HTTP
      routes `/api/v1/shared-inboxes/{inboxId}/emails/{emailId}/...`.
      React page `SharedInboxView.tsx` + typed client
      `web/src/api/sharedinbox.ts`.)_
- [x] Calendar bridge (KChat scheduling, meeting rooms, reminders,
      chat notifications).
      _(`internal/calendarbridge/notifications.go` adds a
      `Notifier` with `NotifyEventCreated` / `NotifyEventUpdated` /
      `NotifyEventCancelled` / `NotifyReminder` reusing the
      existing `chatbridge.KChatClient` (exposed via
      `chatbridge.Service.KChat()`) so the package does not
      duplicate the KChat REST plumbing. The handlers in
      `calendarbridge.handlers.go` fan event CRUD into the
      notifier post-success. `internal/calendarbridge/reminder_worker.go`
      is a 60 s-tick goroutine that scans the upcoming-30 m window,
      fires reminders at the 15-min and 5-min thresholds, and
      dedupes via Valkey keys `reminder:{tenantID}:{eventID}:{minutesBefore}`
      with 24 h TTL. Channel resolution uses a
      `StaticChannelResolver` for Phase 4 (one channel per
      tenant); per-resource channel selection is the Phase 5
      follow-up. Wired in `cmd/kmail-api/main.go` alongside the
      existing alert / shard-health workers.)_
- [x] Tenant-level billing integration.
      _(`internal/billing/lifecycle.go` adds `Lifecycle.OnTenantCreated`,
      `OnTenantDeleted`, `OnPlanChanged` (with proration:
      `(new_seat_cents - old_seat_cents) * seat_count *
      remaining_days / period_days`), `OnSeatAdded`, and
      `OnSeatRemoved`. `tenant.ServiceConfig.WithBillingLifecycle`
      wires the hooks into `CreateTenant` / `DeleteTenant`. The
      billing handlers gain
      `GET /api/v1/tenants/{id}/billing/proration-preview` and
      `GET /api/v1/tenants/{id}/billing/history`. A Stripe webhook
      stub at `internal/billing/webhook.go`
      (`POST /api/v1/billing/webhooks/stripe`) parses
      `payment_intent.succeeded`, `invoice.paid`,
      `invoice.payment_failed`, and `customer.subscription.updated`
      with HMAC-SHA256 signature verification (dev mode bypasses
      empty secrets). `migrations/019_billing_lifecycle.sql`
      creates `billing_subscriptions` (RLS, status enum, trigger
      for `updated_at`). The hooks degrade gracefully when Stripe
      is unconfigured so dev keeps working without a webhook
      secret.)_
- [x] Published pricing: KChat Core Email, KChat Mail Pro,
      KChat Privacy.
      _(Three-tier matrix — KChat Core Email ($3 / seat / mo,
      500 sends / day, 5 GB / seat), KChat Mail Pro ($6, 2,000,
      15 GB), KChat Privacy ($9, 5,000, 50 GB) — surfaced via
      `web/src/pages/Admin/PricingAdmin.tsx`. The page reads the
      tenant's current plan from `getBillingSummary`, highlights
      the matching column, shows seat count × per-seat cents as
      a current monthly total, and offers upgrade / downgrade
      buttons that POST to the new
      `PATCH /api/v1/tenants/{id}/billing/plan` endpoint. Backend
      `billing.Service.ChangePlan` validates the plan name,
      updates `tenants.plan` under RLS, syncs the per-seat
      default on `quotas.storage_limit_bytes` only when the
      existing limit matches the previous plan default
      (preserving operator overrides made via PATCH .../billing),
      re-runs `EnforcePlanLimits` so a downgrade past current
      usage surfaces `ErrQuotaExceeded` (HTTP 402), and writes a
      `plan_changed` row to `billing_events` for audit. Static
      `PLAN_CATALOG` in `web/src/api/admin.ts` keeps marketing
      copy and the upgrade flow on a single source of truth.)_
- [x] Migration automation (Gmail / IMAP import wizard, staged
      sync, cutover checklist).
      _(Backend orchestrator + worker pool and staged sync were
      already landed in `internal/migration/`. This batch adds
      the tenant-facing UI: `web/src/pages/Admin/MigrationAdmin.tsx`
      is a 3-step wizard (source → credentials → confirm) with
      pre-filled IMAP host/port for Gmail and Microsoft 365, a
      job table with pause / resume / cancel actions, a 5 s
      auto-refresh while any job is running, and a post-cutover
      checklist. Typed client helpers `listMigrationJobs`,
      `createMigrationJob`, `getMigrationJob`,
      `pauseMigrationJob`, `resumeMigrationJob`, and
      `cancelMigrationJob` in `admin.ts`. Route
      `/admin/migrations` in `App.tsx`; nav in `Layout.tsx`.
      A follow-up patch in the same window adds
      `migration.Service.TestConnection` (real IMAP LOGIN / LOGOUT
      with a 10 s deadline, implicit-TLS on 993 and plain TCP
      otherwise, IMAP NO / BAD lines surfaced verbatim) and
      `POST /api/v1/migrations/test-connection`. The wizard's
      step-2 credentials form gains a "Test connection" button
      that calls `testMigrationConnection` in `admin.ts` and
      renders a green / red inline result, so operators can
      validate IMAP credentials before committing to a job.)_
- [x] Availability target: 99.9%.
      _(`internal/monitoring/slo.go` adds an `SLOTracker` that
      records every BFF request's success/latency into Valkey
      sorted sets (`slo:{tenantID}:requests` and
      `:latency`). `middleware.Metrics.WithSLO` wires the
      tracker into the existing metrics middleware so every
      request is mirrored without changing the request path.
      `internal/monitoring/handlers.go` exposes
      `GET /api/v1/admin/slo` (platform-wide),
      `GET /api/v1/admin/slo/{tenantId}`, and
      `GET /api/v1/admin/slo/breaches` returning availability
      ratios + P50/P95/P99 latencies + 24 h breach windows.
      Frontend page `web/src/pages/Admin/SloAdmin.tsx` renders
      a platform availability gauge, per-tenant card, and
      breach history; typed client helpers `getSloOverview`,
      `getTenantSlo`, `getSloBreaches` in `admin.ts`. Default
      target 99.9% (`monitoring.DefaultTarget`).)_

---

## Phase 5 — Privacy & Compliance Expansion (Post-Launch)

**Status**: `COMPLETE` — all original Phase 5 items live as of
2026-04-26 (the closeout batch landed SCIM 2.0 provisioning, the
reverse access proxy, and the compliance documentation pack).
The same batch added natural follow-ups (real export fan-out,
retention enforcement, per-resource calendar channel routing,
BIMI DNS support, CardDAV contact bridge, tenant outbound
webhooks, and a guided onboarding checklist).

Closeout checklist (added 2026-04-26):

- [x] SCIM 2.0 provisioning endpoint
      _(`internal/scim/{schema,service,handlers}.go` mounts
      `/scim/v2/Users` + `/scim/v2/Groups` with
      `application/scim+json`, ListResponse pagination, RFC 7643
      schemas; bearer tokens stored as SHA-256 hashes in
      `scim_tokens` (migration 028) with RLS; admin UI at
      `web/src/pages/Admin/ScimAdmin.tsx`.)_
- [x] Reverse access proxy for admin operations
      _(`internal/adminproxy/{service,handlers}.go` gates SRE
      reads of tenant data behind the existing approval
      workflow; routes at
      `/api/v1/admin/proxy/{tenantId}/...` with session
      tracking in `admin_access_sessions` (migration 029); every
      hop is logged through `audit.Service`.)_
- [x] Compliance documentation pack
      _(`docs/compliance/` adds DPA, SOC 2 control mapping,
      Article 30 records, sub-processor list, and the customer-
      facing security overview.)_

**Goal**: advanced privacy features, compliance controls, and
enterprise readiness.

Checklist:

- [x] Zero-Access Vault (client-side encrypted folders via
      zk-object-fabric `StrictZK` + MLS key hierarchy).
      _(`internal/vault/service.go` adds `VaultService` with
      `CreateVaultFolder` / `ListVaultFolders` / `GetVaultFolder`
      / `DeleteVaultFolder` / `SetFolderEncryptionMeta` (stores
      the wrapped DEK + key algorithm + nonce; the plaintext key
      never leaves the client). All methods use
      `pgx.BeginFunc` + `middleware.SetTenantGUC` so RLS scopes
      every read/write to the caller's tenant. HTTP routes live
      under `/api/v1/tenants/{id}/vault/folders`.
      `migrations/024_vault_folders.sql` creates the
      `vault_folders` table (UUID PK, tenant_id FK, user_id,
      folder_name, encryption_mode default `StrictZK`,
      wrapped_dek BYTEA, key_algorithm default
      `XChaCha20-Poly1305`, nonce, RLS) plus the
      `kmail_set_updated_at` trigger. Frontend page
      `web/src/pages/Mail/VaultView.tsx` lists vault folders with
      a lock icon, exposes a "Create Vault Folder" form gated on
      an explicit "I understand the server cannot search this
      folder" checkbox, and a folder detail view that renders
      the encryption metadata. Typed clients
      `listVaultFolders` / `createVaultFolder` /
      `deleteVaultFolder` / `setVaultFolderEncryptionMeta` in
      `web/src/api/admin.ts`. Per the do-not-do list, vault mode
      is opt-in per folder — no mailbox is zero-access by
      default.)_
- [x] Customer-managed keys (Privacy / Enterprise tier).
      _(`internal/cmk/service.go` adds `CMKService` with
      `RegisterKey` (validates the PEM, computes a SHA-256
      fingerprint, requires the tenant be on the privacy plan via
      a per-request lookup), `RotateKey` (atomically deprecates
      every active key for the tenant and inserts the new one
      under a single transaction), `RevokeKey`, `GetActiveKey`,
      and `ListKeys`. HTTP routes
      `GET / POST /api/v1/tenants/{id}/cmk`,
      `PUT /api/v1/tenants/{id}/cmk/{keyId}/rotate`,
      `DELETE /api/v1/tenants/{id}/cmk/{keyId}/revoke`,
      `GET /api/v1/tenants/{id}/cmk/active`. The handler returns
      `403 plan_not_eligible` for tenants outside the privacy
      plan. `migrations/025_customer_managed_keys.sql` creates
      `customer_managed_keys` (RLS, status check IN
      active/deprecated/revoked, key_fingerprint UNIQUE,
      algorithm default `RSA-OAEP-256`). Frontend page
      `web/src/pages/Admin/CmkAdmin.tsx` shows the active key
      with its fingerprint, accepts a PEM textarea or a `.pem`
      file upload, exposes Rotate (deprecates the prior active)
      and Revoke (with a confirmation modal) flows, and renders
      a friendly upgrade banner for non-privacy tenants. Typed
      clients `listCmkKeys` / `registerCmkKey` / `rotateCmkKey`
      / `revokeCmkKey` / `getActiveCmkKey` in
      `web/src/api/admin.ts`.)_
- [x] Regional storage controls (zk-object-fabric placement
      policies).
      _(`internal/tenant/placement.go` adds a `PlacementService`
      with `GetPlacementPolicy` (reads from the local
      `tenant_storage_credentials` row + fetches from the fabric
      console), `UpdatePlacementPolicy` (validates non-empty
      country allow-list, gates `client_side` to the privacy
      plan, PUTs to the fabric console, mirrors
      `encryption_mode_default` locally), and
      `ListAvailableRegions` (US/EU/APAC). `placement_handlers.go`
      registers `GET /api/v1/storage/regions`, `GET` /
      `PUT /api/v1/tenants/{id}/storage/placement`. Frontend page
      `web/src/pages/Admin/StoragePlacementAdmin.tsx` lets admins
      pick allowed regions, change encryption mode (StrictZK
      disabled outside privacy tier), and warns that existing
      data is not auto-migrated. Typed clients
      `getPlacementPolicy`, `updatePlacementPolicy`,
      `listRegions` in `admin.ts`.)_
- [x] Retention / archive tier (zk-object-fabric cold storage).
      _(`internal/retention` adds `Service` with policy CRUD
      (`CreatePolicy` / `UpdatePolicy` / `DeletePolicy` /
      `ListPolicies`) and `EvaluateRetention`. `Worker` is a
      24 h-tick goroutine that walks active tenants and emits
      retention summaries (the actual JMAP `Email/set destroy`
      and zk-object-fabric placement archive update is staged as
      a Phase 5 follow-up — the storage hook lives behind a
      pluggable runner pattern so the fan-out PR is a drop-in).
      `migrations/021_retention_policies.sql` creates
      `retention_policies` (RLS, policy_type IN archive/delete,
      applies_to IN all/mailbox/label, target_ref). Frontend page
      `web/src/pages/Admin/RetentionAdmin.tsx` adds policy
      create/list/delete; typed clients
      `listRetentionPolicies`, `createRetentionPolicy`,
      `deleteRetentionPolicy` in `admin.ts`.)_
- [x] Advanced export and eDiscovery preparation.
      _(`internal/export` adds `Service` with `CreateExportJob`,
      `GetExportJob`, `ListExportJobs`, and a pluggable
      `Runner` callback that owns archive packaging — wired in
      `cmd/kmail-api/main.go` to a stub URL today and the
      JMAP/CalDAV/audit fan-out follow-up will plug into the
      same callback. `internal/export/worker.go` is a worker
      pool that claims pending jobs via
      `FOR UPDATE SKIP LOCKED`, runs the runner, and writes
      success/error back to the row. HTTP routes
      `GET` / `POST /api/v1/tenants/{id}/exports` and
      `GET /api/v1/tenants/{id}/exports/{jobId}`.
      `migrations/023_export_jobs.sql` creates `export_jobs`
      (RLS, status pending/running/completed/failed, format
      mbox/eml/pst_stub, scope all/mailbox/date_range). Frontend
      page `web/src/pages/Admin/ExportAdmin.tsx` lets admins
      queue exports and surface the download URL once
      complete; typed clients `listExports`, `createExport` in
      `admin.ts`.)_
- [x] Admin access approval workflow.
      _(`internal/approval` adds `Service` with `RequiresApproval`
      / `CreateRequest` / `ApproveRequest` / `RejectRequest` /
      `ListPendingRequests` / `ListAll` / `ExecuteApproved` and a
      pluggable per-action `Executor` registry so callers (tenant
      service, billing, retention) can register their executors
      without the approval package depending on them.
      `migrations/022_approval_workflow.sql` creates
      `approval_requests` (RLS, status pending/approved/rejected/
      expired, 7-day default expiry) and `approval_config`
      (per-tenant + per-action gating boolean). HTTP routes
      `/api/v1/tenants/{id}/approvals[/{approvalId}{,/approve,/reject,/execute}]`
      and `/approvals/config` (GET/PUT). Frontend page
      `web/src/pages/Admin/ApprovalAdmin.tsx` lists pending
      approvals, lets reviewers approve/reject, and toggles the
      gating config per action; typed clients `listApprovals`,
      `approveApprovalRequest`, `rejectApprovalRequest`,
      `getApprovalConfig`, `setApprovalConfig` in `admin.ts`.)_
- [x] Protected folders.
      _(`internal/vault/protected.go` adds
      `ProtectedFolderService` with `CreateProtectedFolder` /
      `ListProtectedFolders` / `ShareFolder` (grants
      read-or-read_write access to a teammate inside the same
      tenant — cross-tenant sharing is intentionally out of
      scope per the do-not-do list) / `UnshareFolder` /
      `ListFolderAccess` / `GetFolderAccessLog`. Every share /
      revoke writes a row to the audit log table. HTTP routes
      under `/api/v1/tenants/{id}/protected-folders` (list /
      create / `{folderId}/share` / `/unshare` / `/access` /
      `/access-log`). `migrations/026_protected_folders.sql`
      creates `protected_folders`, `protected_folder_access`
      (permission CHECK IN read/read_write), and
      `protected_folder_access_log`, all RLS-scoped on
      tenant_id. Frontend page
      `web/src/pages/Mail/ProtectedFolderView.tsx` lists
      folders with a lock icon, exposes a "Share with team
      member" modal with a permission selector, and renders the
      access log table. Typed clients `listProtectedFolders` /
      `createProtectedFolder` / `shareProtectedFolder` /
      `unshareProtectedFolder` / `listProtectedFolderAccess` /
      `getProtectedFolderAccessLog` in `web/src/api/admin.ts`.)_
- [x] Availability target: 99.95%+.
      _(`monitoring.DefaultTarget` is now 0.9995, with a
      `LegacyTarget` constant retained at 0.999 for the Phase 4
      baseline and an explicit `HighAvailabilityTarget` constant
      so call sites can document intent. `MultiRegionAggregator`
      in `internal/monitoring/multiregion.go` reads region-prefixed
      Valkey keys (`slo:region:{region}:requests`) and folds the
      per-region totals into a global rollup; the
      `KMAIL_SLO_REGIONS` env var (comma-separated) drives the
      fan-out. `GET /api/v1/admin/slo/regions` returns the
      aggregator output. `internal/middleware/degradation.go`
      provides graceful-degradation middleware: when the upstream
      Stalwart shard is unhealthy, GET requests on configured
      read prefixes (default `/jmap`) fall back to a Valkey-cached
      response with `X-KMail-Degraded: true`, while POSTs/PUTs/
      DELETEs return 503 (silent failure on writes is worse than
      loud failure). `web/src/pages/Admin/SloAdmin.tsx` adds a
      region selector, a per-region availability table with a
      global rollup row, and renders the 99.95% target
      alongside the 99.9% legacy line.)_

---

## Phase 6 — Enterprise Readiness

**Status**: `PLANNED` — opens after the Phase 5 closeout batch
ships (2026-04-26). Phase 6 picks up the natural follow-ups
identified during the closeout and the longer-running enterprise
work that was deferred from earlier phases.

Checklist:

- [ ] Real MLS group integration for Confidential Send
      (currently link-based; replace with native MLS rekey on
      participant change).
- [ ] BYOC HSM for customer-managed keys (KMIP / PKCS#11 envelope
      backed by tenant-provided HSM rather than only PEM upload).
- [ ] Exchange interop **research only** — produce a
      compatibility matrix and decide whether to invest. Per the
      do-not-do list, do **not** start an Exchange interop build
      without an explicit phase decision.
- [ ] SCIM provisioning conformance test suite (run the SCIM 2.0
      reference test runner against `/scim/v2/...` and publish
      the results).
- [ ] Webhook HMAC v2 signing scheme that includes a replay-
      protection nonce and a versioned secret.
- [ ] Retention enforcement worker default flip (after a quarter
      of dry-run telemetry, default `KMAIL_RETENTION_DRY_RUN` to
      `false` and document the operator opt-out flag).
- [ ] CardDAV directory bridge to surface a tenant-wide global
      address list (currently per-account address books only).
- [ ] BIMI VMC issuance helper / vendor partnership so tenants
      can buy a Verified Mark Certificate inside the admin
      console.
- [ ] Onboarding checklist auto-completion via webhook events
      (e.g. mark "send test email" complete the moment the
      `email.received` event for the tenant's first inbound
      message lands).
- [ ] Reverse access proxy session expiry watcher (Phase 5 ships
      explicit revoke + TTL; Phase 6 adds a worker that emits a
      `session_expired` audit row when the TTL elapses without a
      revoke).

---

## Appendix: Key Metrics to Track

These targets carry over from [PROPOSAL.md §13](PROPOSAL.md). They
are the exit criteria for "prototype is production-acceptable" and
the SLO baseline for Phase 4 launch.

| Workload                      | Tool                         | Target                                     |
| ----------------------------- | ---------------------------- | ------------------------------------------ |
| Inbox open (warm cache)       | Custom harness               | P95 < 250 ms                               |
| Message open (with body)      | Custom harness               | P95 < 300 ms                               |
| Full-text search (per user)   | Meilisearch load generator   | P95 < 500 ms                               |
| Send accepted                 | `smtp-source`                | P99 < 1 s                                  |
| Calendar event create         | CalDAV client                | P95 < 500 ms                               |
| JMAP sync (cold device)       | JMAP client                  | P95 < 2 s for 1,000 messages                |
| SMTP retry queue              | Stalwart queue metrics       | < 1% deferred > 4 h                        |
| Availability                  | Uptime monitoring            | 99.9% Phase 4, 99.95%+ Phase 5             |
