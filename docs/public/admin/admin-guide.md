---
title: Admin guide
description: A map of the KMail admin console — domains, users, provisioning, shared inboxes, security policy, billing, and the audit log.
category: Admin
order: 5
updated: 2026-06-06
---

This guide is the starting point for KMail administrators. It links to
the task-specific guides for each area of the admin console.

## Getting your domain live

1. Add your domain and publish the DNS records from the
   [DNS setup guide](/help/getting-started/dns-setup) (MX, SPF, DKIM,
   DMARC, and autodiscovery).
2. Verify DMARC is aligned — see [DMARC explained](/help/security/dmarc-explained).
3. Migrate existing mail from
   [Gmail](/help/migration/gmail-migration) or
   [Microsoft 365](/help/migration/m365-migration).

## Users & provisioning

- Create users manually in **Admin → Users**, or
- Automate the user lifecycle from your identity provider with
  [SCIM 2.0 provisioning](/help/admin/scim-provisioning) (Okta, Entra
  ID, OneLogin, JumpCloud, …).

## Shared mailboxes & collaboration

Set up team addresses like `support@` without paying for extra seats —
see [Shared inboxes](/help/admin/shared-inboxes).

## Security policy

- Require TOTP or passkeys, and review/revoke sessions.
- Review the [security & privacy overview](/help/security/privacy-overview)
  for the protections that apply tenant-wide.
- On the Privacy plan, configure customer-managed keys.
- Export the tamper-evident audit log for your records.

## Integrations & automation

Receive events in your own systems with
[webhooks](/help/admin/webhook-events), or call the
[KMail API](/help/getting-started/api-guide) directly.

## Billing

Manage your plan and seats — see
[Plans & billing](/help/billing/plans-and-billing).
