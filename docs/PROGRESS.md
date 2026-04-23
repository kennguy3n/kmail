# KMail — Progress

- **Project**: KMail — Privacy Email & Calendar for KChat B2B
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Foundation (in progress); Phase 2 —
  Prototype (in progress)
- **Last updated**: 2026-04-23 — Phase 2 engineering work has
  begun. This update lands three pieces of the Phase 2 checklist:
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

Delivered in this batch:

- Full **Tenant CRUD** — list / update / delete for tenants and
  users, all RLS-scoped where applicable; matching HTTP routes
  under `/api/v1/tenants/...`.
- **DNS Onboarding Service** — MX / SPF / DKIM / DMARC
  verification, `GenerateRecords` helper for the DNS wizard,
  mockable resolver interface for unit testing; mounted
  in-process by `cmd/kmail-api` and available as a standalone
  binary at `cmd/kmail-dns`.
- **Stalwart v0.16.0 automated bootstrap** — JSON bootstrap at
  `configs/stalwart-bootstrap.json` + admin-API init script at
  `scripts/stalwart-init.sh`, wired into `docker-compose.yml` as
  a `stalwart-init` one-shot so `docker compose up` is now
  hands-off (no manual setup wizard).

Checklist:

- [x] Stalwart deployment with PostgreSQL metadata backend +
      zk-object-fabric blob store backend + Meilisearch search +
      Valkey state. _(compose wiring + automated bootstrap;
      production wiring swaps the MinIO blob mock for the real
      zk-object-fabric gateway.)_
- [ ] Go API Gateway / BFF with KChat auth integration.
- [x] Go Tenant Service (organizations, domains, users, aliases,
      shared inboxes, quotas). _(full CRUD, RLS-scoped.)_
- [x] Go DNS Onboarding Service (MX / SPF / DKIM / DMARC checks,
      domain verification).
- [ ] React KChat Mail UI (inbox, compose, read, search).
- [ ] React KChat Calendar UI (personal calendar, event create /
      edit, RSVP).
- [ ] JMAP client integration (web app → Go BFF → Stalwart JMAP).
- [ ] IMAP / SMTP compatibility testing (Thunderbird, Apple Mail).
- [ ] CalDAV compatibility testing.
- [ ] Basic spam / phishing filtering via Stalwart.
- [ ] Gmail / IMAP migration orchestrator (Go + imapsync).
- [ ] Email-to-chat bridge (share email to KChat channel).
- [ ] zk-object-fabric blob store integration verified end-to-end
      (PUT / GET mail blobs, attachment storage, presigned
      attachment links).
- [ ] Benchmark: inbox open P95 < 250 ms (warm), message open
      P95 < 300 ms, send accepted P99 < 1 s.

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
- [ ] Admin console (React) — tenant management, domain management,
      user management, quota management.
- [ ] Admin audit logs.
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
