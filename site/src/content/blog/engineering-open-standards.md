---
title: "Engineering KMail: open standards, JMAP, and zero-access vaults"
description: "For technical teams evaluating KMail: how we proxy Stalwart with a multi-tenant Go backend, why JMAP matters, and how zero-access vaults actually work."
pubDate: 2026-06-20
author: The KMail team
tags: [engineering, architecture, security]
---

KMail is not a new email protocol. It is a privacy-first layer on top of the same standards that have powered email for decades. For engineering and security teams evaluating the platform, this post explains the architecture, the protocol choices, and the encryption model.

## The control plane: Go backend + Stalwart

The mail storage and delivery engine is Stalwart, a modern JMAP/IMAP/SMTP server. Our Go backend adds multi-tenancy, billing, SCIM provisioning, encryption, and compliance. The React web app talks to the Go backend over a typed JMAP contract, and the backend translates that into Stalwart calls where needed.

This split means:

- **No lock-in.** Your mail is stored in standard formats accessible by any IMAP/SMTP client.
- **Portability.** You can export everything and leave.
- **Familiar operations.** Admins who know Stalwart or standard mail DNS can reason about the system.

## JMAP over IMAP

We use JMAP (RFC 8620/8621) for the web and mobile clients. JMAP is a modern, JSON-based sync protocol that is more efficient than IMAP for web clients: batched requests, typed objects, and push-friendly.

The web app does not speak directly to Stalwart. It calls `jmapClient` methods that hit the Go backend, which either proxies to Stalwart or serves from its own state (billing, retention, audit logs). The contract is documented in `docs/JMAP-CONTRACT.md` in the repository.

## CalDAV/CardDAV for calendar and contacts

Calendar and contacts use CalDAV and CardDAV. The backend layers tenant isolation and sharing on top of Stalwart's CalDAV store, while the web app sees a clean JMAP-shaped calendar API. Native clients like Apple Calendar and Thunderbird sync through standard CalDAV.

![KMail shared calendars](/screenshots/08-shared-calendars.png)

## Zero-access vaults

Zero-access vault folders use strict client-side encryption. When a user creates a vault folder:

1. A key is derived on the client.
2. The folder metadata is stored in Stalwart like any other mailbox.
3. Message bodies are encrypted before upload.
4. The server stores ciphertext only.

The server cannot index, search, or preview vault content. That is the point: even a compromised operator or lawful-access request cannot read the mail without the client's key. This design is why vault folders deliberately disable server-side search and push previews.

![KMail vault admin](/screenshots/03-vault.png)

## Customer-managed keys

For tenants that want more control, KMail supports customer-managed keys through an HSM integration. The tenant holds the key material; KMail's backend only sees a wrapped key reference. Rotation, revocation, and audit are tenant-owned.

![KMail CMK configuration](/screenshots/26-cmk-admin.png)

## SCIM and webhooks

User provisioning uses SCIM 2.0, so you can connect an identity provider and push users, groups, and deactivations into KMail. Webhooks let you react to domain verification, billing, and security events in your own automation.

![KMail SCIM admin](/screenshots/27-scim-admin.png)

![KMail webhook admin](/screenshots/23-webhook-admin.png)

## Why open standards matter

We could have built a proprietary sync protocol. It would have been faster in the short term. But it would also have locked in customers. Open standards are the antidote: they make migration easy, they let teams use their favorite clients, and they force us to compete on product quality rather than data gravity.

## Read more

- [API reference](/docs/api)
- [SCIM conformance notes](/docs/scim)
- [Architecture overview](/docs/architecture)

[Start exploring KMail](/signup) or [compare plans](/pricing).
