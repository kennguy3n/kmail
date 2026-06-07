# 0007 — zk-object-fabric for zero-knowledge blob storage

- **Status**: Accepted
- **Related**: [`../DEVELOPMENT.md`](../DEVELOPMENT.md), [`../compliance/SECURITY_OVERVIEW.md`](../compliance/SECURITY_OVERVIEW.md) §3, `internal/tenant/zkfabric.go`

## Context

Mail bodies and attachments are large binary blobs that must be stored
durably and cheaply. For a privacy-first product, the storage layer
should not be able to read customer content, and each tenant's blobs
should be cryptographically separable so a customer-managed key can
revoke access.

## Decision

Store blobs in **zk-object-fabric**, an S3-compatible object store that
acts as a zero-knowledge gateway: data is encrypted with per-tenant
envelope keys before it reaches storage, so the fabric never sees
plaintext. Each tenant is provisioned its own bucket and S3 credentials
at tenant-creation time (`ZKFabricProvisioner.Provision` /
`internal/tenant/zkfabric.go`); Stalwart's per-tenant `BlobStore`
record points at those credentials. In dev, the sibling
`zk-object-fabric` repo runs in compose; in production it is a shared,
multi-AZ, versioned service.

## Consequences

- The storage layer is untrusted-by-design — it holds only ciphertext.
- Per-tenant keying enables customer-managed keys and crypto-shredding:
  revoking a tenant's key cuts off access without bulk deletion.
- Tenant provisioning has a hard dependency on minting bucket + keys
  before the tenant can accept mail (part of the shard provisioning
  flow in [ADR 0006](./0006-stalwart-on-long-lived-hosts.md)).
- Backup/restore must keep blob versions and the control-plane DB at a
  consistent point in time (see
  [backup & restore](../operator/backup-restore.md#zk-object-fabric-mail-blobs)).
