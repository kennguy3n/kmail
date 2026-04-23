# KMail — Stalwart License Evaluation

**License (this document)**: Proprietary — All Rights Reserved. See
[LICENSE](../LICENSE).

> Status: Phase 1 — Foundation. This document captures the
> architectural analysis that the AGPL boundary around Stalwart is
> preserved by KMail's deployment topology. It is **not legal
> advice**. Before KMail ships as a paid SaaS product a qualified
> attorney must sign off on the final posture, including the
> specific Stalwart version, the exact deployment shape, and any
> future Stalwart-side license changes.

---

## 1. Stalwart's Licensing

Stalwart (upstream repo:
[stalwartlabs/mail-server](https://github.com/stalwartlabs/mail-server))
is published under a dual-license model:

- **Base license**: GNU Affero General Public License, version 3.0
  (AGPL-3.0). This is the license attached to the source and
  release artifacts KMail consumes.
- **Enterprise dual license**: Stalwart Labs offers a commercial
  enterprise license that removes the AGPL copyleft terms for
  customers who cannot accept AGPL obligations or who require
  enterprise-only features.

KMail pins Stalwart to
[v0.16.0](https://github.com/stalwartlabs/mail-server/releases/tag/v0.16.0)
(released April 20, 2026). The evaluation below applies to this
version. Any Stalwart upgrade must revisit this document; upstream
has the right to change license terms in a future release and we
must not silently inherit that change.

### 1.1 Summary of AGPL-3.0 obligations

The AGPL-3.0 creates two distinct obligations on an operator:

1. **Distribution obligations** — identical to GPL-3.0. If you
   distribute a covered binary, you must make the corresponding
   source available under AGPL-3.0.
2. **Network-use obligation** (§13 of AGPL-3.0, the "SaaS clause") —
   if you modify the covered software and run the modified version
   to interact with users over a network, you must offer those
   users the modified source.

Obligation (1) is only triggered by distributing a Stalwart binary,
which KMail does not do — KMail operates Stalwart as part of a
service, it does not ship it to customers. Obligation (2) is only
triggered for users of Stalwart's own network interfaces where
users interact with a **modified** Stalwart. Stock Stalwart —
used unmodified — does not trigger §13 at all.

### 1.2 What AGPL §13 does not require

AGPL §13 does not require KMail to release KMail's own source code
merely because KMail interacts with Stalwart over a network. The
§13 obligation is scoped to **modifications of the covered program
offered to users interacting with it remotely**. A separate program
that calls Stalwart over a standard protocol is not a modification
of Stalwart.

---

## 2. KMail's Licensing Posture

KMail is proprietary, closed-source, and not distributed as source
or binaries to customers. KMail is delivered as a SaaS product
inside KChat B2B. The relevant KMail components are:

- **Go control plane** — first-party, proprietary.
- **React frontend** — first-party, proprietary.
- **Stalwart** — unmodified upstream binary, AGPL-3.0 licensed,
  deployed as a separate process on separate hosts.
- **zk-object-fabric** — first-party, operated as a separate
  service with its own S3-compatible network API; licensing is
  governed by its own repository's LICENSE file.

The question this document answers: **can KMail legitimately ship
this architecture without triggering AGPL-3.0 obligations that are
incompatible with the proprietary KMail license?**

---

## 3. Boundary Analysis

### 3.1 Stalwart runs as a separate process

Stalwart is not linked into any KMail Go binary. The Go control
plane communicates with Stalwart exclusively over network
protocols:

- **JMAP / HTTPS** — client-facing mail operations (owned by the
  Go BFF — see [JMAP-CONTRACT.md](JMAP-CONTRACT.md)).
- **SMTP** — inbound mail from the Internet, outbound mail to
  external MTAs.
- **IMAP / CalDAV / CardDAV / WebDAV** — third-party client
  compatibility.
- **Stalwart admin HTTP API** — tenant provisioning, user / domain
  management.
- **S3-compatible HTTP** — Stalwart → zk-object-fabric blob
  operations.
- **PostgreSQL wire protocol** — Stalwart's metadata store.

KMail consumes **zero** Stalwart Rust source or libraries. KMail
services are not "derivative works" of Stalwart in the copyright
sense; they are independent programs that speak standardized
network protocols.

### 3.2 Standard protocols, not private APIs

Every protocol listed above is an open, standardized, vendor-neutral
interface:

- JMAP — [RFC 8620](https://www.rfc-editor.org/rfc/rfc8620), RFC 8621.
- SMTP — RFC 5321 and successors.
- IMAP — RFC 3501 and RFC 9051.
- CalDAV — RFC 4791. CardDAV — RFC 6352.
- S3 — a de-facto industry-standard HTTP object API.

KMail could swap Stalwart for any other implementation of these
protocols (for example, a different JMAP server) without changing
the KMail first-party code. This is the clearest evidence that
KMail is not derived from Stalwart.

### 3.3 No modified Stalwart binary offered to users

KMail runs **unmodified** Stalwart v0.16.0 release binaries
(plus deployment-time TOML configuration — see
[configs/stalwart.example.toml](../configs/stalwart.example.toml)).
Configuration files are not source-code modifications. Even when
we eventually patch Stalwart for operational needs (for example, a
backported bug fix), we intend to upstream those patches; any
locally applied patches will be kept minimal and tracked explicitly
so that AGPL §13 disclosure, if it ever applies, is easy to
satisfy.

### 3.4 What AGPL §13 would require if triggered

If we ever operate a modified Stalwart such that §13 applies, we
would need to:

- Publish the modified Stalwart source under AGPL-3.0 to users who
  interact with it "over a network," and
- Do so "prominently" and "at no charge."

The §13 obligation covers only the Stalwart source, not KMail's Go
control plane or React frontend — those are independent programs
(see §3.1–§3.2). However, we explicitly commit in this document to
avoid running a materially modified Stalwart in production without
first restructuring whatever change we needed as a fork with a
publicly documented source repository. This preserves optionality.

### 3.5 Conclusion — AGPL boundary is preserved

Under the current architecture:

- KMail first-party code is not a derivative work of Stalwart.
- KMail does not distribute Stalwart binaries.
- KMail does not run a modified Stalwart.
- All KMail ↔ Stalwart communication is over standard network
  protocols with multiple independent implementations.

The AGPL boundary is respected. KMail's proprietary license and
Stalwart's AGPL-3.0 license coexist without conflict.

---

## 4. Do We Need the Enterprise Dual License?

The enterprise dual license primarily targets two use cases:

1. **Customers who cannot accept AGPL obligations** on the
   Stalwart binaries they operate internally (e.g., a customer that
   embeds Stalwart inside its own closed-source appliance).
2. **Customers who need Stalwart enterprise-only features** that
   are not part of the AGPL-licensed base.

Neither applies to KMail's Phase 1–4 posture:

- KMail operates Stalwart itself, as a service, behind the Go BFF
  and Stalwart's own SMTP/IMAP/CalDAV edges. Customers do not
  receive a Stalwart binary.
- KMail uses the AGPL-licensed Stalwart feature set exclusively in
  Phase 1. If a future roadmap item depends on an enterprise-only
  Stalwart feature, we will re-evaluate.

**Recommendation**: do **not** purchase the Stalwart enterprise
dual license for Phase 1. Revisit this decision if:

- A Phase 3+ roadmap item requires a Stalwart enterprise feature.
- Stalwart's v1.0.0 (H1 2026) shifts features between AGPL and
  enterprise tiers.
- A design-partner customer requires a Stalwart contract with a
  commercial warranty / indemnity.

---

## 5. Risks

| Risk                                                          | Likelihood | Impact | Mitigation                                                                                              |
| ------------------------------------------------------------- | ---------- | ------ | ------------------------------------------------------------------------------------------------------- |
| Stalwart relicenses base to a non-AGPL copyleft               | Low        | Medium | Version pin; re-evaluate on each Stalwart upgrade; fork from the last AGPL commit is a backstop.         |
| We silently accumulate local Stalwart patches                 | Medium     | Medium | Require all Stalwart patches to land in a tracked patch set with ticket references; CI gates.           |
| §13 is tested by a litigious user who claims network use      | Low        | Medium | Maintain a public source repository for any locally maintained Stalwart fork; publish build procedure.   |
| A future KMail feature implies tighter Stalwart coupling      | Medium     | Medium | Any change that proposes linking Stalwart code into KMail Go binaries requires explicit legal review.   |
| A customer requires a Stalwart indemnity we cannot offer      | Medium     | Low    | Offer the Stalwart Labs enterprise license as a pass-through add-on on the enterprise tier.             |

---

## 6. Next Steps

1. **Legal review**: route this document to qualified legal counsel
   (trademark, copyright, and software-license specialist) before
   public launch. Capture counsel's review date and signature on
   the final PR.
2. **Stalwart v1.0.0 watch**: track the v1.0.0 release notes
   (expected H1 2026) for any license-boundary changes, particularly
   the split between AGPL base and enterprise features.
3. **Stalwart Labs conversation**: open a vendor relationship with
   Stalwart Labs ahead of a possible enterprise contract for
   dedicated-enterprise KMail customers (Phase 4+).
4. **Internal policy**: adopt the rule that any patch to Stalwart
   must land in a tracked fork repository, under AGPL-3.0, before
   it ships to production. No silent private forks.
5. **Revisit on upgrade**: every Stalwart minor version bump
   triggers a re-read of this document and — if anything has
   changed — a re-review with counsel.

---

## 7. Summary

- Stalwart v0.16.0 is AGPL-3.0. KMail is proprietary SaaS.
- KMail consumes Stalwart as an unmodified, separate network
  service over standard protocols. No Stalwart code is linked into
  KMail.
- AGPL distribution obligations are not triggered (no binary
  distribution).
- AGPL §13 network-use obligations are not triggered (no modified
  Stalwart).
- The enterprise dual license is **not required for Phase 1–4**.
  Revisit on feature need, customer contract, or upstream license
  change.
- Legal review remains a gating item before the SaaS offering
  launches.
