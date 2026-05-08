# KMail — Phase Overview

**License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).

> Status: Phases 1–5, 7–8 complete. Phase 6 — Enterprise Readiness
> (in progress; Exchange interop research and BIMI VMC issuance deferred).
> See [PROGRESS.md](PROGRESS.md) for the phase-gated tracker and the
> verbose changelog, and [PROPOSAL.md](PROPOSAL.md) for the technical
> design.

This document is the scannable, top-level view of KMail's eight build
phases. Each entry lists the goal, key deliverables, status, and the
decision-gate criteria that must be cleared before declaring the
phase complete. For implementation notes, file references, and the
running changelog, see [PROGRESS.md](PROGRESS.md).

---

## Phase 1 — Foundation

- **Timeline**: Weeks 1–4
- **Status**: `COMPLETE` (code) — external MLS review gate pending
- **Goal**: Stand up the repo skeleton, the Go control plane scaffold,
  the React frontend scaffold, the multi-tenant data model, and the
  MLS / zk-object-fabric integration design so Phase 2 can build on a
  reviewed foundation.

**Key deliverables**

- Repository scaffolding (Go module, Makefile, Dockerfile,
  docker-compose, migrations, README/PROPOSAL/ARCHITECTURE).
- PostgreSQL schema for tenants, users, domains, mailboxes,
  migrations, audit log, billing, suppression, IP pools.
- Stalwart bootstrap config (Postgres metadata, zk-object-fabric S3
  blob store, Meilisearch FTS, Valkey state) and per-shard HA
  template.
- Go BFF with OIDC dev-bypass middleware, JMAP proxy stub, tenant
  GUC + RLS plumbing.
- React frontend scaffold (Vite + TypeScript + React Router, MSW
  mock layer for screenshots).
- Documented MLS / zk-object-fabric integration model.

**Decision gate**

- External MLS-architecture review of the Confidential Send,
  Protected Folder, and Shared-Inbox key derivation paths signs off
  on the proposed key hierarchy before Phase 3 ships those features
  to a real tenant.

---

## Phase 2 — Prototype

- **Timeline**: Weeks 5–10
- **Status**: `COMPLETE`
- **Goal**: Ship a working email-and-calendar prototype with a
  KChat-themed React UI, JMAP through the Go BFF, third-party
  IMAP/SMTP/CalDAV/CardDAV compatibility, and a single-tenant dev
  compose stack that boots from `docker compose up`.

**Key deliverables**

- React Inbox + MessageView + Compose with JMAP `Email/get` /
  `Email/query` / `Email/set` / `EmailSubmission/set`.
- React CalendarView + EventCreate against Stalwart CalDAV via the
  BFF.
- ContactsView (vCard CRUD) with CardDAV.
- Tenant Service CRUD + DNS Onboarding (MX / SPF / DKIM / DMARC /
  MTA-STS / TLS-RPT / autoconfig records + verification).
- Migration Orchestrator (IMAP/Gmail import via imapsync workers).
- Chat Bridge (KChat → email and email → KChat notifications).
- Single-tenant compose stack: Postgres, Meilisearch, Valkey,
  zk-object-fabric, Stalwart, BFF, web.

**Decision gate**

- Compose stack boots clean and the e2e harness
  (`scripts/test-e2e.sh`, ten golden-path workflows) passes against
  a freshly-provisioned tenant.

---

## Phase 3 — Private Beta

- **Timeline**: Weeks 11–18
- **Status**: `COMPLETE` (code) — operational beta-onboarding gate pending
- **Goal**: Bring the platform to "deliverable to real domains"
  quality. Wire deliverability, billing, observability, and an
  admin console; then onboard 5–10 SMEs as private-beta customers
  to gather operational feedback before Phase 4's GA launch.

**Key deliverables**

- Deliverability control plane: IP pools + warmup, suppression,
  bounce, DMARC ingestion, FBL (Gmail Postmaster + Yahoo ARF),
  abuse scoring, send-rate limits.
- Attachment-to-link conversion (>10–15 MB → presigned
  zk-object-fabric URL with expiry / password / revocation).
- DNS wizard (admin UI walkthrough).
- Pooled storage quotas + shared inbox (no paid seats).
- Admin console: tenant / domain / user / quota / DMARC / audit
  pages; hash-chained audit log with verify + JSON/CSV export.
- Push notification service (transport-agnostic
  PushService → APNs / FCM / Web Push).
- Resource calendars + shared team calendars.
- Confidential Send mode (link-based portal; MLS key derivation
  stubs) + Billing/Quota Service + Observability (Prometheus + OTEL
  + structured JSON logs).

**Decision gate**

- 5–10 SMEs onboarded with positive deliverability and operational
  feedback (operational gate, not a code task).

---

## Phase 4 — Production SME Launch

- **Timeline**: Weeks 19–28
- **Status**: `COMPLETE`
- **Goal**: Production launch with published pricing tiers, a
  multi-shard Stalwart cluster, full deliverability infrastructure,
  and migration automation customers can self-serve.

