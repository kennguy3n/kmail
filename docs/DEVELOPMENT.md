# KMail — local development guide

This document is the single entry-point for getting a KMail dev stack
booted, Stalwart configured against the shared infrastructure, and a
mail round-trip verified end-to-end. It supersedes any boot-order
notes sprinkled through `README.md`, `docs/PROPOSAL.md`, and
`docs/ARCHITECTURE.md`.

For the broader architectural context (phase gates, shard topology,
privacy mode ↔ zk-object-fabric mode matrix) start in
[`docs/PROPOSAL.md`](./PROPOSAL.md) and
[`docs/ARCHITECTURE.md`](./ARCHITECTURE.md). For the list of
currently-Met gates, see [`docs/PROGRESS.md`](./PROGRESS.md).


## 1. What the compose stack gives you

Running `docker compose up` from the repo root brings up every piece
of infrastructure Stalwart and the Go control-plane depend on:

| Service      | Image                                | Host port(s)      | Role                                                 |
| ------------ | ------------------------------------ | ----------------- | ---------------------------------------------------- |
| `postgres`   | `postgres:16`                        | `5432`            | Control-plane metadata + Stalwart v0.16.0 data store |
| `meilisearch`| `getmeili/meilisearch:v1.10`         | `7700`            | Phase 2 search tier                                  |
| `valkey`     | `valkey/valkey:8`                    | `6379`            | Short-TTL state / cache                              |
| `zk-fabric`  | built from `../zk-object-fabric`     | `9080`, `9081`    | S3 gateway (blob store) + console API                |
| `stalwart`   | `stalwartlabs/stalwart:v0.16.0`      | `25/465/587/143/993/8080` | Mail core (SMTP, IMAP, JMAP, admin UI) |

The Go BFF (`cmd/kmail-api`) and the Vite dev server for `web/` are
**not** in compose — you run those directly on your host once the
compose stack is healthy.


## 2. Stalwart v0.16.0 — configuration schema change

This repo started out against the pre-v0.16.0 TOML schema; everything
below describes why the on-disk layout changed in this version and
what the compose stack does to work around it.

1. **Bootstrap config** — a single JSON file at
   `/etc/stalwart/config.json` tells Stalwart *only* which data store
   to use. `configs/stalwart/config.json` is mounted there and points
   at the dedicated `stalwart` Postgres database.
2. **Everything else** — listeners, blob stores, domains, users,
   DKIM keys, directories, Sieve scripts — lives inside the data
   store. v0.16.0 exposes the full admin registry over JMAP as
   `x:<ObjectType>/{get,set,query}` method calls on the regular
   `/jmap` endpoint (Basic auth against the recovery admin works),
   so `scripts/stalwart-init.sh` writes the blob / in-memory /
   search stores and the dev tenant domain into Postgres
   automatically on `docker compose up`. No admin-UI wizard is
   involved. `configs/stalwart.toml` is retained in the tree only
   as a read-only reference for the values the init script bakes
   in.

The `stalwart` database and `stalwart` login role are provisioned
automatically on the first `docker compose up` against a fresh
`postgres_data` volume by
[`scripts/postgres-init-stalwart.sql`](../scripts/postgres-init-stalwart.sql),
which Postgres's entrypoint runs out of
`/docker-entrypoint-initdb.d/`. Stalwart then connects, creates its
schema (~30 tables in the `public` schema of the `stalwart`
database), and serves the admin UI on host `:8080`.


## 3. Quick start

```bash
# One-time: make sure the sibling zk-object-fabric repo is checked
# out next to kmail so the compose file's build context resolves.
git -C .. clone https://github.com/kennguy3n/zk-object-fabric.git

# Boot the full stack.
docker compose up -d

# Wait for everything to become healthy (~10-15s).
docker compose ps
```

Expected output:

```
kmail-postgres     Up (healthy)
kmail-zk-fabric    Up (healthy)
kmail-valkey       Up
kmail-meilisearch  Up
kmail-stalwart     Up (healthy)
```

