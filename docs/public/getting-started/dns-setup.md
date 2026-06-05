---
title: DNS setup guide
description: Add the MX, SPF, DKIM, DMARC, and autodiscover records KMail needs to send and receive mail on your domain.
category: Getting Started
order: 10
updated: 2026-06-05
---

To send and receive mail on your own domain, you publish a handful of
DNS records at your domain registrar or DNS host. KMail's **DNS
onboarding wizard** (Admin → DNS Wizard) generates the exact records for
your domain and verifies them live, so you usually just copy-paste.

This guide explains what each record does so you know what you're
publishing.

## 1. MX — where mail is delivered

```
Type   Host   Value                       Priority
MX     @      mx.kmail.kchat.dev          10
```

MX records tell other mail servers where to deliver mail for your
domain. Remove any old MX records from a previous provider so mail
isn't split.

## 2. SPF — who may send for your domain

```
Type   Host   Value
TXT    @      v=spf1 include:spf.kmail.kchat.dev -all
```

SPF lists the servers allowed to send mail using your domain. The
`-all` suffix tells receivers to reject anything from an unlisted
server. If you also send from another service (e.g. a CRM), add its
`include:` before `-all`.

## 3. DKIM — cryptographic signing

The wizard generates a unique public key per domain:

```
Type   Host                       Value
TXT    kmail._domainkey           v=DKIM1; k=rsa; p=MIGfMA0GCSq...
```

DKIM lets receivers verify that mail wasn't altered in transit and
really came from your domain. KMail signs every outbound message with
the matching private key (managed for you).

## 4. DMARC — policy and reporting

```
Type   Host      Value
TXT    _dmarc    v=DMARC1; p=quarantine; rua=mailto:dmarc@yourdomain.com
```

DMARC ties SPF and DKIM together and tells receivers what to do with
mail that fails. Start with `p=none` to monitor, then move to
`p=quarantine` and finally `p=reject`. See
[DMARC explained](/help/security/dmarc-explained).

## 5. Autodiscover / autoconfig (optional but recommended)

```
Type    Host                  Value
CNAME   autoconfig            autoconfig.kmail.kchat.dev
CNAME   autodiscover          autodiscover.kmail.kchat.dev
SRV     _autodiscover._tcp    0 0 443 autodiscover.kmail.kchat.dev
```

These let Thunderbird, Apple Mail, and Outlook configure themselves
from just an email address and password.

## Verifying

DNS changes can take from a few minutes up to 48 hours to propagate.
The wizard re-checks each record and shows a green tick when it's
visible. If a record stays red:

- Confirm you edited the **correct** zone (the apex domain, not a
  subdomain).
- Make sure your host didn't append your domain to an already-fully-
  qualified value.
- Wait for your TTL to expire, then re-check.

Still stuck? See [delivery troubleshooting](/help/troubleshooting/delivery-issues).
