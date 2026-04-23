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
   store and is written from the admin web UI ("setup wizard") or the
   admin API. None of it is reflected in files on disk any more, and
   `configs/stalwart.toml` is retained in the tree only as a
   reference for the follow-up that seeds those settings via the
   admin API (see §6).

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

Then open http://localhost:8080/admin/ in a browser and walk the
setup wizard (§4 below).


## 4. First-boot setup wizard

On a fresh `postgres_data` volume Stalwart has no configured
listeners, domains, or users. The admin UI detects this and walks you
through an initial-setup flow. Log in with the credentials
`docker-compose.yml` pins via `STALWART_RECOVERY_ADMIN`:

- **Username:** `admin`
- **Password:** `kmail-dev`

Walk through the five screens in order:

### 4a. Domain

- **Domain name:** `kmail.local` (or any domain you own — anything
  with a DNS A/MX record works; `kmail.local` is fine for a stack
  that only ever talks to `localhost`).

### 4b. Administrator account

Stalwart wants a permanent administrator separate from the
recovery-mode admin.

- **Account name / email:** `admin@kmail.local`
- **Password:** choose anything; you only use it for the admin UI,
  not for mail.

### 4c. Blob store — point at zk-object-fabric

This is the load-bearing step that wires the mail core into the
zk-object-fabric S3 gateway. Choose **S3-compatible** as the blob
store type and fill in:

| Field        | Value                      | Notes                                              |
| ------------ | -------------------------- | -------------------------------------------------- |
| Endpoint     | `http://zk-fabric:8080`    | Compose-internal DNS name, not `localhost`.        |
| Region       | `us-east-1`                | zk-fabric accepts anything; match the gateway.     |
| Bucket       | `kmail-blobs`              | Pre-created by the `zk-fabric-init` init container.|
| Access key   | `kmail-access-key`         | Matches the `kmail-dev` tenant in zk-object-fabric.|
| Secret key   | `kmail-secret-key`         | Matches the `kmail-dev` tenant in zk-object-fabric.|
| Path style   | on                         | zk-fabric's S3 surface uses path-style URLs.       |

These match the values `configs/stalwart.toml` records as the
canonical reference (Stalwart now ignores that file, but the values
it holds are the ones you want to paste into the wizard).

### 4d. Listeners

Accept the defaults. Stalwart will enable:

| Protocol | Container port | Host port | Notes                                       |
| -------- | -------------- | --------- | ------------------------------------------- |
| SMTP     | 25             | 25        | Inbound from other MTAs                     |
| SMTPS    | 465            | 465       | Implicit TLS submission                     |
| Submission | 587          | 587       | STARTTLS submission — the "send" port       |
| IMAP     | 143            | 143       | STARTTLS IMAP                               |
| IMAPS    | 993            | 993       | Implicit TLS IMAP                           |
| HTTP     | 8080           | 8080      | JMAP + admin UI; also serves `/.well-known/`|

### 4e. Finish

The wizard writes everything into Postgres and reloads Stalwart.
After it reloads, the admin UI drops you onto the regular dashboard
and the mail listeners are hot.


## 5. Smoke tests

### SMTP submission

```bash
swaks --to admin@kmail.local \
      --from admin@kmail.local \
      --auth PLAIN \
      --auth-user admin@kmail.local \
      --auth-password <password you set in 4b> \
      --server localhost:587 \
      --tls \
      --body "hello from the dev stack"
```

### Blob landed in zk-object-fabric

```bash
AWS_ACCESS_KEY_ID=kmail-access-key \
AWS_SECRET_ACCESS_KEY=kmail-secret-key \
aws --endpoint-url http://localhost:9080 s3 ls s3://kmail-blobs/ --recursive
```

You should see at least one new object whose size matches the raw
`.eml` payload Stalwart wrote.

### IMAP read-back

```bash
curl --ssl-reqd --user 'admin@kmail.local:<password>' \
     imaps://localhost:993/INBOX
```

or point any IMAP client (Thunderbird, `mutt`) at `localhost:993`
with the same credentials.


## 6. What's deliberately still manual

The setup wizard is walked **once per fresh `postgres_data`
volume** — after that, the settings live in Postgres and a plain
`docker compose up` boots straight into a configured server. Persist
the volume by not running `docker compose down -v`; a normal `down`
keeps the volume and skips the wizard on the next `up`.

A follow-up PR will:

1. Replace the wizard walk-through in §4 with a Go or shell seeder
   that POSTs the same values to Stalwart's admin API on first boot,
   keyed off a sentinel row so it becomes a no-op on re-runs.
2. Retire `configs/stalwart.toml` once that seeder is the source of
   truth for the blob store + listener wiring.


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
