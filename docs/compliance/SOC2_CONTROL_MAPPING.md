# SOC 2 Trust Service Criteria — KMail Control Mapping

This document maps KMail's existing technical controls onto the
SOC 2 Trust Service Criteria (TSC). It is intended as a reviewer
checklist: every TSC has at least one named control with a
pointer to the implementing code, configuration, or operational
runbook.

## Common Criteria

### CC1.x — Control Environment

| TSC | Control | Evidence |
|-----|---------|----------|
| CC1.1 | Code of conduct, security training | Internal HR runbook |
| CC1.4 | Hiring + background checks | HR runbook |
| CC1.5 | Documented org chart and accountability | This repository's `CODEOWNERS` |

### CC2.x — Communication and Information

| TSC | Control | Evidence |
|-----|---------|----------|
| CC2.1 | Internal change communication | Pull-request reviews on every change |
| CC2.2 | Customer-facing security overview | [`SECURITY_OVERVIEW.md`](./SECURITY_OVERVIEW.md) |
| CC2.3 | Status communication | Status page + audit log surfacing in admin UI |

### CC3.x — Risk Assessment

| TSC | Control | Evidence |
|-----|---------|----------|
| CC3.1 | Annual risk assessment | Internal risk register |
| CC3.2 | Vendor / sub-processor evaluation | [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) |

### CC4.x — Monitoring

| TSC | Control | Evidence |
|-----|---------|----------|
| CC4.1 | Continuous monitoring | OpenTelemetry traces, Prometheus metrics, structured logs |
| CC4.2 | Audit-log integrity | `internal/audit/audit.go` SHA-256 hash chain |

### CC5.x — Control Activities

| TSC | Control | Evidence |
|-----|---------|----------|
| CC5.1 | Approval workflow for sensitive ops | `internal/approval/` |
| CC5.2 | Reverse access proxy with admin approval | `internal/adminproxy/` |

### CC6.x — Logical and Physical Access

| TSC | Control | Evidence |
|-----|---------|----------|
| CC6.1 | OIDC auth via KChat | `internal/middleware/oidc.go` |
| CC6.1 | MFA: TOTP with per-account brute-force lockout; WebAuthn | `internal/middleware/totp.go` (migration 010), `internal/middleware/webauthn.go` |
| CC6.2 | Tenant isolation | PostgreSQL RLS (FORCED on every tenant table, migration 008) + per-tenant Stalwart shards + per-tenant zk-object-fabric buckets; regression-tested in `internal/middleware/rls_db_test.go` |
| CC6.3 | Privileged access auditing | `audit_log` chained writes for every admin route |
| CC6.6 | External boundary protection | TLS terminator, security middleware (`internal/middleware/security.go`) |
| CC6.7 | Restricted physical access | Inherited from cloud provider SOC 2 |
| CC6.8 | Malware controls | Stalwart antivirus integration |

### CC7.x — System Operations

| TSC | Control | Evidence |
|-----|---------|----------|
| CC7.1 | Capacity / availability | `internal/monitoring/` SLO tracker |
| CC7.1 | Dependency vulnerability scanning | `.github/workflows/security-scan.yml` (govulncheck / npm audit / cargo audit) + `.github/renovate.json` (automated update + vulnerability-alert PRs); findings in [`SECURITY_FINDINGS.md`](./SECURITY_FINDINGS.md) |
| CC7.2 | Incident response | Pager runbook + audit log |
| CC7.3 | Detection of security events | `internal/deliverability/` + audit chain verification |
| CC7.4 | Recovery from incidents | DB PITR, zk-object-fabric versioning |

### CC8.x — Change Management

| TSC | Control | Evidence |
|-----|---------|----------|
| CC8.1 | Authorised changes | GitHub PR + required reviewer + CI |
| CC8.2 | Migration management | Numbered SQL migrations under `migrations/` |

### CC9.x — Risk Mitigation

| TSC | Control | Evidence |
|-----|---------|----------|
| CC9.1 | Vendor risk | DPA + sub-processor list |
| CC9.2 | Disaster recovery | DB backups, multi-AZ deployment |

## Availability

| TSC | Control | Evidence |
|-----|---------|----------|
| A1.1 | Capacity monitoring | Prometheus metrics, SLO tracker |
| A1.2 | Backup + restoration | Daily full + 5-min WAL backup |
| A1.3 | Recovery testing | Quarterly restore drills |

## Confidentiality

| TSC | Control | Evidence |
|-----|---------|----------|
| C1.1 | Encryption at rest | zk-object-fabric envelopes + Privacy-plan CMK |
| C1.2 | Encryption in transit | TLS 1.2+ on every external endpoint |

## Processing Integrity

