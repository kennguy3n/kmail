#!/usr/bin/env node
/**
 * Generate an OpenAPI 3.1 specification for KMail's HTTP API by
 * extracting the route table from the Go sources.
 *
 * The handlers register routes as `"<METHOD> <path>"` strings passed
 * to `http.ServeMux` (Go 1.22+ method-aware patterns). This script
 * scans `cmd/` and `internal/` for those literals, dedupes them, and
 * emits a spec with:
 *
 *   - one tagged operation per route (tag = first path segment),
 *   - declared path parameters for every `{param}` token,
 *   - bearer (OIDC) security by default, with public routes opted out,
 *   - rich `info.description` covering auth, webhooks, and SCIM,
 *   - `x-codeSamples` (curl/Python/Node/Go) on representative routes.
 *
 * Output: `api/openapi/kmail.openapi.json` (committed; consumed by the
 * site's Redoc page and copied into `site/public/openapi/` at build).
 *
 * Run: `node api/openapi/generate.mjs` (also wired into `make` /
 * the site prebuild via sync-content.mjs copying the result).
 */

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, "..", "..");
const SCAN_DIRS = [path.join(REPO_ROOT, "cmd"), path.join(REPO_ROOT, "internal")];
const OUT = path.join(__dirname, "kmail.openapi.json");

const METHOD_RE = /"(GET|POST|PUT|DELETE|PATCH)\s+(\/[^"]*)"/g;

async function walkGo(dir) {
  const out = [];
  let entries;
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return out;
  }
  for (const e of entries) {
    const full = path.join(dir, e.name);
    if (e.isDirectory()) out.push(...(await walkGo(full)));
    else if (e.isFile() && e.name.endsWith(".go") && !e.name.endsWith("_test.go"))
      out.push(full);
  }
  return out;
}

async function collectRoutes() {
  const routes = new Map(); // key "METHOD path" -> {method, path}
  for (const root of SCAN_DIRS) {
    for (const file of await walkGo(root)) {
      const src = await fs.readFile(file, "utf8");
      let m;
      while ((m = METHOD_RE.exec(src)) !== null) {
        const method = m[1];
        let p = m[2];
        // Skip obvious non-routes / fragments.
        if (p.includes(" ") || p.includes("%")) continue;
        // Normalize trailing wildcard mux patterns like "/{id...}".
        p = p.replace(/\{([a-zA-Z0-9_]+)\.\.\.\}/g, "{$1}");
        // Only document API-surface paths.
        if (
          !p.startsWith("/api/v1") &&
          !p.startsWith("/scim/v2") &&
          !p.startsWith("/.well-known") &&
          p !== "/jmap"
        )
          continue;
        const key = `${method} ${p}`;
        routes.set(key, { method, path: p });
      }
    }
  }
  return [...routes.values()].sort((a, b) =>
    a.path === b.path ? a.method.localeCompare(b.method) : a.path.localeCompare(b.path),
  );
}

