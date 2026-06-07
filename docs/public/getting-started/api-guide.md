---
title: Using the KMail API
description: How to authenticate and call the KMail REST, SCIM, and JMAP APIs, with a pointer to the full OpenAPI reference.
category: Getting Started
order: 50
updated: 2026-06-06
---

KMail exposes three programmatic surfaces:

- **`/api/v1`** — a tenant-scoped REST API for administering mail,
  calendars, contacts, billing, deliverability, and security.
- **`/scim/v2`** — SCIM 2.0 for user/group provisioning from your
  identity provider.
- **`/jmap`** — JMAP (RFC 8620/8621) for mailbox data: reading,
  sending, and syncing mail and calendars.

The full, always-current reference is generated from the server code
and rendered at **[the API reference](/docs/api)**.

## Authentication

Most `/api/v1` endpoints require an **OIDC bearer token** from your
configured identity provider:

```
Authorization: Bearer <access_token>
```

The token's claims identify the tenant and user. Requests without a
valid token get `401 Unauthorized`; valid tokens lacking the required
role get `403 Forbidden`. Every endpoint is tenant-scoped, so you only
ever see your own tenant's data.

A few endpoints are intentionally **public** (no token): the signup
endpoint, the Confidential Send recipient portal, and the
`/.well-known/*` autodiscovery documents.

### SCIM tokens

The SCIM API uses a dedicated per-tenant bearer token you mint in
**Admin → SCIM** and configure in your IdP. See the
[SCIM provisioning guide](/help/admin/scim-provisioning).

## Example request

```bash
curl -s https://api.kmail.kchat.dev/api/v1/tenants \
  -H "Authorization: Bearer $KMAIL_TOKEN"
```

Every response carries an `X-KMail-Correlation-Id` header you can quote
to support when reporting an issue.

## Errors

Errors use standard HTTP status codes with a JSON body:

```json
{ "error": "human readable message", "code": "machine_code" }
```

## Webhooks

Rather than polling, register an HTTPS endpoint to receive events
(`email.received`, `email.bounced`, `calendar.event_created`, …). Each
delivery is signed with HMAC-SHA256. See the
[webhook event catalog](/help/admin/webhook-events).

## Versioning

The REST base path is `/api/v1`. Additive changes (new fields, new
endpoints) are backwards-compatible; breaking changes bump the major
version.
