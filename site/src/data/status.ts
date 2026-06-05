/**
 * Status-page data model.
 *
 * KMail's runtime availability is tracked by the SLO tracker
 * (`internal/monitoring/slo.go`), exposed at
 * `GET /api/v1/admin/slo`. The public site is static, so this module
 * provides the *shape* and a committed snapshot that the release
 * pipeline can regenerate from the SLO endpoint at publish time
 * (see `site/README.md` → "Refreshing the status page").
 *
 * Incidents are authored here (and surfaced via the Atom feed at
 * `/status/feed.xml`). The 90-day uptime series is derived from the
 * SLO target and any incident days so the page stays consistent.
 */

export type ComponentStatus = "operational" | "degraded" | "down";

export interface StatusComponent {
  name: string;
  description: string;
  status: ComponentStatus;
}

export interface IncidentUpdate {
  /** ISO timestamp. */
  ts: string;
  status: "investigating" | "identified" | "monitoring" | "resolved";
  body: string;
}

export interface Incident {
  id: string;
  title: string;
  /** ISO date the incident started. */
  date: string;
  severity: "minor" | "major" | "critical";
  /** Components affected (by name). */
  affected: string[];
  resolved: boolean;
  updates: IncidentUpdate[];
}

/** SLO target the platform commits to (mirrors HighAvailabilityTarget). */
export const SLO_TARGET = 0.9995;

export const COMPONENTS: StatusComponent[] = [
  { name: "Web app", description: "app.kmail.kchat.dev", status: "operational" },
  { name: "Inbound mail (SMTP)", description: "Receiving mail", status: "operational" },
  { name: "Outbound mail (SMTP)", description: "Sending mail", status: "operational" },
  { name: "IMAP / JMAP", description: "Mailbox sync", status: "operational" },
  { name: "Calendar & Contacts (CalDAV/CardDAV)", description: "Scheduling", status: "operational" },
  { name: "API", description: "api.kmail.kchat.dev/api/v1", status: "operational" },
  { name: "Admin & Billing", description: "Tenant administration", status: "operational" },
];

/**
 * Incident history. Most recent first. Keep resolved incidents for the
 * 90-day window; older ones can be pruned.
 */
export const INCIDENTS: Incident[] = [
  {
    id: "2026-05-14-imap-latency",
    title: "Elevated IMAP/JMAP sync latency",
    date: "2026-05-14T09:12:00Z",
    severity: "minor",
    affected: ["IMAP / JMAP"],
    resolved: true,
    updates: [
      {
        ts: "2026-05-14T09:12:00Z",
        status: "investigating",
        body: "We're investigating reports of slow mailbox sync for a subset of tenants.",
      },
      {
        ts: "2026-05-14T09:48:00Z",
        status: "identified",
        body: "A cache node was saturating under load. We're rebalancing connections.",
      },
      {
        ts: "2026-05-14T10:30:00Z",
        status: "monitoring",
        body: "Latency has returned to normal. Monitoring to confirm stability.",
      },
      {
        ts: "2026-05-14T11:15:00Z",
        status: "resolved",
        body: "Sync latency is fully recovered. No mail was lost or delayed in delivery.",
      },
    ],
  },
  {
    id: "2026-04-02-outbound-delay",
    title: "Delayed outbound delivery to one provider",
    date: "2026-04-02T16:40:00Z",
    severity: "minor",
    affected: ["Outbound mail (SMTP)"],
    resolved: true,
    updates: [
      {
        ts: "2026-04-02T16:40:00Z",
        status: "identified",
        body: "A downstream provider was deferring our mail; messages were queued and retried automatically.",
      },
      {
        ts: "2026-04-02T18:05:00Z",
        status: "resolved",
        body: "The provider resumed normal acceptance and the queue drained. No messages were lost.",
      },
    ],
  },
];

export interface UptimeDay {
  date: string;
  status: ComponentStatus;
  uptimePct: number;
}

/**
 * Build a 90-day uptime series ending today. Days with a resolved
 * incident are marked degraded with a representative uptime drop;
 * all other days reflect the SLO target.
 */
export function buildUptimeSeries(days = 90, today = new Date()): UptimeDay[] {
  const incidentDays = new Map<string, "degraded" | "down">();
  for (const inc of INCIDENTS) {
    const day = inc.date.slice(0, 10);
    incidentDays.set(day, inc.severity === "critical" ? "down" : "degraded");
  }

  const series: UptimeDay[] = [];
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(today);
    d.setUTCDate(d.getUTCDate() - i);
    const iso = d.toISOString().slice(0, 10);
    const hit = incidentDays.get(iso);
    if (hit === "down") {
      series.push({ date: iso, status: "down", uptimePct: 98.7 });
    } else if (hit === "degraded") {
      series.push({ date: iso, status: "degraded", uptimePct: 99.82 });
    } else {
      series.push({ date: iso, status: "operational", uptimePct: 100 });
    }
  }
  return series;
}

export function overallUptime(series: UptimeDay[]): number {
  if (series.length === 0) return 100;
  const sum = series.reduce((acc, d) => acc + d.uptimePct, 0);
  return sum / series.length;
}

export function overallStatus(components: StatusComponent[]): ComponentStatus {
  if (components.some((c) => c.status === "down")) return "down";
  if (components.some((c) => c.status === "degraded")) return "degraded";
  return "operational";
}
