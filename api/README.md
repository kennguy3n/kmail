# KMail — API Contracts

**License**: Proprietary — All Rights Reserved. See [../LICENSE](../LICENSE).

This directory is the home for the machine-readable API contracts
that the Go control plane exposes.

## `openapi/` — generated OpenAPI 3.1 spec

[`openapi/kmail.openapi.json`](./openapi/kmail.openapi.json) is the
committed OpenAPI 3.1 description of KMail's HTTP surface. It is
**generated, not hand-edited**: [`openapi/generate.mjs`](./openapi/generate.mjs)
scans the Go sources in `cmd/` and `internal/` for the
`"<METHOD> /path"` literals the handlers register on
`http.ServeMux` (Go 1.22+ method-aware patterns) and emits one tagged
operation per route. It documents the `/api/v1` administrative
surface, the SCIM 2.0 provisioning API at `/scim/v2`, the `/jmap`
proxy entry point, and the `/.well-known` autodiscovery documents.

Regenerate it after adding or changing a route:

```bash
make openapi          # = node api/openapi/generate.mjs
```

The spec is consumed by the marketing site's API reference page
([`site/src/pages/docs/api.astro`](../site/src/pages/docs/api.astro),
served at `/docs/api`), which renders it with Redoc.
`site/scripts/sync-content.mjs` copies the JSON into
`site/public/openapi/kmail.openapi.json` at build time, so the
committed spec is the single source of truth — do not edit the copy
under `site/public/`.

## Narrative contracts

The prose contracts that the generated spec complements live in
`docs/`:

- [../docs/JMAP-CONTRACT.md](../docs/JMAP-CONTRACT.md) — the JMAP
  surface the Go BFF proxies between the React client and Stalwart.
- [../docs/ARCHITECTURE.md §7](../docs/ARCHITECTURE.md) — the Go
  service topology.
- [../docs/SCHEMA.md](../docs/SCHEMA.md) — the control-plane
  Postgres schema shape.

## Conventions

- Breaking changes require a major version bump in the base path
  (`/api/v1` → `/api/v2`).
- Every response carries `X-KMail-Correlation-Id` — see
  `docs/JMAP-CONTRACT.md §7.3`.
- Every endpoint is tenant-scoped; no cross-tenant query paths.
- A small set of routes are intentionally public (no bearer token):
  `POST /api/v1/signup` and its status polling route, the
  Confidential Send recipient portal (`/api/v1/secure/{token}`), and
  `/.well-known/*`. These are marked with an empty `security: []` in
  the spec. The `/api/v1/send/{id}` undo-send routes are **not** public
  — they are wrapped with `authMW.Wrap` and require a bearer token.
