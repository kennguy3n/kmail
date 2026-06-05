# Incident Response Playbook

Maps to SOC 2 **CC7.2** (incident response), **CC7.3** (detection of
security events), and **CC7.4** (recovery). This playbook is the
operational runbook on-call follows when a security or availability
incident is declared; each executed incident produces a dated
postmortem that is the SOC 2 evidence artifact for CC7.2.

## 1. Severity levels

| Sev | Definition | Examples | Page? |
|-----|------------|----------|-------|
| SEV1 | Confirmed breach, data exposure, or full outage | Tenant data cross-access, master-key compromise, API down | Immediate, 24/7 |
| SEV2 | Partial outage or credible security threat | One subsystem down, suspected intrusion, auth degraded | Within 30 min |
| SEV3 | Degraded, no data risk | Elevated error rate, single tenant impact | Business hours |

## 2. Roles

- **Incident Commander (IC)** — owns the incident, makes the call,
  delegates. The on-call lead by default.
- **Communications Lead** — customer + internal status updates.
- **Scribe** — maintains the timeline (feeds the postmortem).
- **Subject-matter responders** — pulled in by the IC.

## 3. Detection sources

- Prometheus alerts / SLO burn (`internal/monitoring/`).
- Audit-chain verification failure (`internal/audit/audit.go`
  `VerifyChain`) — a break implies tampering.
- Deliverability + anomaly signals (`internal/deliverability/`).
- Reverse-access-proxy approval logs (`internal/adminproxy/`).
- Customer report via support.

## 4. Response flow

1. **Declare** — anyone can declare. Open an incident channel; assign
   IC. Set severity.
2. **Contain** — stop the bleeding before root-causing:
   - Suspected credential compromise → rotate the affected secret.
     For the kmail-secrets master key follow the zero-downtime
     rotation runbook in [`../SECRETS.md`](../SECRETS.md) (dual-key
     window — no read downtime).
   - Suspected session/token abuse → revoke sessions
     (`POST /api/v1/sessions/revoke`) and/or force re-auth.
   - Suspected tenant cross-access → verify RLS GUC scoping; isolate
     the affected tenant if needed.
   - Active abuse traffic → confirm the rate limiter is fail-closed
     (`KMAIL_RATELIMIT_FAIL_CLOSED=true`) so a degraded Valkey can't
     lift the ceiling.
3. **Eradicate** — remove the root cause (patch, revoke, block).
4. **Recover** — restore service. DB point-in-time recovery and
   zk-object-fabric object versioning back CC7.4. Verify the audit
   chain after any restore.
5. **Communicate** — Comms Lead posts status per the cadence below.
6. **Close** — IC declares resolved when metrics are nominal and the
   root cause is contained.

## 5. Communication cadence

| Sev | Internal | Customer-facing |
|-----|----------|-----------------|
| SEV1 | Every 30 min | Status page within 1 h; updates hourly |
| SEV2 | Every 60 min | Status page if customer-visible |
| SEV3 | At resolution | Only if customer asks |

## 6. Postmortem (the CC7.2 evidence artifact)

Within **5 business days** of any SEV1/SEV2, the IC publishes a
blameless postmortem containing:

- Timeline (from the Scribe's log).
- Impact (tenants, data, duration).
- Root cause.
- What detected it / what should have.
- Action items with owners and due dates.

Postmortems are stored in the incident tracker and indexed for the
auditor. The action-item tracker closure is itself sampled evidence
that CC7.2 operates over the observation window.

## 7. Breach-notification clock

If personal data is exposed, the DPA notification obligations start
at confirmation. The Comms Lead owns regulator/customer notification
timing per [`../compliance/DPA.md`](./DPA.md). Do not wait for the
postmortem to start the notification clock.

## 8. Drills

A tabletop or live incident drill is run **quarterly** and recorded
the same way as a real incident, so CC7.2 has evidence even in a
quarter with no real SEV1/SEV2.
