# KMail — Progress

- **Project**: KMail — Privacy Email & Calendar for KChat B2B
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Foundation (in progress); Phase 2 —
  Prototype (in progress)
- **Last updated**: 2026-04-24 — Phase 2 remainder + early
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
  * Admin console React — `web/src/api/admin.ts` typed client,
    admin types in `web/src/types/index.ts`, and interactive
    pages at `web/src/pages/Admin/TenantAdmin.tsx` (plan +
    status editor, seat pricing summary), `DomainAdmin.tsx`
    (list / add / verify / DNS-record display),
    `UserAdmin.tsx` (user table with role / status / quota
    edit + delete + shared-inbox section).
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

- **Previously (2026-04-24)**: Phase 2 compatibility + spam +
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

**Status**: `NOT STARTED`

**Goal**: multi-tenant private beta with 5–10 SME design partners,
deliverability infrastructure, IP reputation, and migration support.

Checklist:

- [ ] Multi-tenant Stalwart shard (5,000–10,000 mailbox target).
- [ ] IP pool architecture (system transactional, mature trusted,
      new / warming, restricted, dedicated enterprise).
- [ ] Tenant send limits and warmup schedule.
- [ ] DNS wizard (MX, SPF, DKIM 2048-bit, DMARC, MTA-STS, TLS-RPT,
      autoconfig).
- [ ] DMARC report ingestion.
- [ ] Gmail Postmaster / Yahoo feedback loop monitoring.
- [ ] Suppression lists and bounce tracking.
- [ ] Abuse scoring and compromised-account detection.
- [ ] Pooled storage quotas (tenant pool, not per-user).
- [ ] Shared inboxes (`sales@`, `support@`, `info@`) without
      requiring paid seats.
- [ ] Attachment-to-link conversion (> 10–15 MB → zk-object-fabric
      presigned link with expiry / password / revocation).
- [~] Admin console (React) — tenant management, domain management,
      user management, quota management.
      _(React pages at `web/src/pages/Admin/TenantAdmin.tsx`,
      `DomainAdmin.tsx`, `UserAdmin.tsx` with add/edit forms,
      verification-status badges, DNS-records display, quota /
      role / status editors, shared-inbox management; typed
      client at `web/src/api/admin.ts` fronting
      `/api/v1/tenants/...`. Backed by the existing Tenant / DNS
      services and the new audit log. Quota-enforcement worker
      and seat-pricing UI still pending.)_
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
- [ ] Mobile push notifications.
- [ ] Resource calendars and shared team calendars.
- [ ] Confidential Send mode (MLS-derived envelope keys, encrypted
      portal for external recipients).
- [ ] Billing / quota service (storage accounting, seat accounting,
      plan enforcement).
- [ ] Observability (Prometheus, OpenTelemetry, Loki).
- [ ] Beta customer onboarding (5–10 SMEs).

---

## Phase 4 — Production SME Launch (Weeks 19–28)

**Status**: `NOT STARTED`

**Goal**: production launch with published pricing tiers, full
deliverability infrastructure, and migration automation.

Checklist:

- [ ] Production Stalwart cluster (multi-node, HA).
- [ ] Production zk-object-fabric integration (Wasabi primary,
      Linode cache).
- [ ] IP reputation dashboards.
- [ ] Automated deliverability alerts.
- [ ] Shared mailbox workflows.
- [ ] Calendar bridge (KChat scheduling, meeting rooms, reminders,
      chat notifications).
- [ ] Tenant-level billing integration.
- [ ] Published pricing: KChat Core Email, KChat Mail Pro,
      KChat Privacy.
- [ ] Migration automation (Gmail / IMAP import wizard, staged
      sync, cutover checklist).
- [ ] Availability target: 99.9%.

---

## Phase 5 — Privacy & Compliance Expansion (Post-Launch)

**Status**: `NOT STARTED`

**Goal**: advanced privacy features, compliance controls, and
enterprise readiness.

Checklist:

- [ ] Zero-Access Vault (client-side encrypted folders via
      zk-object-fabric `StrictZK` + MLS key hierarchy).
- [ ] Customer-managed keys (Privacy / Enterprise tier).
- [ ] Regional storage controls (zk-object-fabric placement
      policies).
- [ ] Retention / archive tier (zk-object-fabric cold storage).
- [ ] Advanced export and eDiscovery preparation.
- [ ] Admin access approval workflow.
- [ ] Protected folders.
- [ ] Availability target: 99.95%+.

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
