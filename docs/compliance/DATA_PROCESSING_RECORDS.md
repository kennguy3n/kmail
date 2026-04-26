# Article 30 Records of Processing Activities

This document is the GDPR Article 30(2) record KMail maintains
as a processor.

## 1. Controller and Processor

* **Processor:** KMail (operated by KChat).
* **Controller:** Each KMail tenant is a controller for the
  personal data they upload, send, or receive through the
  service.
* **Data Protection Officer:** dpo@kmail.example (placeholder —
  set in production via `KMAIL_DNS_REPORTING_MAILBOX`).

## 2. Categories of Processing

| ID | Activity | Purpose | Legal Basis |
|----|----------|---------|-------------|
| P-01 | Email send / receive | Provide messaging service | Contract (Art. 6(1)(b)) |
| P-02 | Mailbox storage | Provide messaging service | Contract |
| P-03 | Calendar / contact storage | Provide collaboration service | Contract |
| P-04 | Audit logging | Security, regulatory compliance | Legal obligation + legitimate interest |
| P-05 | Operational metrics | Service availability | Legitimate interest |
| P-06 | Billing | Invoicing, plan management | Contract |
| P-07 | Bounce / complaint tracking | Deliverability | Legitimate interest |

## 3. Categories of Data Subjects

* Tenant employees, contractors, authorised users
* External email correspondents
* Calendar attendees
* Address book contacts (CardDAV)

## 4. Categories of Personal Data

| Category | Examples |
|----------|----------|
| Identity | Email address, display name, OIDC subject |
| Mailbox content | Message bodies, headers, attachments |
| Calendar | iCalendar events, attendees, locations |
| Contacts | vCard FN / EMAIL / TEL / ORG / NOTE |
| Network metadata | IP addresses, user agent, request IDs |
| Audit metadata | Action, actor, target resource, timestamp, hash chain |

No special categories (Art. 9) are processed by the platform;
tenants who upload such data are responsible for their own
lawful basis.

## 5. Recipients of Personal Data

| Recipient | Purpose | Reference |
|-----------|---------|-----------|
| Stalwart Mail Server | Mail delivery + JMAP | [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) |
| zk-object-fabric (Wasabi) | Encrypted blob storage | [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) |
| PostgreSQL | Control-plane state | [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) |
| Meilisearch | Mailbox search index | [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) |
| Valkey | Rate-limit + cache | [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) |

## 6. International Transfers

EU/UK personal data stays in EEA Stalwart shards by default.
US-region tenants may opt into a US placement at signup.
Transfers between regions occur only on tenant request (export
runner) with the Standard Contractual Clauses where applicable.

## 7. Retention Periods

| Data | Default Retention | Configurable? |
|------|-------------------|---------------|
| Mailbox content | Indefinite | Yes — tenant retention policies (Phase 5 enforcement) |
| Calendar / contact | Indefinite | Yes — tenant retention policies |
| Audit log | 7 years | No — regulatory minimum |
| Bounce / complaint events | 90 days | No |
| Rate-limit counters | 24 hours | No |
| Backups | 35 days rolling + monthly snapshot | No |

## 8. Technical and Organisational Measures

See [`SOC2_CONTROL_MAPPING.md`](./SOC2_CONTROL_MAPPING.md) and
[`SECURITY_OVERVIEW.md`](./SECURITY_OVERVIEW.md) for the full
list of controls. Highlights:

* Encryption at rest (zk-object-fabric envelopes; Privacy-plan
  customer-managed keys)
* TLS 1.2+ in transit
* Tenant isolation via PostgreSQL RLS + per-tenant blob bucket
  + per-tenant Stalwart shard
* SHA-256 hash-chained audit log
* Approval-gated reverse access proxy for support operations
* Rate limiting via Valkey
* Mandatory CI gates: `go vet`, `go build`, `go test -race`,
  frontend type-check
