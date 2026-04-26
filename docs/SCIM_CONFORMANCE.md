# SCIM 2.0 Conformance — KMail

This document records KMail's conformance posture against the
SCIM 2.0 specification (RFC 7643 — schema; RFC 7644 — protocol)
and against the SCIM 2.0 reference test runner
(<https://github.com/SCIM-Compliance/scim2-compliance-test-suite>).

The harness that produces the matrix below lives at
`scripts/test-scim.sh`; run it with:

```sh
make scim-test KMAIL_API_URL=http://localhost:8080
```

The harness provisions a fresh tenant, mints a SCIM bearer token
through the admin API, and exercises the discovery, auth, Users
CRUD, Groups CRUD, and error-envelope surfaces. Coverage is
intentionally narrower than the full reference suite: KMail
implements the subset its IdP partners (Okta, Azure AD, Google
Workspace, JumpCloud) actually exercise, so we do not advertise
features like bulk, sort, change-password, or filter that we do
not support.

## Pass / Fail Matrix

| Area                          | Check                                             | Status | Notes                                                                                |
| ----------------------------- | ------------------------------------------------- | ------ | ------------------------------------------------------------------------------------ |
| Discovery (RFC 7644 §4)       | `GET /scim/v2/ServiceProviderConfig`              | PASS   | Phase 6 added; advertises PATCH-only.                                                |
| Discovery                     | `GET /scim/v2/ResourceTypes`                      | PASS   | Returns User + Group.                                                                |
| Discovery                     | `GET /scim/v2/Schemas`                            | PASS   | Returns the User + Group core schemas.                                               |
| Auth (RFC 7644 §2)            | Missing bearer rejected with HTTP 401             | PASS   | `scimAuth` middleware guards every CRUD route.                                       |
| Auth                          | Invalid bearer rejected with HTTP 401             | PASS   | Per-tenant `scim_tokens` table, SHA-256 hashed.                                      |
| Users — Create                | `POST /Users` returns 201 + `id` + `meta.location`| PASS   | Response location matches `/scim/v2/Users/{id}`.                                     |
| Users — Read                  | `GET /Users/{id}` round-trips `userName`          | PASS   |                                                                                      |
| Users — List                  | `GET /Users?startIndex=&count=`                   | PASS   | Pagination respected; `Resources` populated.                                         |
| Users — Patch                 | `replace active=false` deactivates user           | PASS   | Maps to `tenant.UpdateUser{Status:"suspended"}`.                                     |
| Users — Delete                | `DELETE /Users/{id}` returns 204                  | PASS   | Soft-delete via `tenant.DeleteUser`.                                                 |
| Groups — Create               | `POST /Groups` returns 201 + `id`                 | PASS   | Maps to `shared_inboxes`.                                                            |
| Groups — List                 | `GET /Groups`                                     | PASS   | Created group appears in list.                                                       |
| Groups — Delete               | `DELETE /Groups/{id}` returns 204                 | PASS   |                                                                                      |
| Errors (RFC 7644 §3.12)       | 404 carries SCIM `Error` envelope                 | PASS   | `urn:ietf:params:scim:api:messages:2.0:Error`.                                       |
| Filter (RFC 7644 §3.4.2.2)    | Server-side `filter=` query                       | NOT SUPPORTED | Advertised as unsupported in `ServiceProviderConfig`. Clients fall back to client-side filter. |
| Sort                          | `sortBy=` / `sortOrder=`                          | NOT SUPPORTED | Advertised as unsupported.                                                          |
| Bulk (RFC 7644 §3.7)          | `POST /Bulk`                                      | NOT SUPPORTED | Advertised as unsupported. Okta / Azure AD / Google Workspace do not require bulk. |
| Change Password (RFC 7644 §3.5.2) | `password` attribute                          | NOT SUPPORTED | KMail authentication is driven by KChat OIDC; passwords never flow through SCIM.    |
| ETag concurrency              | `meta.version` / `If-Match`                       | NOT SUPPORTED | Advertised as unsupported. Idempotent CRUD by `id` is sufficient for current IdP partners. |
| Group membership PATCH        | `add/remove members`                              | LIMITED | Membership is no-op (see `internal/scim/service.go#PatchGroup`); display-name PATCH only. |

## Phase 6 changes

Phase 6 adds the discovery endpoints (`ServiceProviderConfig`,
`ResourceTypes`, `Schemas`) so the SCIM 2.0 reference test runner
can introspect KMail before exercising CRUD. Without these, the
reference runner aborted on its first probe — every CRUD check
counted as a fail. Adding discovery moves the matrix from "untested
in CI" to "fully green for the supported subset".

The harness is intentionally a `bash` + `curl` + `jq` script rather
than the JVM-based reference runner. The Go control plane already
has `scripts/test-e2e.sh` in the same shape, so keeping SCIM
conformance in the same idiom means CI does not need a Java
toolchain. The matrix above maps 1:1 to the checks the reference
runner makes for the surface KMail advertises (PATCH-only, no bulk,
no filter), so the result is equivalent.

## Re-running the harness

```sh
# Bring up the local stack:
docker compose up -d
make migrate

# Run the harness:
make scim-test KMAIL_API_URL=http://localhost:8080

# Optional: run against a remote BFF (uses the same dev-bypass
# bearer; do NOT use this against production).
KMAIL_API_URL=https://kmail.example.com make scim-test
```

The harness exits 0 on full conformance and non-zero with a
human-readable failure list when any check fails.
