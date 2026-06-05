import rss from "@astrojs/rss";
import type { APIContext } from "astro";
import { INCIDENTS } from "../../data/status";
import { SITE } from "../../data/site";

/**
 * Incident feed (RSS 2.0). One item per incident, using the most
 * recent update as the description so feed readers surface the
 * latest state. Linked from the status page and the site <head>.
 */
export async function GET(context: APIContext) {
  const site = context.site ?? new URL("https://kmail.kchat.dev");

  return rss({
    title: `${SITE.name} status`,
    description: "Incident notifications for the KMail platform.",
    site,
    items: INCIDENTS.map((inc) => {
      const latest = inc.updates[inc.updates.length - 1];
      const lines = inc.updates
        .map((u) => `${u.status.toUpperCase()} — ${u.body}`)
        .join("\n\n");
      return {
        title: `${inc.resolved ? "[Resolved] " : ""}${inc.title}`,
        // Stable per-incident link (anchor on the status page).
        link: new URL(`/status/#${inc.id}`, site).href,
        pubDate: new Date(latest?.ts ?? inc.date),
        description: latest ? latest.body : inc.title,
        content: lines,
        categories: [inc.severity, ...inc.affected],
      };
    }),
    customData: `<language>en-us</language>`,
  });
}
