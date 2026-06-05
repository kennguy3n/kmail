# Security Testing

This document describes KMail's penetration-testing preparation: the
runnable probes under `scripts/security/`, what each verifies, and
how to run them against a non-production deployment. These are
**preparation harnesses** — they exercise the real endpoint
inventory so an external pentest (or a CI security stage) starts from
a known-good baseline, not a blank page.

> **Run against staging / ephemeral environments only.** The probes
> send malformed and unauthenticated requests. None of them fabricate
> credentials — tokens and tenant ids are supplied by the operator
> via environment variables.

## Endpoint inventory

`scripts/security/tenant-endpoints.txt` is the list of tenant-scoped
API endpoints (those with an `{id}` / `{tenantID}` segment, plus the
admin reverse-proxy routes). It is **auto-derived** from the Go route
registrations — regenerate it after adding or renaming routes:

```sh
scripts/security/gen-endpoints.sh
```

This keeps the IDOR/SQLi/CSRF probes covering the *actual* surface
(currently 94 tenant-scoped endpoints) rather than a hand-maintained
subset that drifts.

## Probes

All probes share `lib.sh`, print `PASS:` / `FAIL:` / `SKIP:` lines,
end with a `pass=/fail=/skip=` tally, and exit non-zero if any check
fails — so they are CI-gateable. With no `TARGET` set they exit 0
after a `SKIPPED` note.

### 1. IDOR — `idor-check.sh`

Verifies tenant isolation: tenant A's token must not read tenant B's
data through any tenant-scoped endpoint. For each endpoint it
substitutes the victim tenant id into the tenant slot and calls it
with the attacker's token. A correctly isolated endpoint returns
**401/403/404** — any **2xx** is a finding.

```sh
TARGET=https://staging.kmail.example \
ATTACKER_TOKEN="<bearer for tenant A>" \
VICTIM_TENANT="<tenant B id>" \
scripts/security/idor-check.sh
```

### 2. SQL injection — `sqli-scan.sh`

Injects a curated payload set into path parameters of every
tenant-scoped endpoint and asserts the server never **500s** and
never leaks a database error string (`pq:`, `SQLSTATE`, `syntax
error at or near`, …). KMail uses parameterised queries + RLS
throughout, so the expected result is a clean 4xx.

```sh
TARGET=https://staging.kmail.example \
TOKEN="<bearer>" \
scripts/security/sqli-scan.sh
```

### 3. CSRF — `csrf-check.sh`

KMail authenticates with Bearer/OIDC tokens, not ambient cookies, so
a forged cross-site request cannot attach credentials. This probe
confirms that property: a state-changing `POST` carrying an
attacker `Origin` and a planted cookie but **no Authorization
header** must be rejected with 401/403. Any 2xx is a finding.

```sh
TARGET=https://staging.kmail.example scripts/security/csrf-check.sh
```

### 4. OWASP ZAP baseline — `zap-baseline.sh` + `zap-baseline.conf`

Runs the official ZAP baseline (passive) scan via the ZAP container.
`zap-baseline.conf` promotes the security headers KMail now enforces
(HSTS, CSP, X-Frame-Options, X-Content-Type-Options,
Permissions-Policy — see `internal/middleware/security.go`) to
**FAIL** level, and ignores cookie rules that don't apply to a
token-auth JSON API.

```sh
TARGET=https://staging.kmail.example scripts/security/zap-baseline.sh
# report written to ./zap-report/zap-report.{html,json}
```

## Suggested CI wiring (non-blocking → blocking)

Run these as a dedicated `security` stage against an ephemeral
deployment. Start non-blocking (collect findings), then flip to
blocking once the baseline is clean:

1. `gen-endpoints.sh` — fail the build if it produces a diff (route
   added without the inventory being regenerated).
2. `idor-check.sh`, `csrf-check.sh` — block: these encode hard
   security invariants.
3. `sqli-scan.sh` — block on 500/DB-leak.
4. `zap-baseline.sh` — start non-blocking; review the report.

## Relationship to SOC 2

The pentest summary is referenced from the SOC 2 control mapping
(`docs/compliance/SOC2_CONTROL_MAPPING.md`, CC7.x). A clean run of
these probes plus an annual third-party pentest is the evidence for
the external-boundary and detection controls.