There is no setup wizard to walk through — `stalwart-init` runs
the configuration for you. See §4 for what it does under the hood.

Once the compose stack is up, start the Go BFF and the Vite dev
server on your host. The BFF refuses to boot in production-shaped
configurations (no JWKS issuer, or a dev bypass token wired in)
unless `KMAIL_ENV=development` is exported, so the local shell
needs the flag:

```bash
# In one shell — the Go BFF.
export KMAIL_ENV=development                # unlocks unverified-JWT fallback + dev bypass
export KMAIL_DEV_BYPASS_TOKEN=dev-token      # optional; only honoured when KMAIL_ENV=development
go run ./cmd/kmail-api

# In another shell — the React frontend.
cd web && npm install && npm run dev
```

`KMAIL_ENV` defaults to `production` (see `internal/config/config.go`)
so that a misconfigured deployment fails closed. Recognised values
are `development`, `staging`, and `production`; the shorthand
aliases `dev`, `stg`, and `prod` (matching the `docker-compose.yml`
sidecar convention) are accepted and resolve to the canonical
forms before any guard is evaluated. Anything else is treated as
`production` and `NewOIDC` emits an operator-facing warning at
startup.

#### Migration note: OIDC fail-closed (production / staging)

As of the Phase A hardening, `NewOIDC` refuses to construct when
`KCHAT_OIDC_ISSUER` is empty *and* `KMAIL_ENV` is anything other
than `development` / `dev`. Existing deployments that relied on
the unverified-JWT fallback to boot will now fail with a startup
error like:

```
NewOIDC: KCHAT_OIDC_ISSUER is required when KMAIL_ENV=%q
```

To migrate:

1. Point `KCHAT_OIDC_ISSUER` (and `KMAIL_KCHAT_OIDC_ISSUER` in
   the Helm ConfigMap) at the KChat OIDC issuer URL — the same
   URL the KChat Authelia / Keycloak install advertises in its
   `/.well-known/openid-configuration` document.
2. Verify the JWKS endpoint is reachable from the BFF pods (the
   chart's egress NetworkPolicy already allows DNS + the issuer
   host).
3. Re-roll the kmail-api deployment.

There is no way to opt out of this check in staging or
production — the dev bypass and unverified fallback are
hard-locked behind `KMAIL_ENV=development`. This is intentional:
both paths were authentication-bypass vectors before the Phase A
fix. See `docs/JMAP-CONTRACT.md` "OIDC fail-closed" for the
on-the-wire details.

### Scaling Stalwart with mTLS enabled

When `mtls.enabled=true` in the Helm values, the server
certificate SANs are generated at template-render time from
`stalwart.replicaCount` (see
`deploy/helm/kmail/templates/stalwart-mtls.yaml`). Scaling the
StatefulSet *without* a corresponding `helm upgrade` will leave
the new replicas (e.g. `stalwart-2`, `stalwart-3`) presenting a
certificate whose SAN list excludes their pod DNS names, and
BFF→Stalwart handshakes to those pods will fail with `x509:
certificate is valid for X, not Y`.

The correct procedure is therefore:

```bash
# WRONG — leaves SAN list stale, new pods fail TLS handshake.
kubectl scale statefulset/kmail-stalwart --replicas=4

# RIGHT — re-renders Certificate resource, cert-manager reissues
# with all four pod DNS names in the SAN list, Reloader restarts
# the existing pods to pick up the new cert.
helm upgrade kmail ./deploy/helm/kmail \
  --reuse-values \
  --set stalwart.replicaCount=4
```

The BFF logs a startup WARNING if it detects an mTLS + bare
`.svc` hostname mismatch (see `internal/jmap/proxy.go`
`NewProxy`), so a misconfigured override of `KMAIL_STALWART_URL`
also surfaces immediately rather than after the first request.

In production the BFF presents a client certificate to Stalwart
(mTLS) instead of relying on a trusted-network header. The Helm
chart wires this up via cert-manager; local dev keeps using
plain HTTP against `http://localhost:8080`.

