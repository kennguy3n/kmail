---
title: Troubleshooting mail delivery
description: Diagnose and fix common sending and receiving problems — bounces, spam folder, DNS, and authentication.
category: Troubleshooting
order: 10
updated: 2026-06-05
---

Most delivery problems come down to DNS, authentication, or reputation.
Work through these in order.

## Mail isn't being received

1. **Check MX** — `dig MX yourdomain.com` should return KMail's MX. If
   old records remain, mail may go to your previous provider.
2. **Check the DNS wizard** — **Admin → DNS Wizard** flags any record
   that isn't visible yet.
3. **Propagation** — recent DNS changes can take up to 48 hours.

## Mail I send lands in spam

1. **SPF, DKIM, DMARC** must all pass and align. See
   [DNS setup](/help/getting-started/dns-setup) and
   [DMARC explained](/help/security/dmarc-explained).
2. **Reputation** — new domains/IPs need warm-up. On the Privacy plan,
   dedicated IP pools warm up automatically; check **Admin → IP
   reputation**.
3. **Content** — avoid spammy formatting, mismatched display names, and
   link shorteners.

## Outbound mail bounces

- **Hard bounce (5xx):** the address doesn't exist or the receiving
  domain rejected you. Check the bounce reason under the message and the
  suppression list (`GET /api/v1/tenants/{id}/suppression`).
- **Soft bounce (4xx):** temporary (mailbox full, greylisting); KMail
  retries automatically.

## Authentication / sign-in problems

- If a desktop client fails to authenticate and you use TOTP or a
  passkey, generate an **app password** in the web app under Security
  settings.
- Confirm the client uses **SSL/TLS** on ports 993 (IMAP) and 465
  (SMTP), not plain or STARTTLS on those ports.

## Still stuck?

Gather the affected sender/recipient, timestamps, and any bounce text,
then contact support from the [Help Center](/help). The platform
[status page](/status) shows whether there's an active incident.
