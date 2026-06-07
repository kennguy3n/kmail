---
title: Security & privacy overview
description: How KMail protects your mail — tenant isolation, encryption at rest and in transit, key management, authentication, and audit logging.
category: Security
order: 5
updated: 2026-06-06
---

KMail is private business email built so that your data stays yours.
This page summarises the protections that apply to every account. For
the auditor-facing detail (control mappings, sub-processors, DPA), see
the compliance documents linked at the end.

## Tenant isolation

Every tenant's data is isolated. Control-plane records are partitioned
per tenant and enforced in PostgreSQL with **row-level security**, so a
query can only ever see its own tenant's rows — there is no
cross-tenant query path in the API. Mailbox storage is likewise scoped
per tenant in the mail core.

## Encryption

- **In transit** — TLS 1.2+ is required on every user-facing port
  (IMAP, SMTP submission, CalDAV/CardDAV, JMAP, and HTTPS). Plaintext
  connections are never accepted.
- **At rest** — mailbox and blob data are encrypted with per-tenant
  envelope keys. Object storage goes through the zero-knowledge object
  fabric so the storage layer never sees plaintext.
- **Zero-Access Vault** — opt-in client-side encrypted folders that the
  server cannot index or read.
- **Confidential Send** — messages can be sent through an
  MLS-derived envelope with a recipient portal, so the body stays
  encrypted end-to-end.

## Key management

KMail uses a hierarchical key model (master keyring → per-tenant data
keys). On the Privacy plan you can bring your own **customer-managed
key (CMK)**: KMail wraps tenant keys with a key you control and can
revoke, which cryptographically cuts off access.

## Authentication & access control

- Single sign-on via OIDC; sessions are tracked server-side in a
  session ledger so they can be listed and revoked.
- Strong second factors: **TOTP** and **WebAuthn / passkeys**.
- Legacy IMAP/SMTP clients that can't do OAuth2 use scoped **app
  passwords** instead of your login password.
- Administrative actions are role-gated.

## Audit logging

Administrative actions are recorded in a **hash-linked, tamper-evident
audit chain**, so the log can be verified for completeness and any
gap or edit is detectable. Admins can review and export the audit log.

## Abuse & deliverability protection

KMail enforces per-tenant rate limits, outbound spam/abuse scoring, and
DMARC/DKIM/SPF alignment to protect sender reputation. See
[DMARC explained](/help/security/dmarc-explained) and
[Delivery issues](/help/troubleshooting/delivery-issues).

## Backups & recovery

Tenant data is backed up on a regular schedule with point-in-time
recovery for the control-plane database. Recovery procedures are
exercised by the operations team.

## Compliance & data processing

For control mappings, the data processing agreement, and the current
sub-processor list, see the repository compliance pack:

- Security overview (detailed): `docs/compliance/SECURITY_OVERVIEW.md`
- SOC 2 control mapping: `docs/compliance/SOC2_CONTROL_MAPPING.md`
- Data processing agreement: `docs/compliance/DPA.md`
- Sub-processors: `docs/compliance/SUBPROCESSORS.md`
- Incident response: `docs/compliance/incident-response.md`