### cert-manager Issuer must emit `ca.crt`

The BFF reads its trust anchor from `/etc/kmail/tls/stalwart-client/ca.crt`
(via `KMAIL_STALWART_TLS_CA`, set by
`deploy/helm/kmail/templates/deployment-api.yaml`). The file is
populated by cert-manager into the client-cert Secret — but only
when the configured `Issuer` (or `ClusterIssuer`) actually
provides the CA. Most in-cluster Issuers (`ca`, `vault`, `step-ca`,
`selfsigned` with `isCA: true`) do; some external ACME Issuers do
*not* attach a `ca.crt` key to the issued Secret.

If the chosen Issuer does not emit `ca.crt`, the BFF will fail at
startup with `cmk/jmap: open /etc/kmail/tls/stalwart-client/ca.crt:
no such file or directory` from `caPoolLoader.load()` in
`internal/jmap/proxy.go`. This is the intended fail-fast — there
is no safe default for a trust anchor.

Resolutions, in order of preference:

1. **Switch to an Issuer that bundles the CA**, since trust-anchor
   management belongs with cert-manager. Most production setups
   use the in-cluster `ca` Issuer chained to a long-lived root.
2. **Mount the CA bundle separately**: create a `ConfigMap`
   holding the internal-PKI root, add it via Helm `extraVolumes` /
   `extraVolumeMounts` at the same `/etc/kmail/tls/stalwart-client`
   path. This is the operator-managed escape hatch when the
   Issuer is fixed by external constraints (compliance, audit).

Either approach yields the same on-disk contract; the BFF does
not care how the file arrived.


## 4. Automated first-boot configuration

The `stalwart-init` one-shot container in `docker-compose.yml` runs
`scripts/stalwart-init.sh` once per `docker compose up` and drives
Stalwart's admin registry over JMAP with Basic auth using the
`STALWART_RECOVERY_ADMIN` recovery credentials (compose passes the
password through via `STALWART_ADMIN_PASSWORD`). On a fresh volume
it performs these writes in order:

| Object                | Method                 | Configures                                                                   |
| --------------------- | ---------------------- | ---------------------------------------------------------------------------- |
| `x:BlobStore/set`     | `update singleton`     | `@type: "S3"` pointed at `http://zk-fabric:8080` with `kmail-blobs` bucket.  |
| `x:InMemoryStore/set` | `update singleton`     | `@type: "Redis"` pointed at `redis://valkey:6379`.                           |
| `x:SearchStore/set`   | `update singleton`     | `@type: "Meilisearch"` pointed at `http://meilisearch:7700` + Bearer key.    |
| `x:Domain/set`        | `create` (if missing)  | Dev tenant domain `kmail.dev`.                                               |

Stalwart v0.16.0 auto-creates the `BlobStore` / `InMemoryStore` /
`SearchStore` singletons as `Default` (Postgres-backed) variants on
first boot, so each upsert is an `update` on id `"singleton"` —
idempotent by design. The network listeners (SMTP 25 / 465 / 587,
IMAP 143 / 993, HTTP 8080 for JMAP + admin) are created
automatically by Stalwart with the standard bindings; the script
doesn't touch them.

### The first-boot restart

Stalwart resolves the concrete blob / in-memory / search backends
from the registry **only at startup**. The `/set` writes above land
in Postgres but don't swap the live pointer the mail core is
holding, so on a brand-new volume the blob store is still the
Postgres default after the script returns. The script closes this
loop by bind-mounting `/var/run/docker.sock` and issuing a
`POST /containers/kmail-stalwart/restart` against the Docker Engine
API. After the restart (~5 s) Stalwart reads the singletons on
startup and all blob writes flow to zk-object-fabric. On subsequent
`docker compose up` runs the restart still fires but is effectively
free — Stalwart had already resolved the right backends on its
previous boot. The `stalwart-init` container runs as
`user: "0:0"` so it can read the socket, which is owned by
`root:docker` on the host.

