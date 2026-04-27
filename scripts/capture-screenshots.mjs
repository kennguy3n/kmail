// Capture demo screenshots for KMail React routes.
// Run with: node scripts/capture-screenshots.mjs
// Requires the Vite dev server to be running on http://localhost:5173.

import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";
import path from "node:path";

const BASE = process.env.KMAIL_DEV_URL || "http://localhost:5173";
const OUT = path.resolve("docs/screenshots");

const routes = [
  { path: "/mail", name: "01-mail-inbox" },
  { path: "/mail/compose", name: "02-compose" },
  { path: "/mail/vault", name: "03-vault" },
  { path: "/mail/shared", name: "04-shared-inbox" },
  { path: "/mail/protected-folders", name: "05-protected-folders" },
  { path: "/calendar", name: "06-calendar" },
  { path: "/calendar/new", name: "07-event-create" },
  { path: "/calendar/shared", name: "08-shared-calendars" },
  { path: "/contacts", name: "09-contacts" },
  { path: "/secure/demo-token-abc123", name: "10-secure-portal" },
  { path: "/admin/domains", name: "11-domain-admin" },
  { path: "/admin/dns-wizard", name: "12-dns-wizard" },
  { path: "/admin/users", name: "13-user-admin" },
  { path: "/admin/pricing", name: "14-pricing-admin" },
  { path: "/admin/pricing-plans", name: "15-pricing-page" },
  { path: "/admin/dkim", name: "16-dkim-admin" },
  { path: "/admin/sieve", name: "17-sieve-admin" },
  { path: "/admin/security", name: "18-security-settings" },
  { path: "/admin/search", name: "19-search-admin" },
  { path: "/admin/slo", name: "20-slo-admin" },
  { path: "/admin/onboarding", name: "21-onboarding" },
  { path: "/admin/retention", name: "22-retention-admin" },
  { path: "/admin/webhooks", name: "23-webhook-admin" },
  { path: "/admin/audit", name: "24-audit-admin" },
  { path: "/admin/billing", name: "25-billing-admin" },
  { path: "/admin/cmk", name: "26-cmk-admin" },
  { path: "/admin/scim", name: "27-scim-admin" },
  { path: "/admin/exports", name: "28-export-admin" },
];

async function main() {
  await mkdir(OUT, { recursive: true });
  const browser = await chromium.launch();
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const page = await context.newPage();

  for (const r of routes) {
    const url = `${BASE}${r.path}`;
    process.stdout.write(`→ ${r.name.padEnd(26)} ${r.path} … `);
    try {
      await page.goto(url, { waitUntil: "networkidle", timeout: 15000 });
    } catch {
      await page.goto(url, { waitUntil: "domcontentloaded", timeout: 15000 });
    }
    // Give React a moment to render placeholder data.
    await page.waitForTimeout(900);
    const out = path.join(OUT, `${r.name}.png`);
    await page.screenshot({ path: out, fullPage: true });
    process.stdout.write("ok\n");
  }

  await browser.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