| TSC | Control | Evidence |
|-----|---------|----------|
| PI1.1 | Input validation | Service-layer validation in every handler |
| PI1.4 | Output completeness | JMAP / CardDAV / CalDAV protocols enforce server-side identifiers |

## Privacy

| TSC | Control | Evidence |
|-----|---------|----------|
| P1.x | Notice + consent | DPA + privacy notice |
| P2.x | Choice / consent | Per-tenant retention + privacy mode (Standard / Confidential / Zero-Access) |
| P3.x | Collection | Documented in [`DATA_PROCESSING_RECORDS.md`](./DATA_PROCESSING_RECORDS.md) |
| P4.x | Use, retention, disposal | `internal/retention/` + Phase 5 enforcement worker |
| P5.x | Access | Export API, JMAP, audit log |
| P6.x | Disclosure to third parties | Sub-processor list |
| P7.x | Quality | Round-trip vCard / iCal / RFC 5322 preservation |
| P8.x | Monitoring + enforcement | Audit log + reverse access proxy |

## Audit Evidence Locations

* CI logs: GitHub Actions `Build` workflow per PR
* Audit chain verification: `internal/audit/audit.go` `VerifyChain` (concurrency-safe: per-tenant advisory lock + monotonic `seq`, migrations 009/011)
* Threat model: [`THREAT_MODEL.md`](./THREAT_MODEL.md); endpoint + auth inventory: [`API_ENDPOINTS.md`](./API_ENDPOINTS.md)
* SOC 2 readiness self-assessment: [`SOC2_READINESS_CHECKLIST.md`](./SOC2_READINESS_CHECKLIST.md)
* Dependency-scan findings tracker: [`SECURITY_FINDINGS.md`](./SECURITY_FINDINGS.md)
* Penetration test summary: shared on request under NDA — see [`../SECURITY_TESTING.md`](../SECURITY_TESTING.md)
* Sub-processor changes: in-product changelog + email

## Evidence Collection Procedures

A SOC 2 Type II audit covers an *observation window* (typically 6–12
months), so each control needs **periodic, dated evidence** rather
than a one-time snapshot. The table below is the collection plan;
`scripts/compliance/generate-evidence.sh` automates the machine-
collectable rows into a single timestamped bundle.

| Control | Evidence artifact | Cadence | Owner | Automated by |
|---------|-------------------|---------|-------|--------------|
| CC4.2 / CC6.3 | Audit-log hash-chain verification output | Monthly | Security | `generate-evidence.sh --audit` |
| CC7.1 / CC9.2 | Dependency vulnerability scan output (Go/npm/cargo) | Per release + weekly | Security | `generate-evidence.sh --deps` |
| CC6.1 / CC6.2 | Quarterly user access review (per-tenant role dump) | Quarterly | Security + tenant admins | `generate-evidence.sh --access-review` |
| CC8.1 | Change log: merged PRs with reviewer + CI status | Per release | Eng leads | `generate-evidence.sh --change-log` |
| CC7.2 | Incident postmortems | Per incident | On-call | [`incident-response.md`](./incident-response.md) |
| CC9.1 / CC3.2 | Vendor / sub-processor review register | Quarterly | Security | [`vendors.md`](./vendors.md) |
| A1.3 | DB restore-drill record | Quarterly | SRE | Manual runbook |
| C1.1 | Encryption-at-rest config snapshot (envelope wired, CMK status) | Per release | Eng | `generate-evidence.sh --change-log` (boot log capture) |

### Continuous control monitoring

The following controls are monitored continuously rather than
sampled:

* **Access reviews** — the quarterly review is a *point-in-time
  attestation*, but role grants are continuously audit-logged
  (`audit_log` chained writes on every admin route), so the
  quarterly report is a reconciliation, not a discovery.
* **Change management** — branch protection on `main` requires a PR
  with at least one approving review and green CI before merge. This
  is enforced in the platform (GitHub branch protection) and in CI;
  see "Change management enforcement" below.
* **Incident response** — see [`incident-response.md`](./incident-response.md).
* **Vendor management** — see [`vendors.md`](./vendors.md).

### Change management enforcement

CC8.1 ("authorised changes") is enforced, not just documented:

1. **Branch protection** on `main` — no direct pushes; PR + review +
   passing required status checks before merge.
2. **CI gate** — the `Build` workflow runs `go build`, `go vet`,
   `go test -race`, the web lint/typecheck/test suite, and Helm lint.
3. **CODEOWNERS** — sensitive paths (auth, crypto, migrations) route
   review to the owning team.

The change-log evidence (merged PRs with reviewer + CI conclusion)
is exported by `generate-evidence.sh --change-log` using the GitHub
CLI when available.