### Dev tenant domain

The dev tenant uses `kmail.dev` (a real TLD owned by Google) rather
than `kmail.local` — Stalwart v0.16.0's domain validator rejects
the RFC 2606 / mDNS suffixes (`.local`, `.test`,
`localhost.localdomain`). Override with the
`KMAIL_DEV_TENANT_DOMAIN` environment variable if you want a
different dev domain.


## 5. Smoke tests

### Blob round-trip through zk-object-fabric

```bash
# Upload an arbitrary blob through Stalwart's JMAP /upload endpoint.
echo 'kmail zk-fabric smoke test' | \
  curl -sS -u "admin:$STALWART_ADMIN_PASSWORD" -H 'Content-Type: text/plain' \
    --data-binary @- http://localhost:8080/jmap/upload/d333333

# Expected: {"accountId":"d333333","blobId":"...","type":"text/plain","size":27}

# Confirm it landed in the zk-fabric bucket.
AWS_ACCESS_KEY_ID=kmail-access-key \
AWS_SECRET_ACCESS_KEY=kmail-secret-key \
AWS_DEFAULT_REGION=us-east-1 \
  aws --endpoint-url http://localhost:9080 \
      s3api list-objects-v2 --bucket kmail-blobs \
      --query 'Contents[].{Key:Key,Size:Size}'
```

You should see at least one 27-byte object, plus any blobs Stalwart
wrote on startup (the admin SPA bundle is ~550 KB and shows up as
soon as the admin UI is opened). `GET
/jmap/download/d333333/{blobId}/filename.txt` round-trips the same
bytes back.

### Stalwart admin UI

Open http://localhost:8080/admin/ and log in as `admin` with the
password compose pins via `STALWART_RECOVERY_ADMIN`. The dashboard
drops you straight onto the configured server — no wizard.


## 5a. Third-party client configuration

Stalwart's IMAP, SMTP submission, and CalDAV surfaces are enabled
by `scripts/stalwart-init.sh` and mapped to the following host
ports by `docker-compose.yml`:

| Protocol           | Port(s)       | Notes                                  |
| ------------------ | ------------- | -------------------------------------- |
| SMTP (MX)          | 25            | Inbound only; MTA-to-MTA traffic.      |
| SMTP submission    | 587 (STARTTLS), 465 (implicit TLS) | Client send path. |
| IMAP               | 143 (STARTTLS), 993 (implicit TLS) | Folder sync.      |
| CalDAV / CardDAV   | 8080 over HTTPS in prod; plain HTTP on dev | Served under `/dav/`. |

### Thunderbird

1. Account setup → "Email account" → enter any `@kmail.dev`
   dev-tenant mailbox + password.
2. Manual config:
   - **Incoming**: IMAP, server `localhost`, port `143`, STARTTLS,
     normal password auth.
   - **Outgoing**: SMTP, server `localhost`, port `587`, STARTTLS,
     normal password auth.
3. Thunderbird's Lightning / TB Calendar add-on — "New calendar" →
   "On the network" → CalDAV → `http://localhost:8080/dav/` →
   pick the default calendar.
4. Autoconfig / autodiscover is not wired in dev; production
   deployments can point `autoconfig.<domain>` and
   `autodiscover.<domain>` at the BFF if they want Thunderbird's
   "just the email address" flow.

### Apple Mail / Calendar

1. System Settings → Internet Accounts → "Add Other Account…" →
   Mail account.
2. IMAP server `localhost`, port `143`, STARTTLS. SMTP server
   `localhost`, port `587`, STARTTLS. Username = full address.
3. Calendar account → CalDAV → "Manual" → username + password,
   server address `localhost:8080`. Enter the CalDAV path when
   prompted (`/dav/calendars/<principal>/`).

### Known limitations (Stalwart v0.16.0)

