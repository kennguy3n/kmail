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
| CC6.2 | Tenant isolation | PostgreSQL RLS + per-tenant Stalwart shards + per-tenant zk-object-fabric buckets |
| CC6.3 | Privileged access auditing | `audit_log` chained writes for every admin route |
| CC6.6 | External boundary protection | TLS terminator, security middleware (`internal/middleware/security.go`) |
| CC6.7 | Restricted physical access | Inherited from cloud provider SOC 2 |
| CC6.8 | Malware controls | Stalwart antivirus integration |

### CC7.x — System Operations

| TSC | Control | Evidence |
|-----|---------|----------|
| CC7.1 | Capacity / availability | `internal/monitoring/` SLO tracker |
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
* Audit chain verification: `internal/audit/audit.go` `VerifyChain`
* Penetration test summary: shared on request under NDA
* Sub-processor changes: in-product changelog + email