function tagFor(p) {
  if (p.startsWith("/scim/v2")) return "SCIM";
  if (p.startsWith("/.well-known")) return "Discovery";
  if (p === "/jmap" || p.startsWith("/jmap")) return "JMAP";
  const seg = p.replace(/^\/api\/v1\//, "").split("/")[0] ?? "misc";
  const map = {
    tenants: "Tenants & Admin",
    admin: "Platform Admin",
    signup: "Signup",
    auth: "Authentication",
    calendars: "Calendars",
    "resource-calendars": "Calendars",
    contacts: "Contacts",
    migrations: "Migration",
    push: "Push",
    snoozed: "Mail",
    "scheduled-sends": "Mail",
    "shared-inboxes": "Shared Inboxes",
    attachments: "Mail",
    send: "Mail",
    secure: "Confidential Send",
    search: "Search",
    storage: "Storage",
    "chat-bridge": "Chat Bridge",
    integ: "Webhooks & Integrations",
    oauth: "OAuth",
  };
  return map[seg] ?? "API";
}

function isPublic(method, p) {
  // Public, unauthenticated surfaces.
  if (p.startsWith("/.well-known")) return true;
  if (p === "/api/v1/signup" && method === "POST") return true;
  if (p.startsWith("/api/v1/signup/")) return true; // status polling
  if (p.startsWith("/api/v1/secure/")) return true; // recipient portal token
  // NOTE: do NOT treat "/api/v1/send/" as public. The only routes under
  // that prefix are the undo-send endpoints (POST /api/v1/send/{id}/cancel
  // and GET /api/v1/send/{id}), both wrapped with authMW.Wrap in
  // internal/undosend/handlers.go — they require an OIDC bearer token. A
  // broad prefix match here previously emitted `security: []` for them,
  // telling clients no auth was needed (causing spurious 401s).
  return false;
}

function paramsFor(p) {
  const params = [];
  for (const m of p.matchAll(/\{([a-zA-Z0-9_]+)\}/g)) {
    params.push({
      name: m[1],
      in: "path",
      required: true,
      description: `\`${m[1]}\` path parameter.`,
      schema: { type: "string" },
    });
  }
  return params;
}

function titleCase(s) {
  return s.replace(/[-_]/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

function summaryFor(method, p) {
  const segs = p.split("/").filter(Boolean);
  const last = segs[segs.length - 1] ?? "";
  const resource = last.startsWith("{")
    ? titleCase(segs[segs.length - 2] ?? "resource")
    : titleCase(last);
  const verb = {
    GET: last.startsWith("{") ? "Get" : "List",
    POST: "Create",
    PUT: "Replace",
    PATCH: "Update",
    DELETE: "Delete",
  }[method];
  return `${verb} ${resource}`.trim();
}

// Representative x-codeSamples keyed by "METHOD path".
function codeSamples(method, p) {
  const base = "https://api.kmail.kchat.dev";
  if (p === "/api/v1/tenants" && method === "GET") {
    return [
      {
        lang: "curl",
        label: "curl",
        source: `curl -s ${base}/api/v1/tenants \\\n  -H "Authorization: Bearer $KMAIL_TOKEN"`,
      },
      {
        lang: "Python",
        label: "Python",
        source: `import os, requests\n\nr = requests.get(\n    "${base}/api/v1/tenants",\n    headers={"Authorization": f"Bearer {os.environ['KMAIL_TOKEN']}"},\n)\nr.raise_for_status()\nprint(r.json())`,
      },
      {
        lang: "JavaScript",
        label: "Node.js",
        source: `const res = await fetch("${base}/api/v1/tenants", {\n  headers: { Authorization: \`Bearer \${process.env.KMAIL_TOKEN}\` },\n});\nif (!res.ok) throw new Error(\`HTTP \${res.status}\`);\nconsole.log(await res.json());`,
      },
      {
        lang: "Go",
        label: "Go",
        source: `req, _ := http.NewRequest("GET", "${base}/api/v1/tenants", nil)\nreq.Header.Set("Authorization", "Bearer "+os.Getenv("KMAIL_TOKEN"))\nresp, err := http.DefaultClient.Do(req)\nif err != nil { log.Fatal(err) }\ndefer resp.Body.Close()\nio.Copy(os.Stdout, resp.Body)`,
      },
    ];
  }
  if (p === "/api/v1/signup" && method === "POST") {
    return [
      {
        lang: "curl",
        label: "curl",
        source: `curl -s -X POST ${base}/api/v1/signup \\\n  -H "Content-Type: application/json" \\\n  -d '{"email":"founder@acme.com","org_name":"Acme","plan":"pro"}'`,
      },
      {
        lang: "JavaScript",
        label: "Node.js",
        source: `const res = await fetch("${base}/api/v1/signup", {\n  method: "POST",\n  headers: { "Content-Type": "application/json" },\n  body: JSON.stringify({ email: "founder@acme.com", org_name: "Acme", plan: "pro" }),\n});\nconst { checkout_url } = await res.json();\nwindow.location = checkout_url; // redirect to Stripe Checkout`,
      },
    ];
  }
  return null;
}

const INFO_DESCRIPTION = `
# KMail API

The KMail API is a tenant-scoped REST API for administering mail,
calendars, contacts, billing, deliverability, and security. Mailbox
data access (reading/sending mail, syncing folders) uses **JMAP** at
\`/jmap\`; this reference covers the \`/api/v1\` administrative surface,
the **SCIM 2.0** provisioning API at \`/scim/v2\`, and autodiscovery
endpoints under \`/.well-known\`.

All responses include an \`X-KMail-Correlation-Id\` header for tracing.

## Authentication

Most endpoints require an **OIDC bearer token**:

\`\`\`
Authorization: Bearer <access_token>
\`\`\`

Obtain a token from your configured OIDC provider (the token's claims
identify the tenant and user). Requests without a valid token receive
\`401 Unauthorized\`; valid tokens lacking the required role receive
\`403 Forbidden\`. Every endpoint is tenant-scoped and isolated with
PostgreSQL row-level security — you can only ever see your own tenant's
data.

A small number of endpoints are intentionally **public** (no bearer
token): \`POST /api/v1/signup\` and its status polling route, the
Confidential Send recipient portal (\`/api/v1/secure/{token}\`), open
tracking links (\`/api/v1/send/{id}\`), and the \`/.well-known/*\`
autodiscovery documents.

### SCIM tokens

The SCIM provisioning API (\`/scim/v2\`) authenticates with a dedicated
bearer token you mint per tenant (\`POST /api/v1/tenants/{id}/scim/tokens\`)
and configure in your identity provider. See the
[SCIM provisioning guide](/help/admin/scim-provisioning).

## Webhooks

Register HTTPS endpoints (\`POST /api/v1/integ/webhooks\`) to receive
events: \`email.received\`, \`email.bounced\`, \`email.complaint\`,
\`calendar.event_created\`, \`calendar.event_updated\`,
\`migration.completed\`, plus a \`webhook.ping\` test delivery. Each
delivery is signed with HMAC-SHA256 in the \`X-KMail-Signature\` header.
See the full [webhook event catalog](/help/admin/webhook-events) for the
signature scheme and verification examples.

## Errors

Errors use standard HTTP status codes with a JSON body:

\`\`\`json
{ "error": "human readable message", "code": "machine_code" }
\`\`\`

## Versioning

The base path is \`/api/v1\`. Breaking changes bump the major version;
additive changes (new fields, new endpoints) do not.
`.trim();

async function main() {
  const routes = await collectRoutes();

  const paths = {};
  for (const { method, path: p } of routes) {
    const op = {
      tags: [tagFor(p)],
      summary: summaryFor(method, p),
      operationId: `${method.toLowerCase()}_${p
        .replace(/[/{}]/g, " ")
        .trim()
        .replace(/\s+/g, "_")
        .replace(/\.\.\./g, "")}`,
      parameters: paramsFor(p),
      responses: {
        "200": { description: "Successful response" },
        "400": { $ref: "#/components/responses/BadRequest" },
        "401": { $ref: "#/components/responses/Unauthorized" },
        "403": { $ref: "#/components/responses/Forbidden" },
        "404": { $ref: "#/components/responses/NotFound" },
      },
    };
    if (["POST", "PUT", "PATCH"].includes(method)) {
      op.requestBody = {
        required: true,
        content: {
          "application/json": { schema: { type: "object", additionalProperties: true } },
        },
      };
      op.responses["201"] = { description: "Created" };
    }
    if (method === "DELETE") {
      op.responses["204"] = { description: "Deleted" };
    }
    if (isPublic(method, p)) {
      op.security = [];
    }
    const samples = codeSamples(method, p);
    if (samples) op["x-codeSamples"] = samples;

    if (!op.parameters.length) delete op.parameters;

    if (!paths[p]) paths[p] = {};
    paths[p][method.toLowerCase()] = op;
  }

  // Stable tag ordering for the rendered docs.
  const TAG_ORDER = [
    "Signup",
    "Authentication",
    "Tenants & Admin",
    "Mail",
    "Shared Inboxes",
    "Confidential Send",
    "Calendars",
    "Contacts",
    "Migration",
    "Webhooks & Integrations",
    "OAuth",
    "SCIM",
    "Push",
    "Search",
    "Storage",
    "Chat Bridge",
    "Platform Admin",
    "Discovery",
    "JMAP",
    "API",
  ];
  const usedTags = new Set(routes.map((r) => tagFor(r.path)));
  const tags = TAG_ORDER.filter((t) => usedTags.has(t)).map((name) => ({ name }));

  const spec = {
    openapi: "3.1.0",
    info: {
      title: "KMail API",
      version: "1.0.0",
      summary: "Tenant-scoped administrative, provisioning, and discovery API for KMail.",
      description: INFO_DESCRIPTION,
      contact: { name: "KMail API support", email: "support@kmail.kchat.dev" },
      license: { name: "Proprietary" },
    },
    servers: [
      { url: "https://api.kmail.kchat.dev", description: "Production" },
      { url: "http://localhost:8088", description: "Local development (BFF)" },
    ],
    tags,
    security: [{ bearerAuth: [] }],
    paths,
    components: {
      securitySchemes: {
        bearerAuth: {
          type: "http",
          scheme: "bearer",
          bearerFormat: "JWT",
          description: "OIDC access token (JWT). Identifies the tenant and user.",
        },
        scimToken: {
          type: "http",
          scheme: "bearer",
          description: "Per-tenant SCIM provisioning token.",
        },
      },
      responses: {
        BadRequest: { description: "Invalid request", content: errorContent() },
        Unauthorized: { description: "Missing or invalid credentials", content: errorContent() },
        Forbidden: { description: "Authenticated but not permitted", content: errorContent() },
        NotFound: { description: "Resource not found", content: errorContent() },
      },
      schemas: {
        Error: {
          type: "object",
          properties: {
            error: { type: "string", description: "Human-readable message." },
            code: { type: "string", description: "Stable machine-readable code." },
          },
          required: ["error"],
        },
      },
    },
  };

  await fs.writeFile(OUT, JSON.stringify(spec, null, 2) + "\n");
  console.log(
    `[openapi] wrote ${Object.keys(paths).length} paths / ${routes.length} operations → ${path.relative(REPO_ROOT, OUT)}`,
  );
}

function errorContent() {
  return { "application/json": { schema: { $ref: "#/components/schemas/Error" } } };
}

await main();
