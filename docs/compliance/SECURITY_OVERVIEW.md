# KMail Security Overview

This is the customer-facing security whitepaper for KMail, the
privacy-centric email + calendar service for KChat. It explains
the encryption architecture, tenant isolation model, key
management, audit logging, and incident response process.

## 1. Architecture Snapshot

KMail is a multi-tenant SaaS sitting in front of a per-tenant
Stalwart Mail Server v0.16.0 shard. The control plane (auth,
tenant config, audit, billing) is a Go service ("BFF") backed
by PostgreSQL. Mail bodies and attachments are stored in
**zk-object-fabric**, KMail's encrypted blob layer fronting an
S3-compatible storage gateway. See
[`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) for the full
deployment topology.

## 2. Tenant Isolation

* **Database:** PostgreSQL with row-level security (RLS) on every
  multi-tenant table. Every transaction sets the
  `app.tenant_id` GUC; RLS policies enforce
  `tenant_id = current_setting('app.tenant_id')::uuid`.
* **Mail data plane:** Each tenant has a dedicated Stalwart
  shard URL resolved at request time
  (`tenant.ShardService.GetTenantShard`).
* **Storage:** Per-tenant zk-object-fabric bucket provisioned at
  signup (`internal/tenant/zkfabric.go`). Cross-tenant blob
  access is impossible at the gateway layer.
* **Identity:** OIDC subject from KChat is mapped to a tenant +
  user via the existing `users` table; SCIM tokens are
  tenant-scoped (`scim_tokens` with RLS).

## 3. Encryption

| Surface | At rest | In transit |
|---------|---------|------------|
| Mail bodies + attachments | zk-object-fabric envelope (AES-256-GCM); Privacy plan can BYO CMK | TLS 1.2+ to clients, internal mTLS |
| Mail metadata (headers, labels) | PostgreSQL TDE on managed instance | TLS 1.2+ |
| Calendar / contacts | Same as mail data plane | TLS 1.2+ |
| Audit log | PostgreSQL TDE; row-level integrity via SHA-256 hash chain | TLS 1.2+ |

### Privacy modes

KMail supports three privacy modes per mailbox:

* **Standard Private Mail** — the default; KMail-managed
  encryption keys (`ManagedEncrypted`).
* **Confidential Send** — short-lived, link-only delivery via the
  public confidential-send portal (`PublicDistribution`).
* **Zero-Access Vault** — opt-in vault folders where KMail does
  not hold the decryption key (`StrictZK`); only the customer
  can decrypt. Every mailbox is **not** zero-access by default
  — zero-access is a deliberate per-folder opt-in.

## 4. Key Management

* zk-object-fabric envelope keys are managed by KMail by
  default and rotated quarterly.
* Privacy-plan tenants may bring their own master key (CMK)
  that wraps the zk-object-fabric envelope keys
  (`internal/cmk/`). Revoking a CMK renders blobs encrypted
  under that key unrecoverable.
* OIDC + SCIM tokens are stored hashed; plaintext is returned
  only at issuance.

## 5. Authentication and Authorisation

* End users authenticate with OIDC against the customer's
  KChat instance; KMail validates the token and resolves
  tenant + user.
* Admin operations are audited (`audit_log`) with the
  authenticating actor's identity, IP, and user agent.
* Sensitive admin actions (`user_delete`, `domain_remove`,
  `data_export`, `plan_downgrade`, `retention_policy_change`)
  are gated by the customer's approval policy
  (`internal/approval/`).
* SRE / support access to tenant data flows through the
  **Reverse Access Proxy** (`internal/adminproxy/`) which
  requires a customer-side approval and issues a time-bounded
  session.

## 6. Audit Logging

Every administrative action writes a row to `audit_log` with
`prev_hash` + `entry_hash` forming a tenant-scoped SHA-256 hash
chain. Customers can verify the chain at any time via
`POST /api/v1/audit/verify`. Tampering with the audit log is
detectable cryptographically.

## 7. Rate Limiting and Abuse Prevention

* Per-tenant + per-user rate limiting backed by Valkey
  (`internal/middleware/rate_limit.go`).
* Bounce and complaint signals from upstream MTAs flow into the
  deliverability service (`internal/deliverability/`) and feed
  the suppression list.
* DMARC + SPF + DKIM are mandatory for sending domains; BIMI
  records are emitted by the DNS wizard for tenants who want
  brand-indicator support.

## 8. Backup and Disaster Recovery

* PostgreSQL: daily full + WAL archived every 5 minutes; PITR
  window 35 days; monthly snapshots retained 1 year.
* Mail bodies: zk-object-fabric is multi-AZ replicated with
  versioning enabled.
* Quarterly DR drills validate restore time objectives.

## 9. Vulnerability Management

* Mandatory PR-time CI gates: `go vet`, `go build`, `go test
  -race`, frontend `npm run typecheck` and `npm run build`.
* Renovate / Dependabot covers Go modules and npm packages.
* Annual third-party penetration test; remediation tracked in
  the internal risk register.

## 10. Incident Response

KMail follows a documented incident response runbook with
24/7 on-call. Customers are notified of any personal data
breach affecting their tenant within 72 hours per the DPA.

## 11. Compliance Programme

* SOC 2 Type II audit underway; control mapping in
  [`SOC2_CONTROL_MAPPING.md`](./SOC2_CONTROL_MAPPING.md).
* GDPR Article 30 records in
  [`DATA_PROCESSING_RECORDS.md`](./DATA_PROCESSING_RECORDS.md).
* Data Processing Agreement template in [`DPA.md`](./DPA.md).
* Sub-processor list in [`SUBPROCESSORS.md`](./SUBPROCESSORS.md).
