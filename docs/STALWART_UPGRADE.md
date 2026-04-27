# Stalwart upgrade runbook

KMail pins Stalwart to a tested version (currently `v0.16.0` — see
`README.md` and the version pin note in
`docs/PROPOSAL.md` §1). Stalwart v1.0.0 is expected H1 2026 and
is the first major release with breaking JMAP / admin-registry
changes since the KMail BFF was wired up. This runbook is the
load-bearing document for moving the production pin from one
version to another. Read it end-to-end before starting an upgrade.

## Version matrix

| KMail release | Stalwart pin | Notes |
| ------------- | ------------ | ----- |
| ≤ Phase 6 | `v0.16.0` | Current pin. Initialised by `scripts/stalwart-init.sh`. |
| Phase 7 staging | `v0.16.0` + `v1.0.0` blue/green | Compatibility shim (`internal/jmap/compat.go`) detects which shape each shard speaks. |
| Phase 7+ | `v1.0.0` | Production pin moves once both staging legs pass `scripts/test-stalwart-upgrade.sh` and a 24h soak. |

The compatibility shim is intentionally permissive: an unreachable
or unparseable `Server:` / `stalwartVersion` falls back to the
v0.16.0 shape. That keeps the BFF up during a partial upgrade.

## Known breaking changes between v0.16.0 and v1.0.0

These are the deltas that drive the shim. Every entry here has a
corresponding test in `internal/jmap/compat_test.go`.

1. **Capability URN namespace.** v0.16.0 advertises and accepts
   `urn:stalwart:<feature>` URNs. v1.0.0 advertises IANA-shaped
   `urn:ietf:params:jmap:<feature>` URNs but still accepts the
   legacy form for at least one major release. Adapter:
   `Adapter.AdaptCapabilityURN` rewrites legacy URNs when the
   detected upstream is v1+.
2. **Admin method namespace.** v0.16.0 uses `x:<Type>/<verb>`
   (`x:Domain/set`, `x:BlobStore/set`). v1.0.0 moves them under
   `urn:stalwart:admin:<Type>/<verb>`. The wire shape of the
   request (envelope, `using`, `methodCalls`, args) is unchanged.
   Adapter: `Adapter.AdaptAdminMethod`.
3. **JMAP error envelope.** v1.0.0 adds an integer `code` field
   alongside the existing `type` / `description` fields. The BFF
   keeps reading `type` as the canonical error tag and uses
   `Adapter.AdaptErrorEnvelope` to backfill `type` from `code`
   if a future revision drops `type`.
4. **`stalwartVersion` in session.** v1.0.0's JMAP session
   response includes a `stalwartVersion` string at the top level
   so consumers no longer need to parse the `Server:` header.
   `VersionDetector.Detect` reads either source.

The init scripts mirror the same boundary:

- `scripts/stalwart-init.sh` — current `v0.16.0` initialiser.
- `scripts/stalwart-init-v1.sh` — parallel `v1.0.0` initialiser
  documenting the assumed admin namespace move. Switch the
  compose `stalwart-init` one-shot to this script when the pin
  moves.

## Pre-flight checklist

Before staging an upgrade:

1. Confirm Stalwart upstream has published the target image
   (e.g. `stalwartlabs/stalwart:v1.0.0`).
2. Read the upstream changelog and the `stalwartlabs/mail-server`
   migration guide. Update this document if any breaking change
   is not yet covered above.
3. Run `make test` against `main`. Phase 7 adds
   `internal/jmap/compat_test.go` to the suite — those tests
   must be green.
4. On a staging cluster, run
   `STALWART_V1_IMAGE=stalwartlabs/stalwart:v1.0.0 \
    scripts/test-stalwart-upgrade.sh`. The script boots both
   versions sequentially and runs `scripts/test-e2e.sh` against
   each. Exit code 0 = both legs green.
5. Confirm that the JMAP proxy (`internal/jmap/proxy.go`) and
   admin-registry callers exercise the shim through real traffic
   (smoke against staging for at least 30 minutes).

## Rollout (blue/green)

The default rollout is a per-shard blue/green:

1. Capture a Postgres + Stalwart snapshot. The Phase 6 retention
   counters (`kmail_retention_*`) and the SLO tracker establish
   the baseline.
2. Pick one shard (lowest tenant count). Bring up a *new* node in
   the shard's pool running the v1.0.0 image and the parallel
   `scripts/stalwart-init-v1.sh` initialiser. Leave the existing
   v0.16.0 nodes serving live traffic.
3. Repoint the shard's `tenant.shards.url` to the v1.0.0 node.
   Old nodes drain. The compatibility shim handles per-request
   versioning for any straggling caller.
4. Soak for 24h. Watch the SLO dashboard, the deliverability
   bounce metrics, and the retention counters for regressions.
5. Repeat per shard.
6. Once every shard is on v1.0.0, switch the compose
   `stalwart-init` to `scripts/stalwart-init-v1.sh` and update
   `README.md` / `docs/PROPOSAL.md` §1 to pin v1.0.0.

## Rollback

If any shard fails the soak:

1. Repoint `tenant.shards.url` back to the v0.16.0 node. The
   compat shim makes this a hot swap — no BFF restart required.
2. Capture the failure logs from the v1.0.0 node and the BFF (the
   structured-JSON request log carries `stalwart_version` once
   the shim resolves it).
3. File the incident in `docs/PROGRESS.md` and the upstream
   issue tracker. Pause the upgrade for the rest of the fleet
   until the regression is fixed.
4. Restore the Postgres / Stalwart snapshot only if data
   corruption is suspected — a control-plane regression alone
   never warrants a snapshot restore.

## Operational notes

- The JMAP proxy logs the detected version once per shard URL at
  `INFO`; subsequent requests reuse the cached value (`5m` TTL).
- The compat shim's TTL is intentionally short so a hot rollback
  recovers within the next probe.
- Do **not** auto-upgrade in CI. The version pin is the
  authoritative source of truth. Any change must come with an
  explicit revision of this runbook.
