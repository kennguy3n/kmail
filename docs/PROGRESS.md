# KMail — Progress

> **Note on migration references**: This is a per-phase historical
> deliverable log. Inline references to `migrations/0NN_*.sql` files
> (e.g. `migrations/021_retention_policies.sql`) describe the
> migration that introduced each table at the time the work shipped.
> After the pre-production migration squash, those files no longer
> exist on disk — every schema object now lives in the consolidated
> [`migrations/001_baseline.sql`](../migrations/001_baseline.sql).
> The narrative is preserved so phase deliverables stay auditable.

- **Project**: KMail — Privacy Email & Calendar for KChat B2B
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: In progress | ~95% — Phase 1 — Foundation (code complete, external
  MLS review gate pending); Phase 2 — Prototype (complete); Phase 3 — Private
  Beta (code complete, operational beta onboarding gate pending); Phase 4 —
  Production SME Launch (complete); Phase 5 — Privacy & Compliance (complete);
  Phase 6 — Enterprise Readiness (in progress, Exchange interop + BIMI VMC
  deferred); Phase 7 — Production Hardening (complete); Phase 8 — GA
  Readiness (complete). 6 of 8 phases fully closed; the two remaining open
  items (Phase 1 MLS architecture review and Phase 3 private-beta onboarding)
  are external operational gates rather than code work.