**Key deliverables**

- Production multi-shard Stalwart with shard-aware JMAP proxy and
  shard-failover transport (circuit breaker + health worker).
- Per-tenant zk-object-fabric provisioning (bucket + credentials +
  placement policy).
- IP-reputation dashboards + automated deliverability alerts.
- Shared-mailbox workflows (assign / note / status).
- Calendar bridge: KChat scheduling / reminders / chat
  notifications + reminder worker (Valkey-deduped).
- Tenant-level billing integration with Stripe webhook stub +
  proration + lifecycle hooks.
- Published pricing: KChat Core Email ($3) / Pro ($6) / Privacy
  ($9), seat × per-seat-cents totals in admin.
- Migration automation: 3-step IMAP/Gmail wizard with
  pause/resume/cancel and a Test-connection probe.
- Availability target 99.9% with the SLO tracker (Valkey-backed
  rollup).

**Decision gate**

- Production multi-shard cluster sustains the 99.9% availability
  target; published pricing in market; first paid tenants self-serve
  through the migration wizard.

---

## Phase 5 — Privacy & Compliance Expansion

- **Timeline**: Post-launch (rolling)
- **Status**: `COMPLETE`
- **Goal**: Layer the advanced privacy features and the
  enterprise-grade compliance controls that turn the platform from
  "SME-launchable" into "regulated-customer-ready".

**Key deliverables**

- Zero-Access Vault folders (zk-object-fabric `StrictZK` + MLS-
  derived folder master key).
- Customer-managed keys (PEM upload, RSA-OAEP-256 by default,
  privacy-plan gated; HSM extensions handled in Phase 6).
- Regional storage controls (allow-list + encryption-mode pinning
  via the placement policy).
- Retention / archive tier (policy CRUD + 24h evaluation worker).
- Advanced export and eDiscovery (worker-pool-driven export jobs).
- Admin access approval workflow (per-action gating + executor
  registry + 7-day expiry).
- Protected folders (per-tenant share matrix + access log).
- SCIM 2.0 provisioning (`/scim/v2/Users` + `/scim/v2/Groups`,
  bearer tokens with SHA-256-hashed storage, RFC 7643 schemas).
- Reverse access proxy for SRE reads (session tracking + audit
  every hop).
- Compliance documentation pack (DPA, SOC 2 control mapping,
  Article 30 records, sub-processor list, security overview).
- Availability target 99.95%+ with a multi-region SLO aggregator
  and graceful read-fallback middleware.

**Decision gate**

- Zero-Access Vault, CMK, and SCIM are tenant-deployable; the
  compliance documentation pack passes legal review and is shipped
  with the customer-facing security overview.

---

## Phase 6 — Enterprise Readiness

- **Timeline**: Post-Phase 5 (rolling)
- **Status**: `IN PROGRESS` — 80% (8 of 10 items complete; 2 items
  intentionally deferred and tracked separately)
- **Goal**: Close the remaining gaps that block large-customer wins
  — real MLS for Confidential Send, BYOC HSM, SCIM conformance,
  webhook signing v2, retention live-mode default, GAL, BIMI, and a
  zero-touch onboarding checklist.

**Key deliverables**

- Real MLS group integration for Confidential Send (HTTPKeyDeriver
  against the KChat MLS service, status / wrap / rekey endpoints,
  graceful degrade when unconfigured).
- BYOC HSM for customer-managed keys (KMIP and PKCS#11 connection-
  metadata layer; real wire traffic ships in Phase 8).
- SCIM 2.0 conformance harness (`make scim-test`, discovery
  endpoints, `docs/SCIM_CONFORMANCE.md`).
- Webhook HMAC v2 signing (timestamp + nonce + versioned secret).
- Retention enforcement default flip (live by default; dry-run is
  opt-in via env flag) plus Prometheus counters.
- CardDAV global address list (tenant-scoped dedup, GAL search and
  sync endpoints).
- Reverse access proxy session-expiry watcher + Prometheus counter.
- Onboarding checklist auto-completion via webhook events
  (`email.received`, `domain.verified`, `user.created`).
- ContactsView completion (PHOTO / CATEGORIES, vCard
  import/export, contact groups, delete confirmation).
- Admin UI hardening across SCIM / Webhook / Onboarding.

**Decision gate**

- Real MLS Confidential Send and live-mode retention deploy cleanly
  to production; SCIM conformance results published; onboarding
  checklist auto-completes for at least one full new-tenant
  workflow without human intervention.

**Deferred (explicit do-not-do without re-decision)**

- Microsoft Exchange interop — research-only; produce a
  compatibility matrix before any build investment.
- BIMI VMC issuance helper / vendor partnership — no in-console
  purchase flow until the vendor relationship is signed.

---

## Phase 7 — Production Hardening

