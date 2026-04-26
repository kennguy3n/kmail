# Data Processing Agreement (Template)

This Data Processing Agreement ("DPA") supplements the KMail
Master Subscription Agreement between **KMail** (the
"Processor") and the customer (the "Controller") and forms part
of that agreement.

## 1. Definitions

Terms used in this DPA have the meanings given in Article 4 of
the General Data Protection Regulation ("GDPR"): "personal data",
"processing", "controller", "processor", "data subject", "sub-
processor", "supervisory authority", "personal data breach".

## 2. Subject Matter and Duration

KMail processes personal data on behalf of the Controller solely
to provide the KMail email + calendar service described in the
Master Subscription Agreement. Processing continues for the term
of the Master Subscription Agreement, and during any post-
termination wind-down period (typically 30 days).

## 3. Categories of Data Subjects

* Controller's employees, contractors, and other authorised users
* External correspondents who send mail to (or receive mail from)
  Controller's mailboxes
* Calendar attendees external to the Controller

## 4. Categories of Personal Data

* Account identity: email address, display name, OIDC subject ID
* Mailbox content: RFC 5322 message bodies, headers, attachments
* Calendar data: iCalendar VEVENT/VTODO/VJOURNAL records
* Address book data: vCard 4.0 contact records
* Operational metadata: IP addresses, user agent strings, audit
  log entries, rate-limit counters, billing seat counts

## 5. Controller Instructions and Lawful Basis

KMail processes personal data only on documented instructions
from the Controller, including instructions encoded in the KMail
Service configuration (e.g. retention policies, suppression
lists, BIMI records, sub-processor consent). The Controller
remains responsible for the lawful basis under Article 6 GDPR.

## 6. Confidentiality and Authorised Personnel

KMail ensures that personnel authorised to process personal data
are bound by appropriate confidentiality obligations. Operator
access to tenant data is gated through the **Reverse Access
Proxy** (Phase 5), which requires a Controller-side approval
recorded in `approval_requests` before any data is read.

## 7. Security of Processing (Article 32)

KMail implements technical and organisational measures including:

* **Encryption at rest** via zk-object-fabric storage envelopes;
  Privacy plan tenants may bring their own KMS root key (CMK).
* **Tenant isolation** via PostgreSQL Row-Level Security plus
  per-tenant zk-object-fabric buckets and Stalwart shards.
* **Authentication** via OIDC against the Controller's KChat
  instance; bearer tokens for SCIM provisioning.
* **Audit logging** with a SHA-256 hash chain
  (`audit_log.entry_hash`) so admins can detect tampering.
* **Rate limiting** via Valkey to mitigate brute-force and
  enumeration attacks.
* **Secure development** via mandatory CI checks (lint, vet,
  build, test, frontend type-check) and dependency review.

## 8. Sub-Processors

The list of authorised sub-processors is maintained in
[`SUBPROCESSORS.md`](./SUBPROCESSORS.md). KMail will give the
Controller notice (through the in-product changelog and via
email to the Controller's billing contact) at least 30 days
before authorising any new sub-processor. The Controller may
object on reasonable grounds.

## 9. International Transfers

KMail Stalwart shards are placed in the region the Controller
selects at signup. Where a sub-processor stores or processes
personal data outside the EEA, KMail relies on the EU Standard
Contractual Clauses (Module 3, Processor-to-Processor) and on
applicable adequacy decisions.

## 10. Data Subject Requests

KMail provides Controller-side tooling (admin console, JMAP
APIs) so the Controller can fulfil data-subject access, erasure,
rectification, and portability requests directly. The
**Export Runner** (Phase 5) packages a tenant's mailboxes,
calendars, and audit log into a portable archive for GDPR
Article 20 portability requests.

## 11. Personal Data Breach Notification

KMail will notify the Controller without undue delay, and at
the latest within 72 hours, of any personal data breach
affecting the Controller's data. The notification will include
the categories and approximate number of data subjects and
records, the likely consequences, and the measures taken or
proposed.

## 12. Audits

The Controller may audit KMail's compliance with this DPA by
reviewing the latest SOC 2 Type II report
([`SOC2_CONTROL_MAPPING.md`](./SOC2_CONTROL_MAPPING.md)) and
penetration test summary. On request and subject to a mutually
agreed scope and confidentiality protections, KMail will provide
additional information necessary to demonstrate compliance.

## 13. Deletion and Return of Data

On termination of the Master Subscription Agreement, the
Controller may export their data via the Export API for 30 days,
after which KMail will delete all personal data, except where
storage is required by Union or Member State law.

## 14. Liability and Indemnity

The liability provisions of the Master Subscription Agreement
apply to processing under this DPA.