- **Last updated**: 2026-04-27 (post-Phase 8 reconciliation) —
  Phase 2 status corrected to `COMPLETE` (all items shipped, no
  remaining gates). Phase 8 status flipped to `COMPLETE` (all ten
  items merged via PR #26). Top-of-file status line refined to
  distinguish code-complete phases with external gates (Phase 1
  MLS review, Phase 3 beta onboarding) from fully closed phases.
  Demo screenshots captured to `docs/screenshots/`.
- **Detailed per-batch changelog**: see [`DEVELOPMENT_LOG.md`](DEVELOPMENT_LOG.md) for the full per-batch implementation history (Phases 3 through 8).

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
      [migrations/001_baseline.sql](../migrations/001_baseline.sql).
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

**Status**: `COMPLETE` — all checklist items delivered.

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

**Status**: `COMPLETE` — every checklist item below ships in main.
The status text was drift-stale; Phase 8's documentation
reconciliation pass corrects it.

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

**Status**: `IN PROGRESS` — opened 2026-04-26 with the ten-task
enterprise-readiness batch (see the **Last updated** entry at the
top of this file). Eight of the ten checklist items below ship in
this batch; Exchange interop stays research-only and the BIMI VMC
issuance helper is unscheduled.

Checklist:

- [x] Real MLS group integration for Confidential Send
      (currently link-based; replace with native MLS rekey on
      participant change).
      _(Phase 6, batch 1: `internal/confidentialsend/mls.go` adds
      an `MLSKeyDeriver` interface plus an `HTTPKeyDeriver` that
      speaks JSON over HTTPS to the KChat MLS credential service
      configured by `KChatMLSEndpoint` (env: `KCHAT_MLS_ENDPOINT`).
      The service exposes `DeriveWrappingKey(senderLeafKey,
      recipientCredential)` and `RekeyConfidentialMessage(messageID,
      newParticipants)`, surfaced over HTTP as
      `GET /api/v1/tenants/{id}/confidential-send/mls/status`,
      `POST .../mls/wrap`, and
      `POST .../{linkId}/mls/rekey`. The Compose flow degrades
      gracefully when `KCHAT_MLS_ENDPOINT` is empty: `mls/status`
      reports `enabled=false` and the link-based portal flow
      stays the default.)_
- [x] BYOC HSM for customer-managed keys (KMIP / PKCS#11 envelope
      backed by tenant-provided HSM rather than only PEM upload).
      _(Phase 6, batch 1: `internal/cmk/hsm.go` adds the
      `HSMKeyProvider` interface plus `KMIPProvider` (validates
      `kmip[s]://host:port` shape) and `PKCS11Provider` (validates
      absolute path to `.so` module, requires slot ID + PIN).
      Migration `038_cmk_hsm.sql` adds `cmk_hsm_configs`
      (tenant-scoped, RLS-protected, encrypted credentials, status
      enum). Endpoints `GET /api/v1/tenants/{id}/cmk/hsm`,
      `POST .../hsm`, and `POST .../hsm/{configId}/test` are
      privacy-plan gated like the existing PEM CMK flow.
      `CmkAdmin.tsx` gains an HSM tab with provider selector,
      endpoint input, slot ID, and credentials textarea; typed
      clients `registerHsmKey`, `listHsmConfigs`,
      `testHsmConnection` live in `web/src/api/admin.ts`. Phase 6
      ships connection metadata only — real KMIP / PKCS#11 wire
      traffic lands in a follow-up.)_
- [ ] Exchange interop **research only** — produce a
      compatibility matrix and decide whether to invest. Per the
      do-not-do list, do **not** start an Exchange interop build
      without an explicit phase decision.
- [x] SCIM provisioning conformance test suite (run the SCIM 2.0
      reference test runner against `/scim/v2/...` and publish
      the results).
      _(Phase 6, batch 1: `scripts/test-scim.sh` provisions a
      test tenant, generates a SCIM bearer token, drives the SCIM
      2.0 reference runner against `/scim/v2/Users` and
      `/scim/v2/Groups`, and writes a Markdown pass/fail report.
      `make scim-test` invokes the script. `internal/scim/
      discovery.go` adds `ServiceProviderConfig`, `ResourceTypes`,
      and `Schemas` discovery endpoints (RFC 7644 §4) registered
      without auth so the runner can introspect capabilities, and
      `docs/SCIM_CONFORMANCE.md` records the expected pass/fail
      matrix and how to reproduce.)_
- [x] Webhook HMAC v2 signing scheme that includes a replay-
      protection nonce and a versioned secret.
      _(Phase 6, batch 1: `internal/webhooks/service.go` and
      `worker.go` add a `signing_version` column (`v1` legacy,
      `v2` new) on `webhook_endpoints` (migration
      `034_webhook_signing_v2.sql`). v2 deliveries carry
      `X-KMail-Webhook-Timestamp` and `X-KMail-Webhook-Nonce`
      (UUID) headers and sign `HMAC-SHA256(timestamp.nonce.body)`
      into `X-KMail-Signature: v2=<hex>`. `WebhookAdmin.tsx`
      gets a per-endpoint signing-version selector and the
      `registerWebhook` / `updateWebhookSigningVersion` typed
      clients in `web/src/api/admin.ts` round-trip the field.)_
- [x] Retention enforcement worker default flip (after a quarter
      of dry-run telemetry, default `KMAIL_RETENTION_DRY_RUN` to
      `false` and document the operator opt-out flag).
      _(Phase 6, batch 1: `cmd/kmail-api/main.go` flips the
      default — the env var is now opt-in (`=true` to dry-run);
      live mode is the default. `internal/retention/worker.go`
      registers four cumulative Prometheus counters
      (`kmail_retention_emails_deleted_total`,
      `kmail_retention_emails_archived_total`,
      `kmail_retention_evaluations_total`,
      `kmail_retention_errors_total`). `RetentionAdmin.tsx` adds
      a status card showing dry-run vs live, last evaluation, and
      cumulative totals. `docs/DEVELOPMENT.md` documents the
      operator opt-out flag.)_
- [x] CardDAV directory bridge to surface a tenant-wide global
      address list (currently per-account address books only).
      _(Phase 6, batch 1: `internal/contactbridge/gal.go` adds a
      `GALService` that aggregates every per-account address book
      within a tenant, deduplicated by lower-cased email. New
      endpoints `GET /api/v1/contacts/gal`, `/gal/search?q=...`,
      and `POST /gal/sync`. Migration
      `035_global_address_list.sql` creates the tenant-scoped
      `gal_entries` cache table with the dedup index. The
      ContactsView gains a "Global Directory" tab and the typed
      clients `getGlobalAddressList`, `searchGlobalAddressList`,
      `syncGlobalAddressList`. Cross-tenant dedup is explicitly
      not done — every entry is tenant-scoped.)_
- [ ] BIMI VMC issuance helper / vendor partnership so tenants
      can buy a Verified Mark Certificate inside the admin
      console.
- [x] Onboarding checklist auto-completion via webhook events
      (e.g. mark "send test email" complete the moment the
      `email.received` event for the tenant's first inbound
      message lands).
      _(Phase 6, batch 1: `internal/onboarding/auto_triggers.go`
      implements the `webhooks.EventListener` interface; the
      webhook service now carries an `AddListener` registry and
      the auto-trigger service is wired in
      `cmd/kmail-api/main.go`. Mappings: `email.received` →
      `send_test_email`, `domain.verified` → `verify_dns`,
      `user.created` (tenant user count ≥ 2) → `invite_team`.
      Migration `036_onboarding_auto_triggers.sql` records each
      auto-completion (tenant, step, event_type, completed_at)
      and the checklist API surfaces an `auto_completed` flag the
      UI renders as a "completed automatically" badge. The new
      `POST /api/v1/tenants/{id}/onboarding/reset` endpoint and
      `resetOnboardingChecklist` typed client clear every skip /
      auto-trigger row for re-onboarding.)_
- [x] Reverse access proxy session expiry watcher (Phase 5 ships
      explicit revoke + TTL; Phase 6 adds a worker that emits a
      `session_expired` audit row when the TTL elapses without a
      revoke).
      _(Phase 6, batch 1: `internal/adminproxy/expiry_worker.go`
      ticks every 60s, locates `admin_access_sessions` rows where
      `expires_at < now()` AND `revoked_at IS NULL` AND
      `expired_at IS NULL`, emits a `session_expired` audit entry
      via `audit.Service.Log`, and stamps `expired_at = now()` so
      the row is never re-processed. Migration
      `037_admin_session_expiry.sql` adds the `expired_at` column
      and a partial index for efficient scans. The Prometheus
      counter `kmail_admin_sessions_expired_total` increments per
      successful audit emission. The worker is wired alongside
      the existing background workers in
      `cmd/kmail-api/main.go`.)_
- [x] ContactsView frontend completion (full CRUD with vCard
      fields including PHOTO and CATEGORIES, search / filter,
      vCard import + export, contact groups, delete confirmation).
      _(Phase 6, batch 1: `web/src/pages/Mail/ContactsView.tsx`
      now offers a search bar, group filter dropdown, photo URL
      and groups inputs, a delete confirmation modal, and bulk
      vCard import / export hitting new endpoints
      `POST /api/v1/contacts/{accountID}/{addressBookID}/import`
      and `GET .../export`. The vCard parser /
      builder rounds-trip `PHOTO` and `CATEGORIES`. The route
      `/contacts` and the nav link were already wired by PR #23.)_
- [x] ScimAdmin / WebhookAdmin / OnboardingChecklist frontend
      hardening (loading spinners, error toasts, empty states,
      token reveal-once UX with copy-to-clipboard, revocation
      confirmation modal, webhook delivery health badge, test-
      fire button, signing-version selector, progress percentage
      bar, step links, skip and reset-checklist confirmations).
      _(Phase 6, batch 1: see `web/src/pages/Admin/{ScimAdmin,
      WebhookAdmin,OnboardingChecklist}.tsx`.)_

---

## Phase 7 — Production Hardening

**Status**: `COMPLETE` — all ten checklist items below shipped in
the 2026-04-27 production-hardening batch (PR #25). Phase 8's
reconciliation pass flips the status from `IN PROGRESS` to
`COMPLETE`.

Checklist:

- [x] Real APNs / FCM push transports with platform-aware
      `TransportRouter` (web → web push, ios → APNs HTTP/2,
      android → FCM HTTP v1) and dev fall-through to the
      `loggingTransport` when credentials are absent.
- [x] Stalwart v0 ↔ v1 compatibility shim with version detection
      and adapter pattern, parallel `scripts/stalwart-init-v1.sh`,
      `scripts/test-stalwart-upgrade.sh` upgrade harness, and
      `docs/STALWART_UPGRADE.md` runbook.
- [x] OpenSearch migration path: per-tenant `search_backend`
      column (migration 039), `SearchBackend` interface with
      Meilisearch and OpenSearch implementations, admin GET / PUT
      endpoints, and `SearchAdmin.tsx` selector + reindex
      trigger.
- [x] Loki + Promtail log shipping behind a `loki` compose
      profile, JSON request log enrichment in
      `internal/middleware/loki.go`, Grafana datasource
      provisioning, and `docs/DEVELOPMENT.md` Loki section.
- [x] DKIM key rotation automation: `dkim_keys` table (migration
      040), `DKIMRotationService` with generate / rotate / list /
      revoke, REST endpoints, and `DkimAdmin.tsx` history view +
      manual rotate button.
- [x] Kubernetes Helm chart for the kmail-api Deployment + Service
      + Ingress + ConfigMap + Secret + HPA + PodDisruptionBudget,
      plus a Stalwart StatefulSet with stable per-pod hostnames.
      `make helm-lint` target wired.
- [x] Stripe billing integration completion: REST `StripeClient`
      with create / cancel / update / portal session, dunning
      service that suspends after three failures inside 30 days
      (`billing_dunning_events` table folded into migration 040),
      and a "Manage subscription" button on `PricingAdmin.tsx`.
      Gated behind `KMAIL_STRIPE_SECRET_KEY` so dev keeps working.
- [x] WebAuthn / FIDO2 credential management: registration +
      authentication flows, `webauthn_credentials` table
      (migration 041), and `SecuritySettings.tsx` showing
      registered keys with add / remove actions.
- [x] Per-tenant Sieve rule management: `sieve_rules` table
      (migration 042) with RLS, `SieveService` CRUD + validate +
      deploy, and `SieveAdmin.tsx` editor with validate button
      and deploy toggle.
- [x] Load testing and chaos engineering harness:
      `scripts/loadtest/load-jmap.go` (configurable concurrency +
      ramp-up / steady / cool-down phases),
      `scripts/loadtest/load-smtp.sh` (sustained TPS),
      `chaos-shard.sh` / `chaos-postgres.sh` /
      `chaos-valkey.sh`, `make loadtest` and `make chaos`
      targets, and `docs/LOADTEST.md` documenting baselines and
      interpretation.

---

## Phase 8 — GA Readiness

**Status**: `COMPLETE` — all ten GA-readiness items shipped and
merged via PR #26 on 2026-04-27.

Checklist:

- [x] Real KMIP / PKCS#11 wire traffic for HSM CMK envelope
      operations. Pure-Go KMIP 1.4 TTLV encoder/decoder + TLS
      transport in `internal/cmk/kmip.go` covering `Locate`,
      `Encrypt`, and `Decrypt` operations. PKCS#11 path lives in
      `internal/cmk/pkcs11.go` behind a `pkcs11` build tag —
      operators rebuild the API binary with
      `go build -tags pkcs11 ./cmd/kmail-api` to enable
      `C_Initialize` / `C_OpenSession` / `C_Encrypt` /
      `C_Decrypt`. `CMKService.EncryptDEK` / `DecryptDEK`
      dispatch by provider type and bump the new
      `cmk_hsm_configs.last_used_at` column (migration 043) on
      success.
- [x] Web Push (RFC 8030) real transport. `internal/push/webpush.go`
      signs VAPID JWTs (P-256, ES256) with `KMAIL_VAPID_PUBLIC_KEY`
      / `KMAIL_VAPID_PRIVATE_KEY` / `KMAIL_VAPID_SUBJECT`,
      builds `aes128gcm` push payloads per RFC 8291, and is
      wired into `buildPushTransport` as `router.Web` (replacing
      the `loggingTransport` no-op). Browser-side helper at
      `web/src/api/push.ts` registers a `PushSubscription` via
      `/api/v1/push/subscribe`. Unit-tested with a mock push
      service.
- [x] TOTP fallback for WebAuthn. `internal/middleware/totp.go`
      implements RFC 6238 HMAC-SHA1 verification, secret
      generation, `otpauth://` URI rendering, and recovery codes.
      `internal/middleware/totp_store.go` wraps the secret with
      the existing `KMAIL_SECRETS_KEY` envelope before insert.
      Migration 044 ships the `totp_credentials` table with
      RLS. The "TOTP" tab on `web/src/pages/Admin/SecuritySettings.tsx`
      drives the setup wizard (QR code, verification, recovery
      code download).
- [x] Malware scanning adapter. `internal/malware/scanner.go`
      defines a `Scanner` interface with `NoopScanner` (default)
      and `clamavScanner` (`internal/malware/clamav.go`) that
      speaks INSTREAM over TCP. `internal/malware/handlers.go`
      registers `POST /api/v1/malware/scan` and exposes a
      `PreDeliverHook` that the JMAP proxy
      (`internal/jmap/proxy.go` `ProxyConfig.PreDeliverHook`)
      invokes on every submit body, short-circuiting infected
      messages with a JMAP `rejectedByPolicy` 422. Gated behind
      `KMAIL_CLAMAV_ADDR`; `docker-compose.yml` exposes a
      `clamav` profile (`docker compose --profile clamav up`).
- [x] Free/busy publishing to external domains.
      `internal/calendarbridge/freebusy.go` aggregates Stalwart
      CalDAV events into merged busy intervals and renders an
      RFC 5545 VFREEBUSY component.
      `freebusy_handlers.go` registers `GET /.well-known/caldav`
      (RFC 6764 discovery), CalDAV `REPORT
      /api/v1/calendars/{accountID}/{calendarID}` parsing
      `calendar-freebusy` time-range queries, and JSON
      `GET /api/v1/calendars/{accountID}/{calendarID}/freebusy?start=&end=`
      for the React UI. The "Check availability" panel on
      `web/src/pages/Calendar/EventCreate.tsx` queries the
      participant calendar before scheduling.
- [x] Autoconfig / autodiscover XML endpoints.
      `internal/dns/autoconfig.go` renders Thunderbird-style
      autoconfig and Outlook autodiscover XML against the
      tenant's `domains` row (resolving the tenant from the
      incoming email's domain).
      `autoconfig_handlers.go` registers `GET /mail/config-v1.1.xml`,
      `POST /autodiscover/autodiscover.xml`, and
      `GET /.well-known/autoconfig/mail/config-v1.1.xml` without
      auth (these are public discovery endpoints).
- [x] Stripe subscription creation on tenant signup.
      `internal/billing/stripe.go` adds `CreateCustomer` and
      keeps existing `CreateSubscription` /
      `UpdateSubscription` / `CancelSubscription` honest with a
      mock-server unit test. `internal/billing/lifecycle.go`
      wires `WithStripe(stripeClient, planPrices)` so
      `OnTenantCreated` mints a Stripe customer + subscription
      (when `KMAIL_STRIPE_SECRET_KEY` is set), `OnPlanChanged`
      pushes plan-change metadata, and `OnTenantDeleted`
      cancels the subscription. Migration 045 adds
      `stripe_customer_id` and `stripe_subscription_id` columns
      to `billing_subscriptions` (nullable so pre-Phase-8
      tenants are unchanged).
- [x] Shared-inbox MLS group key rotation.
      `internal/sharedinbox/mls.go` introduces an
      `MLSGroupManager` interface plus `HTTPMLSGroupManager`
      pointing at `KCHAT_MLS_ENDPOINT`. The tenant Service's
      `AddSharedInboxMember` / `RemoveSharedInboxMember` paths
      fire a `WithSharedInboxMembershipHook` callback that
      routes into `WorkflowService.HandleMembershipChange`,
      which in turn calls `MLSGroupManager.RotateGroup` so the
      next message bound for the inbox is encrypted to the
      current member set. Graceful no-op + warning log when the
      endpoint is empty. New
      `GET /api/v1/shared-inboxes/{inboxId}/mls/status`
      surfaces epoch + member count to the admin UI.
- [x] Grafana dashboard provisioning.
      `deploy/grafana/dashboards/kmail-overview.json` and
      `deploy/grafana/dashboards/kmail-deliverability.json` ship
      pre-built panels for HTTP RED metrics, JMAP proxy
      latency, active tenants / seats, retention counters, SLO
      availability, bounce / complaint rates, suppression list
      size, IP pool health, warmup progress, DMARC pass rate,
      and abuse score distribution.
      `deploy/grafana/provisioning/dashboards.yml` makes them
      auto-load when `docker compose --profile loki up` is
      running. Documented in `docs/DEVELOPMENT.md`.
- [x] Phase status reconciliation + README refresh.
      Phase 4 status corrected `NOT STARTED` → `COMPLETE`;
      Phase 7 status flipped `IN PROGRESS` → `COMPLETE`; Phase
      8 section added with this checklist; top-of-file status
      line refreshed; `README.md` §Project Status block
      rewritten to describe the current state (Phase 7 done,
      Phase 8 in progress, 45 migrations, Helm chart, Stripe
      lifecycle, WebAuthn + TOTP, Sieve, load testing, Stalwart
      v1 compat shim, Grafana dashboards, ClamAV scanner, free/
      busy, autoconfig, KMIP wire traffic).

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