- No XOAUTH2 yet; clients must use plain password auth over TLS.
- SMTP submission supports SIZE, 8BITMIME, PIPELINING,
  STARTTLS, AUTH PLAIN/LOGIN, but not BURL or DSN.
- IMAP exposes IDLE, CONDSTORE, MOVE, LIST-EXTENDED, but SORT is
  unoptimised — prefer `THREAD` on large mailboxes.

## 5b. Spam / phishing filter

`scripts/stalwart-init.sh` enables Stalwart's built-in spam
classifier on first boot. The declarative snapshot of what the
init script pushes lives at
[`configs/stalwart/spam-config.json`](../configs/stalwart/spam-config.json).

- **Scoring**: `spam ≥ 5.0` moves to Junk, `≥ 10.0` silently
  discards, `≥ 15.0` rejects at MAIL FROM.
- **Bayesian** classifier trains on the JMAP `$junk` / `$notjunk`
  keywords the React UI sets from the per-row Spam / Not spam
  button.
- **DNSBL**: Spamhaus Zen, SpamCop, Spamhaus DBL, SURBL.
- **Phishing**: URL reputation + sender authentication checks.
- **Sieve**: the trusted script auto-files anything tagged
  `X-Spam-Status: Yes` into the per-principal Junk mailbox.

