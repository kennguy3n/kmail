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

The BFF historically binds host `:8080` during local development. For
running both concurrently, start the BFF with
`--addr=127.0.0.1:8088` and update `web/vite.config.ts`'s `/jmap`
proxy target to `http://localhost:8088`. The BFF's internal Stalwart
target stays `http://stalwart:8080` over the compose network —
that's unaffected.

### Setup wizard lost; want to reset the stack

```bash
docker compose down -v   # drops postgres_data + stalwart_data
docker compose up -d
```

This re-runs `postgres-init-stalwart.sql` and drops you back onto the
fresh wizard with `admin / kmail-dev`.
