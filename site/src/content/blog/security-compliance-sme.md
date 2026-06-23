---
title: "Security and compliance features that fit SME budgets"
description: "Customer-managed keys, zero-access vaults, audit logs, and retention policies — how KMail gives small teams enterprise-grade controls."
pubDate: 2026-06-22
author: The KMail team
tags: [security, compliance, sme]
---

Enterprise mail suites often charge enterprise prices for security features that small teams actually need. KMail's approach is to make strong defaults free and advanced controls self-service.

## End-to-end encryption and zero-access vaults

Standard mailboxes are encrypted at rest. For material that should never be searchable or previewable by the server, KMail offers zero-access vault folders. The server stores the ciphertext but cannot read it; only the client holds the keys.

![KMail vault](/screenshots/03-vault.png)

This is useful for legal, finance, HR, and health-related correspondence where even the platform operator should not have access.

## Confidential send

When you need to share something outside your organization without leaving a permanent copy, confidential send creates a password-protected portal link with an expiry and view limit. The recipient reads the message through a secure page rather than receiving the body directly.

![KMail secure portal](/screenshots/10-secure-portal.png)

## Customer-managed keys

For tenants that want to control their own encryption keys, KMail supports customer-managed keys (CMK) via an HSM integration. You keep the key material; KMail keeps the infrastructure. If you rotate or revoke a key, the data becomes unreadable to us.

![KMail CMK admin](/screenshots/26-cmk-admin.png)

## Audit and retention

The audit log records administrative actions — user creation, domain changes, security settings, and data exports — so you can answer "who did what and when" during a review.

![KMail audit admin](/screenshots/24-audit-admin.png)

Retention policies let you define how long mail and calendar data is kept. You can set different policies per tenant or per mailbox, and exports let you pull data for compliance reviews.

![KMail retention admin](/screenshots/22-retention-admin.png)

## Email authentication

KMail automates the DNS records for MX, SPF, DKIM, DMARC, MTA-STS, and TLS-RPT. The domain admin shows the status of each check and the DNS wizard generates the exact records to paste into your registrar.

![KMail DKIM admin](/screenshots/16-dkim-admin.png)

## Why this matters for compliance-minded teams

You do not need a dedicated security team to operate KMail. The controls are exposed through the same web interface the rest of the product uses, and the defaults already meet the posture most SMEs need for GDPR, SOC 2, and basic vendor security reviews.

[See the security overview](/security) or [start a workspace](/signup).