To smoke-test: paste the
[GTUBE](https://spamassassin.apache.org/gtube/) string into a
message body, send it through SMTP :587 from an allow-listed
sender, and confirm the message lands in Junk (not Inbox).

## 6. What's deliberately still manual

The init script configures the infrastructure (blob / in-memory /
search stores and the tenant domain) but intentionally does **not**
create a mail user or seed the control-plane Postgres with the
`(tenant_id, kchat_user_id, stalwart_account_id)` mapping the Go
BFF expects. Sending mail through the Compose UI end-to-end
therefore still requires:

1. A Stalwart `Individual` (mail user) on `kmail.dev`, created via
   the admin SPA at http://localhost:8080/admin/.
2. A matching row in the control-plane `users` table linking the
   dev-bypass identity (`tenant_id =
   00000000-0000-0000-0000-000000000000`, `kchat_user_id =
   dev-user`) to that Stalwart account id.

Both steps are tracked as a Phase 2 follow-up — see
[`docs/PROGRESS.md`](./PROGRESS.md). The blob-storage verification
above doesn't depend on either step.


## 7. Troubleshooting

### `Failed to parse data store settings at /etc/stalwart/config.json`

Happens when the TOML in `configs/stalwart.toml` gets mounted at
`/etc/stalwart/config.json` by accident (v0.15 wiring). Stalwart
v0.16.0 insists on JSON at that path. Check `docker-compose.yml`'s
`stalwart` service mounts only `configs/stalwart/config.json` there.

### Admin UI OIDC redirect sends browsers to `http://<container-id>:8080`

Happens when the container hostname isn't pinned. `docker-compose.yml`
sets `hostname: localhost` on the `stalwart` service and publishes
container port 8080 onto host port 8080 specifically so the
self-advertised OIDC issuer resolves back to the same origin the
browser is already on.

### Port 8080 conflict with the Go BFF

The BFF defaults to `KMAIL_API_ADDR=:8088` (set in
`internal/config/config.go`) so a host-run `go run ./cmd/kmail-api`
does NOT collide with Stalwart, which `docker-compose.yml` publishes
on host port 8080. The Vite dev server's `/jmap` proxy in
`web/vite.config.ts` is already pointed at `http://localhost:8088`
to match. If you override `KMAIL_API_ADDR` to bind a different port
in your shell, update the Vite proxy target to match. The BFF's
internal Stalwart target stays `http://stalwart:8080` over the
compose network — that's unaffected.

### Running the BFF on the host

Because host port 8080 is taken by Stalwart in compose, the BFF
binds `:8088` by default when run on the host. To start it
alongside a running `docker compose up`:

```
go run ./cmd/kmail-api
# listens on http://localhost:8088, proxies JMAP to
# http://localhost:8080 (the compose-published Stalwart port).
```

If you want the BFF to listen on a different port set
`KMAIL_API_ADDR=:9090` (or whatever) in your shell. The internal
proxy target (`STALWART_URL`) defaults to `http://localhost:8080`
which is the docker-compose host publish; inside compose,
override it to `http://stalwart:8080` (the service name on the
internal network).

### Setup wizard lost; want to reset the stack

```bash
docker compose down -v   # drops postgres_data + stalwart_data
docker compose up -d
```

This re-runs `postgres-init-stalwart.sql` and drops you back onto the
fresh wizard with `admin / kmail-dev`.

## 8. Retention enforcement opt-out

The retention worker (`internal/retention/worker.go`) ticks daily and
applies every tenant's policies. As of Phase 6 it runs in **live
mode by default** — emails older than a policy's retention horizon
are destroyed (or archived) for real.

Operators can switch the worker back to dry-run for a deployment by
setting:

```bash
KMAIL_RETENTION_DRY_RUN=true
```

In dry-run mode the worker still walks every policy and writes a row
to `retention_enforcement_log` with `notes = 'dry_run=true'`, but it
does not call `Email/set destroy` or move blobs. The admin UI status
card on **Retention → Enforcement** reads the same flag and renders
a banner so operators always know which mode is active. The
Prometheus counters (`kmail_retention_emails_deleted_total`,
`kmail_retention_emails_archived_total`,
`kmail_retention_evaluations_total`,
`kmail_retention_errors_total`) tick in both modes — `*_deleted_*` /
`*_archived_*` stay at zero in dry-run, which is the canonical
"safety check" before flipping back to live.

## 9. Loki + Promtail log shipping

Phase 7 adds an optional Loki + Promtail stack so operators can
query the BFF's structured-JSON request log alongside the
existing Prometheus metrics. Everything lives behind the `loki`
compose profile — `docker compose up` keeps working without
shipping any logs:

```bash
# Start KMail with the Loki stack attached.
docker compose --profile loki up -d

# Tail the Promtail and Loki containers to confirm ingest.
docker compose logs -f loki promtail
```

Loki listens on port `3100`. The bundled Grafana datasource
config (`deploy/grafana/datasources.yml`) provisions Loki +
Prometheus into a freshly mounted Grafana image so an operator
running `grafana/grafana:11` against
`/etc/grafana/provisioning/datasources/datasources.yml` gets
both datasources without clicking through the UI.

Promtail's pipeline (`deploy/promtail/promtail.yml`) pulls
`tenant_id`, `route`, `status_class`, and `method` out of every
JSON log line emitted by `internal/middleware/logger.go`. Set
`KMAIL_LOG_FORMAT=json` on the BFF (the default in the compose
stack) so the lines have the right shape; the request logger
falls back to text format otherwise and Promtail will refuse the
records.

### Pre-built Grafana dashboards (Phase 8)

The `loki` profile now boots a Grafana 11 container at
http://localhost:3000 (anonymous Viewer enabled, admin
credentials `admin` / `kmail-dev`). Two pre-built dashboards
live under `deploy/grafana/dashboards/` and are loaded
automatically by the provisioner at
`deploy/grafana/provisioning/dashboards.yml`:

- **KMail — Overview** (`kmail-overview`): request rate / P50–P99
  latency, active tenants, seats by plan, JMAP proxy latency,
  retention worker counters, the rolling 30-day availability SLO
  gauge, and a Loki panel for recent kmail-api errors.
- **KMail — Deliverability** (`kmail-deliverability`): bounce
  rate (hard / soft), complaint rate per IP pool, suppression
  list size, DMARC pass rate, IP pool reputation, warmup
  progress, abuse score distribution, and a Loki panel for
  bounce / complaint events.

To edit a dashboard locally, change the JSON on disk; the
provisioner re-reads `/var/lib/grafana/dashboards` every 30
seconds, so the change shows up without restarting Grafana.
