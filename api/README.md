# KMail — API Contracts

**License**: Proprietary — All Rights Reserved. See [../LICENSE](../LICENSE).

This directory is the home for the machine-readable API contracts
that the Go control plane exposes and consumes.

In Phase 1 it is a placeholder. The narrative contracts live in
`docs/`:

- [../docs/JMAP-CONTRACT.md](../docs/JMAP-CONTRACT.md) — the JMAP
  surface the Go BFF proxies between the React client and Stalwart.
- [../docs/ARCHITECTURE.md §7](../docs/ARCHITECTURE.md) — the Go
  service topology.
- [../docs/SCHEMA.md](../docs/SCHEMA.md) — the control-plane
  Postgres schema shape.

## Planned contents (Phase 2+)

- `api/jmap/` — OpenAPI / JSON Schema snippets describing KMail
  extension capabilities (`confidential-send`, `shared-inbox`,
  `chat-bridge`, `vault`) and the BFF-specific response headers
  (`X-KMail-Correlation-Id`, rate-limit headers).
- `api/admin/` — OpenAPI for the tenant / admin console backend.
- `api/migration/` — the Migration Orchestrator control API.
- `api/deliverability/` — IP pool / suppression / DMARC ingestion
  control API.
- `api/audit/` — query and export API for the audit log.

Every contract in this directory is generated from or validated
against Go code; do not edit generated artifacts by hand.

## Conventions

- Breaking changes require a major version bump in the contract
  path (`api/jmap/v1/`, `api/jmap/v2/`).
- Every response carries `X-KMail-Correlation-Id` — see
  `docs/JMAP-CONTRACT.md §7.3`.
- Every endpoint is tenant-scoped; no cross-tenant query paths.
