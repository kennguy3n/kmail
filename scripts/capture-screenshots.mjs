// Capture demo screenshots for KMail React routes.
//
// The Vite dev server must be running with mocked APIs so every
// page renders sample data instead of "Failed to fetch" banners:
//
//   cd web && VITE_MOCK_API=true npx vite --port 5173
//
// The `scripts/capture-screenshots-with-mock.sh` wrapper handles
// starting / waiting on Vite for you. To run this script directly:
//
//   node scripts/capture-screenshots.mjs
//
// The generated screenshots land in `docs/screenshots/`.

import { mkdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const BASE = process.env.KMAIL_DEV_URL || "http://localhost:5173";
// Anchor the output directory to the repo root regardless of where
// node is invoked from.
const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(SCRIPT_DIR, "..");
const OUT = path.resolve(REPO_ROOT, "docs", "screenshots");

// `playwright` is a dev dependency of the React app (web/) so we
// only need it installed in one place. ESM resolution is anchored
// to the importing file, so a bare `import "playwright"` here
// would fail — we resolve the package by absolute URL instead.
const playwrightUrl = pathToFileURL(
  path.join(REPO_ROOT, "web", "node_modules", "playwright", "index.mjs"),
).href;
const { chromium } = await import(playwrightUrl);

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

// Substrings that, when present in the visible body text, indicate
// the page is showing an error banner instead of the polished mock
// state. The screenshot will still be captured, but a warning is
// logged so the maintainer can investigate.
const ERROR_NEEDLES = [
  "Failed to fetch",
  "internal error",
  "Internal error",
  "TypeError",
  "AdminApiError",
  "kmail-web:",
];

async function main() {
  await mkdir(OUT, { recursive: true });
  const browser = await chromium.launch();
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
  });
  const page = await context.newPage();
  const warnings = [];

  for (const r of routes) {
    const url = `${BASE}${r.path}`;
    process.stdout.write(`→ ${r.name.padEnd(26)} ${r.path} … `);
    try {
      await page.goto(url, { waitUntil: "networkidle", timeout: 15000 });
    } catch {
      await page.goto(url, { waitUntil: "domcontentloaded", timeout: 15000 });
    }
    // Give MSW responses time to be processed and React time to
    // render the resulting state. 1500ms is empirically enough for
    // the heaviest admin pages.
    await page.waitForTimeout(1500);
    const out = path.join(OUT, `${r.name}.png`);
    await page.screenshot({ path: out, fullPage: true });

    // Best-effort error-banner detection. We grep the rendered
    // body text rather than DOM selectors because every page
    // surfaces errors differently (banner div, inline `<p>`, alert
    // role, etc.).
    let bodyText = "";
    try {
      bodyText = await page.evaluate(() => document.body.innerText);
    } catch {
      // ignore — page might be navigating away
    }
    const hit = ERROR_NEEDLES.find((needle) => bodyText.includes(needle));
    if (hit) {
      warnings.push({ name: r.name, path: r.path, hit });
      process.stdout.write(`ok (warning: "${hit}")\n`);
    } else {
      process.stdout.write("ok\n");
    }
  }

  await browser.close();

  if (warnings.length > 0) {
    console.warn(
      `\n${warnings.length} route(s) rendered an error-looking string:`,
    );
    for (const w of warnings) {
      console.warn(`  - ${w.name} (${w.path}): "${w.hit}"`);
    }
    console.warn(
      "Inspect those PNGs and add or fix MSW handlers in web/src/mocks/handlers.ts.",
    );
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
