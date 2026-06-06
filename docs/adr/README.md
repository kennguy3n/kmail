# Architecture Decision Records

This directory records the significant architectural decisions behind
KMail — the *why* behind the structure documented in
[`../ARCHITECTURE.md`](../ARCHITECTURE.md). Each ADR captures the
context, the decision, and its consequences so future contributors can
understand (and revisit) a choice without archaeology.

## Format

ADRs follow a lightweight [MADR](https://adr.github.io/madr/)-style
template: **Status**, **Context**, **Decision**, **Consequences**, and
where useful, **Alternatives considered**. Each ADR is numbered and
immutable once **Accepted** — supersede it with a new ADR rather than
rewriting history.

## Index

| ADR | Title | Status |
| --- | ----- | ------ |
| [0001](./0001-jmap-bff-over-stalwart.md) | A Go JMAP BFF in front of Stalwart | Accepted |
| [0002](./0002-postgres-rls-tenant-isolation.md) | PostgreSQL row-level security for tenant isolation | Accepted |
| [0003](./0003-fail-closed-oidc-auth.md) | Fail-closed OIDC authentication | Accepted |
| [0004](./0004-worker-process-decomposition.md) | Splitting background workers into a separate process | Accepted |
| [0005](./0005-bff-stalwart-mtls.md) | BFF→Stalwart mutual TLS via cert-manager | Accepted |
| [0006](./0006-stalwart-on-long-lived-hosts.md) | Running Stalwart on long-lived hosts, not Kubernetes pods | Accepted |
| [0007](./0007-zk-object-fabric-blob-storage.md) | zk-object-fabric for zero-knowledge blob storage | Accepted |
| [0008](./0008-search-backend-cutover.md) | A pluggable search backend with Meilisearch→OpenSearch cutover | Accepted |

## Adding an ADR

1. Copy the structure of an existing ADR; take the next number.
2. Open it as **Proposed**; flip to **Accepted** once merged.
3. Add a row to the index table above.
