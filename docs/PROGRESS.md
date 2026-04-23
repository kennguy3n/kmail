# KMail — Progress

- **Project**: KMail — Privacy Email & Calendar for KChat B2B
- **License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).
- **Status**: Phase 1 — Foundation (in progress)
- **Last updated**: 2026-04-23 (initial docs scaffold: README, PROPOSAL,
  ARCHITECTURE, PROGRESS landed. Architecture locked on Stalwart mail
  core + Go control plane + React frontend + zk-object-fabric blob
  storage; MLS-to-KMail encryption key derivation model and privacy
  mode mapping documented.)

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

- [ ] Ratify architecture: Stalwart mail core + Go control plane +
      React frontend + zk-object-fabric blob storage.
- [ ] Evaluate Stalwart v0.16.0 — pin version, document breaking
      changes from earlier minor releases, plan the staging upgrade
      path to v1.0.0 (expected H1 2026).
- [ ] Define zk-object-fabric integration: configure Stalwart's blob
      store backend to use zk-object-fabric's S3 endpoint, define
      per-tenant bucket layout, pick `EncryptionMode` defaults per
      privacy tier, and wire content-addressing (BLAKE3) alignment.
- [ ] Define MLS ↔ KMail encryption key derivation model
      (confidential-send envelope keys, protected-folder master keys,
      shared-inbox group keys) and document in
      [ARCHITECTURE.md §5](ARCHITECTURE.md).
- [ ] Define privacy mode mapping: Standard Private Mail →
      `ManagedEncrypted`, Confidential Send → `StrictZK`, Zero-Access
      Vault → `StrictZK`; per-mode server-search scope.
- [ ] Define Go service boundaries (tenant, DNS onboarding, admin
      BFF, migration, chat bridge, calendar bridge, billing,
      deliverability, audit).
- [ ] Define JMAP-first client API contract (BFF → Stalwart JMAP
      shape, capability negotiation, push semantics).
- [ ] Define PostgreSQL schema for tenant metadata, users, domains,
      mailbox state, and calendar metadata.
- [ ] Define search tiering model (Core / Pro / Archive / Vault).
- [ ] Stalwart commercial license evaluation (AGPL-3.0 base vs
      enterprise dual license) and KMail licensing compatibility
      decision.
- [ ] Create Go project scaffold (`cmd/`, `internal/`, `api/`,
      `docs/`).
- [ ] Create React project scaffold for KChat Mail + Calendar UI.

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

---

## Phase 2 — Prototype (Weeks 5–10)

**Status**: `NOT STARTED`

**Goal**: a single-tenant prototype with custom-domain email, basic
calendar, JMAP webmail, IMAP/SMTP compatibility, and zk-object-fabric
blob storage wired end-to-end.

Checklist:

- [ ] Stalwart deployment with PostgreSQL metadata backend +
      zk-object-fabric blob store backend + Meilisearch search +
      Valkey state.
- [ ] Go API Gateway / BFF with KChat auth integration.
- [ ] Go Tenant Service (organizations, domains, users, aliases,
      shared inboxes, quotas).
- [ ] Go DNS Onboarding Service (MX / SPF / DKIM / DMARC checks,
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
