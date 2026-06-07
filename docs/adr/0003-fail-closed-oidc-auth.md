# 0003 — Fail-closed OIDC authentication

- **Status**: Accepted
- **Related**: `internal/middleware/auth.go` (`NewOIDC`), [`../DEVELOPMENT.md`](../DEVELOPMENT.md) §3, [`../../deploy/helm/kmail/templates/NOTES.txt`](../../deploy/helm/kmail/templates/NOTES.txt)

## Context

KMail authenticates users with OIDC tokens from the KChat identity
provider. Earlier behaviour: if `KCHAT_OIDC_ISSUER` was unset, the auth
middleware silently fell back to accepting **unverified** JWTs so the
service could still boot. Combined with an optional dev bypass token,
this meant a misconfigured production deployment could come up with
authentication effectively disabled — an auth-bypass vector that fails
*open*.

## Decision

Make authentication **fail closed**. As of the Phase A hardening,
`NewOIDC` refuses to construct when `KMAIL_KCHAT_OIDC_ISSUER` (or bare
`KCHAT_OIDC_ISSUER`) is empty *and* `KMAIL_ENV` is anything other than
`development`/`dev`. The unverified-JWT fallback and the dev bypass
token are hard-locked behind `KMAIL_ENV=development`. `KMAIL_ENV`
defaults to `production`, and any unknown value resolves to
production semantics, so a misconfiguration fails *closed*.

## Consequences

- A misconfigured staging/production deploy **refuses to boot** with a
  clear error rather than silently running without auth. This is the
  intended signal.
- The chart ships `KMAIL_KCHAT_OIDC_ISSUER: ""` on purpose, forcing a
  deliberate operator decision (see the upgrade
  [migration gate](../operator/upgrade.md#phase-a-migration-gates)).
- Local development must export `KMAIL_ENV=development` to use the
  fallback — documented in DEVELOPMENT.md §3.
- No production opt-out exists by design; the only "escape" is the
  development environment, which is never reachable from the public
  internet in a correct deployment.
