#!/usr/bin/env node
/**
 * Build-time content sync for the KMail public site.
 *
 * Two jobs, both idempotent and safe to run repeatedly:
 *
 *   1. Mirror the canonical help-center markdown from
 *      `docs/public/` (repo source of truth) into
 *      `site/src/content/help/` so Astro's content collection can
 *      render it under `/help`. The mirror directory is gitignored.
 *
 *   2. Generate `site/src/data/changelog.generated.json` from
 *      `docs/DEVELOPMENT_LOG.md` (with a status line pulled from
 *      `docs/PROGRESS.md`) so the `/changelog` page is derived from
 *      the engineering log rather than hand-maintained.
 *
 * Run automatically via the `prebuild` / `predev` npm hooks.
 */

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SITE_DIR = path.resolve(__dirname, "..");
const REPO_ROOT = path.resolve(SITE_DIR, "..");

const DOCS_PUBLIC = path.join(REPO_ROOT, "docs", "public");
const HELP_DEST = path.join(SITE_DIR, "src", "content", "help");
const DEV_LOG = path.join(REPO_ROOT, "docs", "DEVELOPMENT_LOG.md");
const PROGRESS = path.join(REPO_ROOT, "docs", "PROGRESS.md");
const CHANGELOG_OUT = path.join(SITE_DIR, "src", "data", "changelog.generated.json");
const OPENAPI_SRC = path.join(REPO_ROOT, "api", "openapi", "kmail.openapi.json");
const OPENAPI_DEST = path.join(SITE_DIR, "public", "openapi", "kmail.openapi.json");

async function rmrf(dir) {
  await fs.rm(dir, { recursive: true, force: true });
}

async function walk(dir) {
  const out = [];
  let entries;
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return out;
  }
  for (const e of entries) {
    const full = path.join(dir, e.name);
    if (e.isDirectory()) out.push(...(await walk(full)));
    // Skip README / index files: they document the directory itself and
    // have no article frontmatter, so they aren't help articles.
    else if (e.isFile() && e.name.endsWith(".md") && e.name.toLowerCase() !== "readme.md")
      out.push(full);
  }
  return out;
}

async function mirrorHelp() {
  await rmrf(HELP_DEST);
  await fs.mkdir(HELP_DEST, { recursive: true });
  const files = await walk(DOCS_PUBLIC);
  let n = 0;
  for (const src of files) {
    const rel = path.relative(DOCS_PUBLIC, src);
    const dest = path.join(HELP_DEST, rel);
    await fs.mkdir(path.dirname(dest), { recursive: true });
    await fs.copyFile(src, dest);
    n++;
  }
  console.log(`[sync-content] mirrored ${n} help article(s) → src/content/help/`);
}

/** Collapse soft-wrapped markdown into single-spaced text. */
function unwrap(s) {
  return s.replace(/\s*\n\s*/g, " ").replace(/\s{2,}/g, " ").trim();
}

/** Strip inline markdown emphasis/backticks/links for clean summaries. */
function stripMd(s) {
  return s
    .replace(/`([^`]*)`/g, "$1")
    .replace(/\*\*([^*]*)\*\*/g, "$1")
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .trim();
}

function firstSentence(s, max = 280) {
  const m = s.match(/^(.*?[.!?])(\s|$)/);
  let out = m ? m[1] : s;
  if (out.length > max) out = out.slice(0, max - 1).trimEnd() + "…";
  return out;
}

async function generateChangelog() {
  // Ensure the output dir exists before either the fallback or success
  // write below (src/data/ may not exist on a clean build).
  await fs.mkdir(path.dirname(CHANGELOG_OUT), { recursive: true });

  let raw;
  try {
    raw = await fs.readFile(DEV_LOG, "utf8");
  } catch {
    console.warn("[sync-content] DEVELOPMENT_LOG.md not found; skipping changelog");
    await fs.writeFile(CHANGELOG_OUT, JSON.stringify({ status: null, entries: [] }, null, 2));
    return;
  }

  // Top-level entries start at column 0 with "- **Last updated**:".
  // Continuation lines are indented. Split on the bullet marker.
  const lines = raw.split("\n");
  const blocks = [];
  let current = null;
  for (const line of lines) {
    if (/^- \*\*Last updated\*\*:/.test(line)) {
      if (current) blocks.push(current);
      current = line;
    } else if (current !== null) {
      if (/^- /.test(line) || /^#/.test(line) || /^> /.test(line)) {
        // A new non-entry block ends the current entry.
        blocks.push(current);
        current = null;
      } else {
        current += "\n" + line;
      }
    }
  }
  if (current) blocks.push(current);

  const entries = [];
  for (const block of blocks) {
    const text = unwrap(block.replace(/^- \*\*Last updated\*\*:\s*/, ""));
    // Expected shape: "2026-04-27 (Phase 8, batch 1) — <summary>"
    const m = text.match(/^(\d{4}-\d{2}-\d{2})\s*(\(([^)]*)\))?\s*[—-]\s*([\s\S]*)$/);
    if (!m) continue;
    const [, date, , label, body] = m;
    const clean = stripMd(body);
    entries.push({
      date,
      label: label ? label.trim() : null,
      title: label ? label.trim() : date,
      summary: firstSentence(clean),
    });
  }

  // De-dupe by (date,label) keeping first (reverse-chronological order
  // in the source is preserved).
  const seen = new Set();
  const deduped = entries.filter((e) => {
    const k = `${e.date}|${e.label}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });

  let status = null;
  try {
    const progress = await fs.readFile(PROGRESS, "utf8");
    const sm = progress.match(/\*\*Status\*\*:\s*([^\n]+(?:\n\s{2,}[^\n]+)*)/);
    if (sm) status = firstSentence(unwrap(stripMd(sm[1])), 320);
  } catch {
    /* PROGRESS.md optional */
  }

  await fs.writeFile(
    CHANGELOG_OUT,
    JSON.stringify({ status, generatedAt: new Date().toISOString(), entries: deduped }, null, 2),
  );
  console.log(`[sync-content] generated ${deduped.length} changelog entr(ies) → src/data/changelog.generated.json`);
}

async function publishOpenApi() {
  try {
    await fs.access(OPENAPI_SRC);
  } catch {
    console.warn(
      "[sync-content] api/openapi/kmail.openapi.json not found; run `node api/openapi/generate.mjs`",
    );
    return;
  }
  await fs.mkdir(path.dirname(OPENAPI_DEST), { recursive: true });
  await fs.copyFile(OPENAPI_SRC, OPENAPI_DEST);
  console.log("[sync-content] published OpenAPI spec → public/openapi/kmail.openapi.json");
}

await mirrorHelp();
await generateChangelog();
await publishOpenApi();
