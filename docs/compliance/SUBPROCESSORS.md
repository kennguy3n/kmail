# KMail Sub-Processors

The following entities process personal data on behalf of KMail
in the course of providing the service.

| Sub-Processor | Purpose | Location | Safeguards |
|---------------|---------|----------|------------|
| **Stalwart Labs (Stalwart Mail Server v0.16.0)** | Mail delivery, JMAP/IMAP/SMTP/CalDAV/CardDAV | Self-hosted on KMail infrastructure | Operated under KMail's controls, no third-party transfer |
| **zk-object-fabric (Wasabi-backed)** | Encrypted blob storage for mail bodies and attachments | EU + US regions | Customer-managed encryption envelopes; Privacy-plan tenants supply CMK |
| **PostgreSQL (managed)** | Control-plane state (tenants, users, audit log, billing, retention) | Same region as Stalwart shard | RLS for tenant isolation; encryption at rest |
| **Meilisearch (self-hosted)** | Mailbox full-text search index | Same region as Stalwart shard | Per-tenant index; encryption at rest |
| **Valkey** | Rate limiting, push session cache, calendar reminder queue | Same region as Stalwart shard | Ephemeral data; TTL-bound entries |
| **KChat** | OIDC identity provider, customer organisation (parent platform) | Customer-selected region | OIDC token exchange only; no mail content shared |
| **Stripe** | Billing + invoicing (when enabled) | EU + US | PCI-DSS Level 1; customer cardholder data never traverses KMail |
| **Cloud provider** | IaaS (compute, network, S3-compatible object storage backing zk-object-fabric) | Customer-selected region | SOC 2 Type II + ISO 27001 inherited |

## Notification of Changes

KMail notifies the Controller's billing contact at least
**30 days** in advance before authorising a new sub-processor.
The list above is also surfaced in-product under
**Settings → Compliance → Sub-processors**.

## Right to Object

The Controller may object to a new sub-processor on reasonable
grounds (e.g. the new entity is in a jurisdiction that the
Controller's legal team has flagged). KMail will work with the
Controller on a mutually acceptable alternative; if no
alternative is reasonably available, the Controller may
terminate the affected portion of the service.
