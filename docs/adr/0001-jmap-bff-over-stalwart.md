# 0001 — A Go JMAP BFF in front of Stalwart

- **Status**: Accepted
- **Related**: [`../JMAP-CONTRACT.md`](../JMAP-CONTRACT.md), [`../ARCHITECTURE.md`](../ARCHITECTURE.md)

## Context

KMail's mail core is [Stalwart](https://stalw.art/), which already
speaks JMAP, IMAP, SMTP, and CalDAV/CardDAV. The product needs
multi-tenant control-plane behaviour Stalwart does not provide on its
own: tenant resolution and shard routing, OIDC session handling,
billing/quota enforcement, abuse and deliverability controls, audit
logging, and a stable HTTP API for the React app and the SDK.

We could have let clients talk to Stalwart directly, or embedded all of
this as Stalwart plugins.

## Decision

Put a **Go backend-for-frontend (BFF)** — `cmd/kmail-api` — in front of
Stalwart. The BFF terminates client auth, applies tenant/policy logic,
and **proxies JMAP** to the correct Stalwart shard
(`internal/jmap/proxy.go`), while exposing the control-plane REST API
under `/api/v1` and SCIM under `/scim/v2`. Stalwart remains the source
of truth for mailbox data; the BFF owns everything around it.

## Consequences

- Clients have a single, stable surface; Stalwart can be upgraded or
  resharded behind the proxy without changing the client contract.
- The per-request shard routing and circuit-breaking live in one place
  (the proxy), enabling failover (see
  [ADR 0006](./0006-stalwart-on-long-lived-hosts.md)).
- The BFF is on the hot path for mailbox traffic, so its latency
  (`kmail_jmap_proxy_duration_seconds`) must be watched closely (see
  [monitoring](../operator/monitoring.md)).
- We own a non-trivial proxy layer rather than leaning entirely on
  Stalwart — a deliberate trade of complexity for tenancy/control.
