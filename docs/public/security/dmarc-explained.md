---
title: DMARC explained
description: What DMARC is, how it works with SPF and DKIM, and how to roll out a reject policy safely.
category: Security
order: 10
updated: 2026-06-05
---

DMARC (Domain-based Message Authentication, Reporting & Conformance)
protects your domain from spoofing. It builds on SPF and DKIM and tells
receiving servers what to do with mail that fails authentication — and
asks them to send you reports.

## The three records

- **SPF** lists which servers may send for your domain.
- **DKIM** cryptographically signs your mail so tampering is detectable.
- **DMARC** sets a policy (`none` / `quarantine` / `reject`) and a
  reporting address, and requires **alignment** between the `From:`
  domain and the SPF/DKIM domain.

## A DMARC record

```
Type   Host      Value
TXT    _dmarc    v=DMARC1; p=reject; rua=mailto:dmarc@yourdomain.com; adkim=s; aspf=s
```

| Tag    | Meaning                                                       |
| ------ | ------------------------------------------------------------- |
| `p`    | Policy for failing mail: `none`, `quarantine`, or `reject`.   |
| `rua`  | Address for aggregate (daily) reports.                        |
| `adkim`/`aspf` | Alignment mode: `s` (strict) or `r` (relaxed).        |

## Roll out safely

Going straight to `p=reject` can drop legitimate mail (e.g. a newsletter
tool sending as your domain). Roll out in stages:

1. **Monitor** — publish `p=none` and collect reports for 1–2 weeks.
2. **Quarantine** — move to `p=quarantine` (or `p=quarantine; pct=25`)
   and watch for failures from legitimate senders.
3. **Reject** — once everything legitimate aligns, set `p=reject`.

## Reading reports in KMail

KMail ingests DMARC aggregate reports for your domains. See
**Admin → DMARC** for a summary of pass/fail by source IP and sending
service, so you can spot a forgotten sender before tightening policy.
Endpoints: `GET /api/v1/tenants/{id}/dmarc-reports` and
`/dmarc-reports/summary`.

## Common pitfalls

- A third-party sender (CRM, invoicing) isn't included in SPF or doesn't
  sign with an aligned DKIM key — its mail fails alignment.
- Forwarded mail can break SPF; DKIM usually survives, which is why
  DKIM alignment matters.
- Multiple `_dmarc` TXT records — there must be exactly one.
