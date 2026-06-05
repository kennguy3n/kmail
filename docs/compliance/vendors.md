# Vendor Management Register

Maps to SOC 2 **CC9.1** (vendor risk) and **CC3.2** (vendor / sub-
processor evaluation). This is the *internal management* register —
risk tier, owner, and review cadence per vendor. The externally-
published list of entities that process personal data lives in
[`SUBPROCESSORS.md`](./SUBPROCESSORS.md); this register is the
control evidence that each is evaluated and re-reviewed on a
schedule.

## Risk tiers

| Tier | Criteria | Review cadence |
|------|----------|----------------|
| Critical | Processes personal data or mail content, or its outage is a SEV1 | Quarterly |
| Important | Control-plane / billing; outage is SEV2 | Semi-annual |
| Standard | No personal data, easily replaced | Annual |

## Register

| Vendor | Service | Tier | Data accessed | Owner | Evidence on file | Last review | Next review |
|--------|---------|------|---------------|-------|------------------|-------------|-------------|
| Stalwart Labs | Mail server | Critical | Mail content | Eng | Self-hosted; under KMail controls | 2025-Q2 | 2025-Q3 |
| zk-object-fabric / Wasabi | Encrypted blob storage | Critical | Encrypted bodies/attachments | Eng | SOC 2 (provider) | 2025-Q2 | 2025-Q3 |
| PostgreSQL (managed) | Control-plane state | Critical | Tenant/user/audit data | SRE | Provider SOC 2 + ISO 27001 | 2025-Q2 | 2025-Q3 |
| Meilisearch | Search index | Important | Derived index data | Eng | Self-hosted | 2025-Q2 | 2025-Q4 |
| Valkey | Cache / rate limit | Important | Ephemeral, TTL-bound | SRE | Self-hosted | 2025-Q2 | 2025-Q4 |
| KChat | OIDC IdP (parent) | Critical | Identity tokens only | Security | Token exchange only | 2025-Q2 | 2025-Q3 |
| Stripe | Billing | Important | Billing contact, no PAN | Finance | PCI-DSS L1 AOC | 2025-Q2 | 2025-Q4 |
| Cloud provider | IaaS | Critical | Hosts all of the above | SRE | SOC 2 Type II + ISO 27001 | 2025-Q2 | 2025-Q3 |

> Review dates are illustrative placeholders to be maintained by the
> control owner. `scripts/compliance/generate-evidence.sh
> --vendor-review` emits a copy of this register into the evidence
> bundle with the collection timestamp.

## Onboarding a new vendor

1. Security risk-tiers the vendor and identifies data accessed.
2. Collect the vendor's compliance attestation (SOC 2 / ISO 27001 /
   PCI as applicable) and DPA.
3. If the vendor processes personal data, follow the
   [`SUBPROCESSORS.md`](./SUBPROCESSORS.md) **30-day customer
   notification** requirement *before* authorising it.
4. Add a row here with owner + next-review date.

## Offboarding

1. Revoke credentials / API keys issued to the vendor.
2. Confirm data deletion or return per the contract.
3. Remove from `SUBPROCESSORS.md` and mark this row decommissioned
   (keep the historical row for the audit window).

## Review procedure (each cadence)

For each vendor due, the owner confirms: attestation still current,
no breaches disclosed since last review, access still least-
privilege, and contract/DPA unchanged. The confirmation date updates
the **Last review** column — that delta over the observation window
is the CC9.1 evidence.