- **Timeline**: Post-Phase 5 (rolling)
- **Status**: `COMPLETE`
- **Goal**: Replace stubs with real transports, productionize the
  deployment, and prove the platform can survive load and chaos at
  scale.

**Key deliverables**

- Real APNs / FCM push transports with platform-aware
  TransportRouter (web / ios / android dispatch); dev fallthrough
  to the logging transport when credentials are absent.
- Stalwart v0 ↔ v1 compatibility shim with version detection,
  parallel `scripts/stalwart-init-v1.sh`, upgrade harness, runbook.
- Per-tenant search backend abstraction (Meilisearch / OpenSearch),
  reindex trigger, admin selector.
- Loki + Promtail log shipping behind a `loki` compose profile,
  Grafana datasource provisioning, structured JSON request logs.
- DKIM key rotation automation (history table, generate / rotate /
  revoke, admin UI).
- Kubernetes Helm chart (Deployment, Service, Ingress, ConfigMap,
  Secret, HPA, PDB) plus a Stalwart StatefulSet with stable
  per-pod hostnames; `make helm-lint`.
- Stripe billing completion (REST client + dunning service +
  customer portal endpoint, "Manage subscription" button).
- WebAuthn / FIDO2 credential management.
- Per-tenant Sieve rule management (CRUD + validate + deploy).
- Load testing (`load-jmap.go`, `load-smtp.sh`) + chaos harness
  (`chaos-shard.sh`, `chaos-postgres.sh`, `chaos-valkey.sh`),
  baselines documented in `docs/LOADTEST.md`.

**Decision gate**

- Helm chart deploys cleanly to a staging cluster; load harness
  meets the [Phase 4 SLO targets](PROGRESS.md#appendix-key-metrics-to-track);
  chaos suite shows graceful degradation on shard / Postgres /
  Valkey failure modes.

---

## Phase 8 — GA Readiness

- **Timeline**: Post-Phase 7 (rolling)
- **Status**: `COMPLETE`
- **Goal**: Final GA push — close every "stub" path, ship the last
  customer-visible compliance and discovery features, and reconcile
  documentation against the shipped code.

**Key deliverables**

- Real KMIP / PKCS#11 wire traffic for HSM CMK envelope ops
  (KMIP 1.4 TTLV encoder/decoder + TLS transport; PKCS#11 path
  behind a `pkcs11` build tag); `last_used_at` telemetry.
- RFC 8030 Web Push transport with VAPID JWT signing (P-256 / ES256).
- TOTP fallback for WebAuthn (RFC 6238, recovery codes,
  `KMAIL_SECRETS_KEY`-wrapped secrets).
- ClamAV malware-scanning adapter wired as a JMAP submit-time
  pre-delivery hook (`KMAIL_CLAMAV_ADDR`, optional `clamav`
  compose profile).
- Free/busy publishing (RFC 5545 VFREEBUSY + RFC 6764 discovery,
  CalDAV REPORT, JSON `freebusy` endpoint, "Check availability" UI).
- Autoconfig / autodiscover XML endpoints (Thunderbird + Outlook
  shapes; tenant-aware via the email's domain).
- Stripe subscription creation on tenant signup
  (`stripe_customer_id` / `stripe_subscription_id` columns,
  lifecycle hooks).
- Shared-inbox MLS group key rotation
  (`HTTPMLSGroupManager`, `RotateGroup` on add/remove member).
- Pre-built Grafana dashboards: KMail Overview + Deliverability
  (auto-loaded under the `loki` compose profile).
- Documentation reconciliation: phase status corrections, README
  refresh, top-of-file PROGRESS status line refined.

**Decision gate**

- All ten GA-readiness items merged; documentation matches shipped
  code; CI green on Go (`make vet` / `make build` / `make test`
  with `-race`) and the React app (`npm run build`).

---

## Cross-cutting tracking

- **Migrations**: 045 numbered, additive-only PostgreSQL migrations,
  all RLS-scoped on `tenant_id`. See
  [PROGRESS.md](PROGRESS.md) for the per-migration ledger.
- **Internal Go packages**: 28 packages under `internal/` covering
  the BFF, tenant, auth, JMAP, deliverability, billing, audit,
  search, scim, vault, cmk, retention, and operational concerns.
  See [ARCHITECTURE.md §7](ARCHITECTURE.md#7-go-service-topology)
  for the full topology.
- **Frontend pages**: a complete admin surface (domain, user,
  security, billing, retention, search, Sieve, DKIM, webhooks,
  onboarding) plus the Mail / Calendar / Contacts end-user surfaces.
- **CI**: GitHub Actions (`.github/workflows/ci.yml`) runs
  `make tidy / vet / build / test` on every push and PR with Go
  1.25.x (`-race` enabled), and `npm run build` for the React app.
- **Metrics targets**: tracked against the SLO baselines in
  [PROGRESS.md Appendix: Key Metrics to Track](PROGRESS.md#appendix-key-metrics-to-track).
