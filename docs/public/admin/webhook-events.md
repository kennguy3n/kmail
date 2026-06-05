---
title: Webhook event catalog
description: The events KMail can deliver to your endpoints, the delivery format, and how to verify signatures.
category: Admin
order: 30
updated: 2026-06-05
---

KMail can push events to HTTPS endpoints you register, so external
systems react in real time — sync a CRM when mail arrives, alert on a
bounce, or kick off automation when a migration finishes.

Register endpoints under **Admin → Webhooks** or via the API
(`POST /api/v1/integ/webhooks`). Each endpoint subscribes to one or more
event types.

## Event types

| Event                     | Fires when…                                            |
| ------------------------- | ------------------------------------------------------ |
| `email.received`          | A message is delivered to a mailbox in your tenant.    |
| `email.bounced`           | An outbound message hard-bounces.                      |
| `email.complaint`         | A recipient marks a message as spam (feedback loop).   |
| `calendar.event_created`  | A calendar event is created.                           |
| `calendar.event_updated`  | A calendar event is modified.                          |
| `migration.completed`     | A Gmail/M365 migration job finishes.                   |
| `webhook.ping`            | A test delivery you trigger when registering/testing.  |

## Delivery format

Each delivery is an HTTP `POST` with a JSON body and these headers:

| Header                      | Meaning                                         |
| --------------------------- | ----------------------------------------------- |
| `Content-Type`              | Always `application/json`.                       |
| `X-KMail-Event`             | The event type (e.g. `email.received`).          |
| `X-KMail-Signature`         | HMAC-SHA256 signature (see below).               |
| `X-KMail-Webhook-Timestamp` | Unix seconds (v2 signing only).                  |
| `X-KMail-Webhook-Nonce`     | Per-delivery nonce for replay defense (v2 only). |

### v1 signature

```
X-KMail-Signature: t=<unix>,v1=<hex>
```

The `v1` value is `HMAC_SHA256(secret, "<unix>." + rawBody)`,
hex-encoded. Verify by recomputing with your endpoint secret and
comparing in constant time.

### v2 signature (recommended)

```
X-KMail-Signature: v2=<hex>
X-KMail-Webhook-Timestamp: <unix>
X-KMail-Webhook-Nonce: <nonce>
```

The `v2` value is `HMAC_SHA256(secret, "<unix>.<nonce>." + rawBody)`.
The nonce participates in the MAC so you can dedupe replays within your
timestamp tolerance window (reject timestamps older than ~5 minutes).

> Always compute the HMAC over the **raw request body bytes** before any
> JSON re-serialization, and compare signatures with a constant-time
> function.

## Retries

Failed deliveries (non-2xx or connection errors) are retried with
exponential backoff. Return a `2xx` quickly and process asynchronously;
deliveries may arrive out of order, so make handlers idempotent using
the event id in the payload.

## Verifying — example (Node.js)

```js
import crypto from "node:crypto";

function verify(req, secret) {
  const ts = req.headers["x-kmail-webhook-timestamp"];
  const nonce = req.headers["x-kmail-webhook-nonce"];
  const sig = req.headers["x-kmail-signature"]; // "v2=<hex>"
  const expected =
    "v2=" +
    crypto
      .createHmac("sha256", secret)
      .update(`${ts}.${nonce}.`)
      .update(req.rawBody) // Buffer of the raw body
      .digest("hex");
  return crypto.timingSafeEqual(Buffer.from(sig), Buffer.from(expected));
}
```

See the [API reference](/docs/api) for the webhook management endpoints.
