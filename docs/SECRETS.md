# Secrets Management

KMail's BFF reads a number of secrets at boot — the kmail-secrets
master key, the Stripe webhook secret, the KChat API token, OIDC
client material, and so on. Historically every one of these was a
plain environment variable. That is fine for local dev and for a
Kubernetes `Secret` mounted as env, but production deployments
usually want secrets sourced from a dedicated secret manager and
want the master key to be rotatable without downtime.

The `internal/secrets` package provides both:

1. A **pluggable provider** abstraction for sourcing secret *values*
   (env today; HashiCorp Vault built in; AWS Secrets Manager / SOPS
   registerable without bloating the core binary).
2. A **rotation-aware envelope** for the kmail-secrets master key so
   `KMAIL_SECRETS_KEY` can be rotated with zero failed reads.

---

## 1. Pluggable secret providers

A provider resolves a backend-specific *reference* to a plaintext
value:

```go
type Provider interface {
    Resolve(ctx context.Context, ref string) (string, error)
    Backend() string
}
```

The backend is selected by `KMAIL_SECRETS_BACKEND` (default `env`),
with backend-specific configuration in `KMAIL_SECRETS_BACKEND_CONFIG`.

### `env` (default)

References are environment variable names. Resolution tries the
`KMAIL_`-prefixed name first, then the bare name — matching the
lookup `internal/config` already uses, so a reference works whether
the Helm chart set `KMAIL_FOO` or a dev shell exported `FOO`.

```
KMAIL_SECRETS_BACKEND=env          # implicit default
```

### `vault` (HashiCorp Vault KV v2)

References have the form `mount/path#field` (the field defaults to
`value` when omitted). The address comes from
`KMAIL_SECRETS_BACKEND_CONFIG` (or `VAULT_ADDR`); the token from the
`VAULT_TOKEN` secret.

```
KMAIL_SECRETS_BACKEND=vault
KMAIL_SECRETS_BACKEND_CONFIG=https://vault.internal:8200
VAULT_TOKEN=s.xxxxxxxx

# reference example
secret/kmail/prod#stripe_webhook
```

Values are cached in-process for 5 minutes so a startup burst of
resolves is one round-trip per secret, not one per consumer.

The Vault backend uses only `net/http` — it does **not** pull in the
official Vault Go client, keeping the core binary's dependency tree
small.

### `aws-secrets-manager` / `sops` (registerable)

These backends require client libraries we deliberately do **not**
hard-link into the core API binary. They are added via the registry:

```go
// in a small adapter package imported by cmd/kmail-api
func init() {
    secrets.RegisterBackend("aws-secrets-manager", func(ctx context.Context, region string) (secrets.Provider, error) {
        // build an AWS SDK client scoped to `region`, return a
        // Provider whose Resolve calls GetSecretValue.
        ...
    })
}
```

The operator then selects it with `KMAIL_SECRETS_BACKEND=aws-secrets-manager`
and `KMAIL_SECRETS_BACKEND_CONFIG=<region>`. `RegisterBackend`
panics on a duplicate name so two adapters can never silently
shadow each other.

---

## 2. Zero-downtime master-key rotation

`KMAIL_SECRETS_KEY` is the single master key from which every
BFF-side at-rest encryption key derives: DKIM private keys, TOTP
shared secrets, recovery codes, HSM credentials, per-tenant S3
secret keys. Each is stored wrapped by `cmk.AESGCMEnvelope`
(`magic || nonce || ciphertext`).

### The problem with naïve rotation

`cmk.AESGCMEnvelope` is keyed by one master key. Swapping the key in
place makes every previously-wrapped row fail `Unwrap` with
`ErrEnvelopeCorrupted`, because those rows were sealed under the old
key. The only safe in-place rotation is "stop the world, re-wrap
every row, restart" — i.e. **downtime**.

### The solution: a keyring

`secrets.RotatingEnvelope` holds an ordered keyring and implements
the same `cmk.SecretsEnvelope` interface, so it is a drop-in for
every existing consumer:

- **Primary key** — `KMAIL_SECRETS_KEY` (the new key). All `Wrap`
  calls seal under this key, so new writes immediately use it.
- **Retired keys** — `KMAIL_SECRETS_KEY_RETIRED` (comma-separated
  old keys). `Unwrap` tries the primary first, then each retired key
  in turn, so rows still sealed under an old key keep decrypting.

`cmd/kmail-api` loads the envelope via `secrets.LoadEnvelope`, which
reads both variables. With no retired keys configured it behaves
identically to the previous single-key `cmk.LoadEnvelope`.

### Rotation runbook (no downtime)

1. **Generate a new 32-byte key** (hex or base64):

   ```sh
   openssl rand -base64 32
   ```

2. **Enter the dual-key window.** Set the new key as primary and
   move the *current* key to the retired list, then roll the
   deployment:

   ```
   KMAIL_SECRETS_KEY=<NEW_KEY>
   KMAIL_SECRETS_KEY_RETIRED=<OLD_KEY>
   ```

   New writes now use `NEW_KEY`; reads of old rows fall back to
   `OLD_KEY`. No read fails.

3. **Re-wrap existing rows.** Read-then-write each wrapped row
   (DKIM keys, TOTP secrets, tenant storage creds, …). Each read
   succeeds via the retired key; the write re-seals under the new
   primary. This can run lazily in the background — there is no
   deadline because step 2 keeps both keys live.

4. **Decommission the old key.** Once every row is confirmed
   re-wrapped under the new key, drop the retired variable and roll
   again:

   ```
   KMAIL_SECRETS_KEY=<NEW_KEY>
   # KMAIL_SECRETS_KEY_RETIRED removed
   ```

   The old key is now fully retired and can be deleted from the
   secret manager.

### Key encoding

Keys are accepted as hex (64 chars) or base64 (any padding) of a
32-byte value, matching `cmk.NewAESGCMEnvelopeFromKeyMaterial`.

### Failure modes

- A blob that authenticates under **no** key on the ring returns
  `cmk.ErrEnvelopeCorrupted` — surface it; do not treat it as legacy
  plaintext.
- Legacy magic-absent plaintext (written before the envelope landed)
  is still passed through unchanged on the primary attempt.
- A malformed primary key fails fast at boot, exactly like the
  single-key path did.
