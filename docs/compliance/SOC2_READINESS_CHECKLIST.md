# SOC 2 Type II Readiness Checklist

A pre-audit self-assessment for KMail. A SOC 2 **Type II** audit
evaluates whether controls operated *effectively over an
observation window* (typically 6–12 months), so most rows require
not just a control but **dated, recurring evidence** across the
window. Pair this with the control catalogue in
[`SOC2_CONTROL_MAPPING.md`](./SOC2_CONTROL_MAPPING.md) and the
collection automation in
[`../../scripts/compliance/generate-evidence.sh`](../../scripts/compliance/generate-evidence.sh).

Status key: `[x]` in place · `[~]` partial / in progress · `[ ]` not started.

## 1. Scope & pre-work
- [x] Define system boundary (BFF API, PostgreSQL, per-tenant Stalwart shards, zk-object-fabric) — see [`SECURITY_OVERVIEW.md`](./SECURITY_OVERVIEW.md) §1.
- [x] Select Trust Service Criteria: Security (CC), Availability (A1), Confidentiality (C1), Processing Integrity (PI1), Privacy (P1–P8).
- [x] Maintain a control catalogue mapped to code/config — [`SOC2_CONTROL_MAPPING.md`](./SOC2_CONTROL_MAPPING.md).
- [ ] Engage an auditor and agree the observation window start date.
- [ ] Complete a Type I readiness review (point-in-time design) before the Type II window opens.

## 2. Security (Common Criteria)
- [x] **CC6.1 Authentication** — OIDC via KChat, fail-closed (`internal/middleware/auth.go`, `oidc.go`).
- [x] **CC6.1 MFA** — TOTP second factor with per-account brute-force lockout (`internal/middleware/totp.go`, migration 010); WebAuthn available (`internal/middleware/webauthn.go`).
- [x] **CC6.2 Tenant isolation** — PostgreSQL RLS **forced** on every tenant-scoped table (migration 008), per-tenant Stalwart shards, per-tenant zk-object-fabric buckets. Continuously regression-tested (`internal/middleware/rls_db_test.go`).
- [x] **CC6.6 Boundary protection** — security headers + CORS allow-list + restrictive CSP (`internal/middleware/security.go`); TLS 1.2+ externally, mTLS to Stalwart.
- [x] **CC6.3 Privileged-access auditing** — every admin route writes to the `audit_log` hash chain; chain integrity is tamper-evident and concurrency-safe (migrations 009/011, advisory lock).
- [x] **CC7.1 Vulnerability management** — automated dependency scanners (govulncheck / npm audit / cargo audit) + Dependabot; findings tracked in [`SECURITY_FINDINGS.md`](./SECURITY_FINDINGS.md).
- [x] **CC7.2 Incident response** — [`incident-response.md`](./incident-response.md).
- [x] **CC8.1 Change management** — branch protection on `main` (PR + review + required `CI Status`), numbered migrations, CODEOWNERS on sensitive paths. **Deploy ordering:** schema migrations run to completion (`cmd/kmail-migrate`) **before** any new API/worker pod is rolled — the audit `seq` column (migration 011) and the TOTP lockout columns (migration 010) must exist before the code that reads them goes live.
- [~] **CC7.3 Threat detection** — deliverability + audit-chain verification in place; centralised SIEM/alerting integration to be confirmed.
- [ ] **CC1.x / CC4.x** — HR controls (background checks, security training, org chart) and periodic management review: confirm operational evidence exists for the window.

## 3. Availability
- [x] **A1.1** Capacity & SLO monitoring (`internal/monitoring/`, Prometheus).
- [x] **A1.2** Backups — daily full + 5-min WAL (PITR); zk-object-fabric versioning.
- [~] **A1.3** Recovery testing — quarterly restore drill *runbook* exists; capture a dated drill record each quarter of the window.

## 4. Confidentiality
- [x] **C1.1** Encryption at rest — zk-object-fabric AES-256-GCM envelope; optional customer CMK (`internal/cmk/`).
- [x] **C1.2** Encryption in transit — TLS 1.2+ on every external endpoint; internal mTLS.
- [x] Secret management — single master key (`KMAIL_SECRETS_KEY`) envelope-wraps BFF at-rest secrets (`internal/secrets/`, `internal/cmk/envelope.go`).

## 5. Processing Integrity
- [x] **PI1.1** Input validation at the service layer in every handler.
- [x] **PI1.4** Server-authoritative identifiers (JMAP/CardDAV/CalDAV); round-trip preservation tests.

## 6. Privacy / GDPR
- [x] Article 30 records — [`DATA_PROCESSING_RECORDS.md`](./DATA_PROCESSING_RECORDS.md).
- [x] DPA template — [`DPA.md`](./DPA.md).
- [x] Sub-processor register — [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) and vendor review [`vendors.md`](./vendors.md).
- [x] Data-subject rights — export API + retention/disposal worker (`internal/export/`, `internal/retention/`).
- [ ] Confirm a documented breach-notification timeline (72h GDPR) is wired into the incident runbook with named owners.

## 7. Evidence collection (the Type II differentiator)
- [x] Automation outline + script — `generate-evidence.sh` collects audit-chain, access-review, change-log, vendor-register, and dependency-scan artifacts into a dated, manifested bundle.
- [x] Evidence cadence table — [`SOC2_CONTROL_MAPPING.md`](./SOC2_CONTROL_MAPPING.md) §"Evidence Collection Procedures".
- [~] Schedule the recurring collectors (monthly audit-chain, quarterly access-review) as cron/CI jobs and retain bundles for the full window.
- [ ] Centralise evidence retention (immutable store) with access logging.

## 8. Penetration testing
- [x] Threat model — [`THREAT_MODEL.md`](./THREAT_MODEL.md).
- [x] API endpoint + auth inventory — [`API_ENDPOINTS.md`](./API_ENDPOINTS.md).
- [~] External pentest — summary shared under NDA ([`../SECURITY_TESTING.md`](../SECURITY_TESTING.md)); schedule a test against the current build and track remediations here.

## How to use this checklist
1. Drive every `[ ]` / `[~]` to `[x]` **with evidence**, not just a control.
2. Run `generate-evidence.sh` on the documented cadence; archive each bundle.
3. Re-review before the Type I assessment and again at the Type II window close.
