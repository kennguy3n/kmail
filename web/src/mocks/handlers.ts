/**
 * MSW request handlers used by `VITE_MOCK_API=true` builds.
 *
 * The screenshot capture script (`scripts/capture-screenshots.mjs`)
 * walks every public route in the React app to produce demo
 * screenshots for `docs/screenshots/`. The Go BFF is not running
 * at capture time, so without these handlers each page would
 * render an "internal error" / "Failed to fetch" banner instead
 * of realistic content.
 *
 * Every handler returns *static* sample data — no persistence,
 * no shared state across requests — so the screenshots are
 * deterministic. The data is shaped to look like a polished
 * "Acme Corp" demo tenant with healthy metrics, all-verified DNS
 * records, a small set of users, and a handful of recent emails
 * and calendar events.
 *
 * Add a new handler whenever a UI page starts hitting a new
 * endpoint that the screenshot capture script visits — otherwise
 * the page will fall back to its error state and the screenshot
 * will look broken.
 */
import { http, HttpResponse } from "msw";

const TENANT_ID = "00000000-0000-0000-0000-000000000001";
const DOMAIN_ID = "00000000-0000-0000-0000-000000000010";
const ACCOUNT_ID = "acct-acme-demo";
const CALENDAR_ACCOUNT_ID = "cal-acme-demo";
const ADMIN_USER_ID = "user-admin-demo";
const NOW = new Date("2026-04-28T09:00:00.000Z");

// ─── Helpers ─────────────────────────────────────────────────────────

type Json = Record<string, unknown>;
type JmapInvocation = [method: string, args: Json, callId: string];

interface JmapBatchRequest {
  using?: string[];
  methodCalls: JmapInvocation[];
  createdIds?: Record<string, string>;
}

function dayOffset(days: number, hour = 9, minute = 0): string {
  const d = new Date(NOW);
  d.setUTCDate(d.getUTCDate() + days);
  d.setUTCHours(hour, minute, 0, 0);
  return d.toISOString();
}

function relPast(seconds: number): string {
  return new Date(NOW.getTime() - seconds * 1000).toISOString();
}

// ─── JMAP fixtures ───────────────────────────────────────────────────

const session = {
  capabilities: {
    "urn:ietf:params:jmap:core": {
      maxSizeUpload: 50_000_000,
      maxConcurrentUpload: 4,
      maxSizeRequest: 10_000_000,
      maxConcurrentRequests: 4,
      maxCallsInRequest: 16,
      maxObjectsInGet: 500,
      maxObjectsInSet: 500,
      collationAlgorithms: ["i;ascii-numeric", "i;ascii-casemap"],
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {},
    "urn:ietf:params:jmap:calendars": {},
  },
  accounts: {
    [ACCOUNT_ID]: {
      name: "demo@kmail.dev",
      isPersonal: true,
      isReadOnly: false,
      accountCapabilities: {
        "urn:ietf:params:jmap:mail": {},
        "urn:ietf:params:jmap:submission": {},
      },
    },
    [CALENDAR_ACCOUNT_ID]: {
      name: "demo@kmail.dev",
      isPersonal: true,
      isReadOnly: false,
      accountCapabilities: {
        "urn:ietf:params:jmap:calendars": {},
      },
    },
  },
  primaryAccounts: {
    "urn:ietf:params:jmap:mail": ACCOUNT_ID,
    "urn:ietf:params:jmap:submission": ACCOUNT_ID,
    "urn:ietf:params:jmap:calendars": CALENDAR_ACCOUNT_ID,
  },
  username: "demo@kmail.dev",
  apiUrl: "/jmap",
  downloadUrl: "/jmap/download/{accountId}/{blobId}/{name}",
  uploadUrl: "/jmap/upload/{accountId}",
  eventSourceUrl: "/jmap/eventsource",
  state: "demo-state-1",
};

const mailboxes = [
  {
    id: "mbx-inbox",
    name: "Inbox",
    parentId: null,
    role: "inbox",
    sortOrder: 0,
    totalEmails: 12,
    unreadEmails: 3,
    totalThreads: 12,
    unreadThreads: 3,
    isSubscribed: true,
    myRights: rights(),
  },
  {
    id: "mbx-drafts",
    name: "Drafts",
    parentId: null,
    role: "drafts",
    sortOrder: 1,
    totalEmails: 1,
    unreadEmails: 0,
    totalThreads: 1,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
  {
    id: "mbx-sent",
    name: "Sent",
    parentId: null,
    role: "sent",
    sortOrder: 2,
    totalEmails: 24,
    unreadEmails: 0,
    totalThreads: 24,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
  {
    id: "mbx-archive",
    name: "Archive",
    parentId: null,
    role: "archive",
    sortOrder: 3,
    totalEmails: 318,
    unreadEmails: 0,
    totalThreads: 318,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
  {
    id: "mbx-junk",
    name: "Junk",
    parentId: null,
    role: "junk",
    sortOrder: 4,
    totalEmails: 0,
    unreadEmails: 0,
    totalThreads: 0,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
  {
    id: "mbx-trash",
    name: "Trash",
    parentId: null,
    role: "trash",
    sortOrder: 5,
    totalEmails: 7,
    unreadEmails: 0,
    totalThreads: 7,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
];

function rights() {
  return {
    mayReadItems: true,
    mayAddItems: true,
    mayRemoveItems: true,
    maySetSeen: true,
    maySetKeywords: true,
    mayCreateChild: true,
    mayRename: true,
    mayDelete: true,
    maySubmit: true,
  };
}

const emails = [
  {
    id: "msg-1",
    subject: "Welcome to KMail!",
    from: [{ name: "KMail Team", email: "welcome@kmail.dev" }],
    preview:
      "Welcome aboard. Here's a quick tour of your new private mailbox.",
    receivedAt: relPast(60 * 60),
    keywords: {} as Record<string, boolean>,
    privacyMode: "standard",
  },
  {
    id: "msg-2",
    subject: "Q2 Budget Review",
    from: [{ name: "Alice Nguyen", email: "alice@acme.com" }],
    preview:
      "Numbers from Finance are in. Quick sync tomorrow at 10am to lock the Q2 ask?",
    receivedAt: relPast(3 * 60 * 60),
    keywords: { $seen: false } as Record<string, boolean>,
    privacyMode: "standard",
  },
  {
    id: "msg-3",
    subject: "Team standup notes – Apr 27",
    from: [{ name: "Bob Martinez", email: "bob@acme.com" }],
    preview:
      "Notes attached. Decisions: ship the migration tool Friday, hold the privacy launch.",
    receivedAt: relPast(8 * 60 * 60),
    keywords: { $seen: true },
    privacyMode: "standard",
  },
  {
    id: "msg-4",
    subject: "Invoice #2847 attached",
    from: [{ name: "Billing — KChat", email: "billing@kchat.dev" }],
    preview:
      "Your April invoice for KChat Mail Pro is ready. Auto-charge runs in 7 days.",
    receivedAt: relPast(20 * 60 * 60),
    keywords: { $seen: true },
    hasAttachment: true,
    privacyMode: "standard",
  },
  {
    id: "msg-5",
    subject: "Confidential: M&A draft",
    from: [{ name: "Cara Patel", email: "cara@acme.com" }],
    preview:
      "Encrypted in KChat. Open the secure portal to view this message.",
    receivedAt: relPast(26 * 60 * 60),
    keywords: { $seen: true },
    privacyMode: "confidential-send",
  },
  {
    id: "msg-6",
    subject: "Customer feedback — Q1 wrap",
    from: [{ name: "Diego Ramos", email: "diego@acme.com" }],
    preview:
      "Top three themes from the customer survey, plus the verbatim quotes Marketing wanted.",
    receivedAt: relPast(2 * 24 * 60 * 60),
    keywords: { $seen: true },
    privacyMode: "standard",
  },
  {
    id: "msg-7",
    subject: "Re: Hiring update — backend platform",
    from: [{ name: "Erin Walsh", email: "erin@acme.com" }],
    preview:
      "Made an offer to the senior platform candidate. Verbal accept this morning.",
    receivedAt: relPast(3 * 24 * 60 * 60),
    keywords: { $seen: true },
    privacyMode: "standard",
  },
  {
    id: "msg-8",
    subject: "DMARC weekly report — acme.com",
    from: [{ name: "KMail Deliverability", email: "deliverability@kmail.dev" }],
    preview:
      "Pass rate 98.4% over the last 7 days. Two failing sources flagged for review.",
    receivedAt: relPast(4 * 24 * 60 * 60),
    keywords: { $seen: true },
    hasAttachment: true,
    privacyMode: "standard",
  },
];

const fullEmails = emails.map((e) => ({
  ...e,
  blobId: `blob-${e.id}`,
  threadId: `thr-${e.id}`,
  mailboxIds: { "mbx-inbox": true } as Record<string, boolean>,
  size: 4096,
  to: [{ name: "Demo Admin", email: "demo@kmail.dev" }],
  cc: null,
  bcc: null,
  replyTo: null,
  sentAt: e.receivedAt,
  hasAttachment: e.hasAttachment ?? false,
}));

const calendars = [
  {
    id: "cal-personal",
    name: "Personal",
    color: "#6366f1",
    isVisible: true,
    isDefault: true,
  },
  {
    id: "cal-work",
    name: "Work",
    color: "#059669",
    isVisible: true,
    isDefault: false,
  },
];

const events = [
  {
    id: "evt-1",
    calendarId: "cal-work",
    title: "Team standup",
    description: "Daily 15-minute sync with the platform team.",
    start: dayOffset(0, 9, 0),
    end: dayOffset(0, 9, 15),
    location: "KChat #platform",
    status: "confirmed",
    participants: [
      { email: "alice@acme.com", name: "Alice Nguyen", role: "required", rsvp: "accepted" },
      { email: "bob@acme.com", name: "Bob Martinez", role: "required", rsvp: "accepted" },
    ],
  },
  {
    id: "evt-2",
    calendarId: "cal-work",
    title: "1:1 — Cara",
    description: "Bi-weekly career conversation.",
    start: dayOffset(1, 14, 0),
    end: dayOffset(1, 14, 30),
    location: "Zoom",
    status: "confirmed",
    participants: [
      { email: "cara@acme.com", name: "Cara Patel", role: "required", rsvp: "accepted" },
    ],
  },
  {
    id: "evt-3",
    calendarId: "cal-work",
    title: "Customer call — Globex",
    description: "Quarterly check-in. Diego will lead the agenda.",
    start: dayOffset(2, 16, 0),
    end: dayOffset(2, 17, 0),
    location: "Google Meet",
    status: "confirmed",
    participants: [
      { email: "diego@acme.com", name: "Diego Ramos", role: "chair", rsvp: "accepted" },
      { email: "wiley@globex.com", name: "Wiley Coyote", role: "required", rsvp: "tentative" },
    ],
  },
  {
    id: "evt-4",
    calendarId: "cal-work",
    title: "Company all-hands",
    description: "Quarterly company-wide update.",
    start: dayOffset(3, 17, 0),
    end: dayOffset(3, 18, 0),
    location: "Auditorium / Zoom",
    status: "confirmed",
    participants: [],
  },
  {
    id: "evt-5",
    calendarId: "cal-personal",
    title: "Yoga",
    description: "Studio class.",
    start: dayOffset(4, 7, 30),
    end: dayOffset(4, 8, 30),
    location: "Studio Loft",
    status: "confirmed",
    participants: [],
  },
];

const identities = [
  {
    id: "identity-demo",
    name: "Demo Admin",
    email: "demo@kmail.dev",
    replyTo: null,
    bcc: null,
    textSignature: "— sent from KMail",
    htmlSignature: "<p>— sent from KMail</p>",
    mayDelete: false,
  },
];

// ─── JMAP dispatcher ─────────────────────────────────────────────────

interface MethodResult {
  result: Json;
}

function jmapDispatch(method: string, args: Json): MethodResult {
  switch (method) {
    case "Mailbox/get":
      return {
        result: {
          accountId: args.accountId,
          state: "demo-state-1",
          list: mailboxes,
          notFound: [],
        },
      };
    case "Email/query":
      return {
        result: {
          accountId: args.accountId,
          queryState: "demo-state-1",
          canCalculateChanges: false,
          position: 0,
          ids: emails.map((e) => e.id),
          total: emails.length,
        },
      };
    case "Email/get":
      return {
        result: {
          accountId: args.accountId,
          state: "demo-state-1",
          list: fullEmails,
          notFound: [],
        },
      };
    case "Identity/get":
      return {
        result: {
          accountId: args.accountId,
          state: "demo-state-1",
          list: identities,
          notFound: [],
        },
      };
    case "Calendar/get":
      return {
        result: {
          accountId: args.accountId,
          state: "demo-state-1",
          list: calendars,
          notFound: [],
        },
      };
    case "CalendarEvent/query":
      return {
        result: {
          accountId: args.accountId,
          queryState: "demo-state-1",
          position: 0,
          ids: events.map((e) => e.id),
          total: events.length,
        },
      };
    case "CalendarEvent/get":
      return {
        result: {
          accountId: args.accountId,
          state: "demo-state-1",
          list: events,
          notFound: [],
        },
      };
    case "Email/set":
    case "CalendarEvent/set":
      return {
        result: {
          accountId: args.accountId,
          oldState: "demo-state-1",
          newState: "demo-state-2",
          created: null,
          updated: null,
          destroyed: null,
        },
      };
    default:
      return {
        result: {
          accountId: args.accountId,
          state: "demo-state-1",
          list: [],
          notFound: [],
        },
      };
  }
}

// ─── Handlers ────────────────────────────────────────────────────────

export const handlers = [
  // ─── JMAP ──────────────────────────────────────────────────────────
  http.get("/jmap/session", () => HttpResponse.json(session)),

  http.post("/jmap", async ({ request }) => {
    const body = (await request.json()) as JmapBatchRequest;
    const methodResponses = (body.methodCalls ?? []).map((call) => {
      const [method, args, callId] = call;
      const { result } = jmapDispatch(method, args ?? {});
      return [method, result, callId];
    });
    return HttpResponse.json({
      methodResponses,
      sessionState: "demo-state-1",
    });
  }),

  // ─── Tenant ────────────────────────────────────────────────────────
  http.get("/api/v1/tenants", () =>
    HttpResponse.json([
      {
        id: TENANT_ID,
        name: "Acme Corp",
        slug: "acme",
        plan: "pro",
        status: "active",
        created_at: relPast(180 * 24 * 60 * 60),
        updated_at: relPast(60 * 60),
      },
    ]),
  ),

  http.get("/api/v1/tenants/:tenantId/domains", () =>
    HttpResponse.json([
      {
        id: DOMAIN_ID,
        tenant_id: TENANT_ID,
        domain: "acme.com",
        verified: true,
        mx_verified: true,
        spf_verified: true,
        dkim_verified: true,
        dmarc_verified: true,
        created_at: relPast(180 * 24 * 60 * 60),
        updated_at: relPast(24 * 60 * 60),
      },
    ]),
  ),

  http.post("/api/v1/tenants/:tenantId/domains/:domainId/verify", () =>
    HttpResponse.json({
      domain_id: DOMAIN_ID,
      domain: "acme.com",
      mx_verified: true,
      spf_verified: true,
      dkim_verified: true,
      dmarc_verified: true,
      verified: true,
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/domains/:domainId/dns-records", () =>
    HttpResponse.json({
      domain: "acme.com",
      records: [
        {
          type: "MX",
          name: "acme.com.",
          value: "mx1.kmail.dev.",
          ttl: 3600,
          priority: 10,
        },
        {
          type: "MX",
          name: "acme.com.",
          value: "mx2.kmail.dev.",
          ttl: 3600,
          priority: 20,
        },
        {
          type: "TXT",
          name: "acme.com.",
          value: "v=spf1 include:_spf.kmail.dev ~all",
          ttl: 3600,
        },
        {
          type: "TXT",
          name: "kmail2026._domainkey.acme.com.",
          value:
            "v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQC8...QAB",
          ttl: 3600,
        },
        {
          type: "TXT",
          name: "_dmarc.acme.com.",
          value:
            "v=DMARC1; p=quarantine; rua=mailto:dmarc@acme.com; pct=100; aspf=s",
          ttl: 3600,
        },
        {
          type: "TXT",
          name: "_mta-sts.acme.com.",
          value: "v=STSv1; id=20260301T000000",
          ttl: 3600,
        },
        {
          type: "CNAME",
          name: "mta-sts.acme.com.",
          value: "mta-sts.kmail.dev.",
          ttl: 3600,
        },
        {
          type: "TXT",
          name: "_smtp._tls.acme.com.",
          value: "v=TLSRPTv1; rua=mailto:tlsrpt@acme.com",
          ttl: 3600,
        },
        {
          type: "CNAME",
          name: "autoconfig.acme.com.",
          value: "autoconfig.kmail.dev.",
          ttl: 3600,
        },
        {
          type: "TXT",
          name: "default._bimi.acme.com.",
          value: "v=BIMI1; l=https://acme.com/bimi.svg",
          ttl: 3600,
        },
      ],
    }),
  ),

  http.get(
    "/api/v1/tenants/:tenantId/domains/:domainId/dkim",
    () =>
      HttpResponse.json({
        keys: [
          {
            id: "dkim-active",
            selector: "kmail2026",
            public_key:
              "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQC8Wfx0demo+key+material/abcd",
            status: "active",
            created_at: relPast(120 * 24 * 60 * 60),
            activated_at: relPast(120 * 24 * 60 * 60),
          },
          {
            id: "dkim-deprecated",
            selector: "kmail2025",
            public_key:
              "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDPrev0us+key+material/wxyz",
            status: "deprecated",
            created_at: relPast(420 * 24 * 60 * 60),
            activated_at: relPast(420 * 24 * 60 * 60),
            expires_at: dayOffset(30, 0, 0),
          },
        ],
      }),
  ),

  http.get("/api/v1/tenants/:tenantId/users", () =>
    HttpResponse.json([
      {
        id: ADMIN_USER_ID,
        tenant_id: TENANT_ID,
        kchat_user_id: "kchat-admin",
        stalwart_account_id: "stalwart-admin",
        email: "demo@kmail.dev",
        display_name: "Demo Admin",
        role: "owner",
        status: "active",
        account_type: "user",
        quota_bytes: 75 * 1024 ** 3,
        created_at: relPast(180 * 24 * 60 * 60),
        updated_at: relPast(24 * 60 * 60),
      },
      {
        id: "user-alice",
        tenant_id: TENANT_ID,
        kchat_user_id: "kchat-alice",
        stalwart_account_id: "stalwart-alice",
        email: "alice@acme.com",
        display_name: "Alice Nguyen",
        role: "member",
        status: "active",
        account_type: "user",
        quota_bytes: 25 * 1024 ** 3,
        created_at: relPast(150 * 24 * 60 * 60),
        updated_at: relPast(2 * 24 * 60 * 60),
      },
      {
        id: "user-bob",
        tenant_id: TENANT_ID,
        kchat_user_id: "kchat-bob",
        stalwart_account_id: "stalwart-bob",
        email: "bob@acme.com",
        display_name: "Bob Martinez",
        role: "member",
        status: "active",
        account_type: "user",
        quota_bytes: 25 * 1024 ** 3,
        created_at: relPast(140 * 24 * 60 * 60),
        updated_at: relPast(60 * 60),
      },
      {
        id: "user-cara",
        tenant_id: TENANT_ID,
        kchat_user_id: "kchat-cara",
        stalwart_account_id: "stalwart-cara",
        email: "cara@acme.com",
        display_name: "Cara Patel",
        role: "member",
        status: "active",
        account_type: "user",
        quota_bytes: 25 * 1024 ** 3,
        created_at: relPast(120 * 24 * 60 * 60),
        updated_at: relPast(3 * 60 * 60),
      },
      {
        id: "user-support",
        tenant_id: TENANT_ID,
        kchat_user_id: "kchat-support",
        stalwart_account_id: "stalwart-support",
        email: "support@acme.com",
        display_name: "Support Inbox",
        role: "member",
        status: "active",
        account_type: "shared_inbox",
        quota_bytes: 50 * 1024 ** 3,
        created_at: relPast(110 * 24 * 60 * 60),
        updated_at: relPast(12 * 60 * 60),
      },
    ]),
  ),

  // ─── Billing / Quota ───────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/billing", () =>
    HttpResponse.json({
      tenant_id: TENANT_ID,
      plan: "pro",
      seat_count: 5,
      seat_limit: 25,
      storage_used_bytes: Math.round(2.1 * 1024 ** 3),
      storage_limit_bytes: 75 * 1024 ** 3,
      per_seat_cents: 600,
      monthly_total_cents: 3000,
      currency: "USD",
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/billing/usage", () =>
    HttpResponse.json({
      tenant_id: TENANT_ID,
      storage_used_bytes: Math.round(2.1 * 1024 ** 3),
      storage_limit_bytes: 75 * 1024 ** 3,
      seat_count: 5,
      seat_limit: 25,
      updated_at: relPast(60 * 60),
    }),
  ),

  http.post("/api/v1/tenants/:tenantId/billing/portal", () =>
    HttpResponse.json({
      id: "bps_demo_session",
      url: "https://billing.stripe.com/p/session/demo",
    }),
  ),

  // ─── Audit ─────────────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/audit-log", () =>
    HttpResponse.json({ entries: auditEntries }),
  ),

  http.post("/api/v1/tenants/:tenantId/audit-log/verify", () =>
    HttpResponse.json({ ok: true }),
  ),

  // ─── DMARC ─────────────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/dmarc-reports/summary", () =>
    HttpResponse.json({
      tenant_id: TENANT_ID,
      domain: "acme.com",
      pass_count: 4920,
      fail_count: 80,
      total: 5000,
      pass_rate: 0.984,
      report_count: 14,
      window_days: 7,
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/dmarc-reports", () =>
    HttpResponse.json([
      {
        id: "dmarc-1",
        tenant_id: TENANT_ID,
        domain_id: DOMAIN_ID,
        report_id: "google.com!acme.com!1745798400!1745884800",
        org_name: "google.com",
        email: "noreply-dmarc-support@google.com",
        date_range_begin: relPast(7 * 24 * 60 * 60),
        date_range_end: relPast(6 * 24 * 60 * 60),
        domain: "acme.com",
        adkim: "s",
        aspf: "s",
        policy: "quarantine",
        pass_count: 1280,
        fail_count: 12,
        records: [],
        created_at: relPast(6 * 24 * 60 * 60),
      },
    ]),
  ),

  // ─── Admin SLO / IP reputation / deliverability ─────────────────────
  http.get("/api/v1/admin/slo", () =>
    HttpResponse.json({
      availability: {
        tenant_id: "global",
        window_seconds: 30 * 24 * 60 * 60,
        total: 1_000_000,
        successes: 999_500,
        failures: 500,
        availability: 0.9995,
        target: 0.999,
      },
      latency: {
        tenant_id: "global",
        window_seconds: 30 * 24 * 60 * 60,
        count: 1_000_000,
        p50_ms: 45,
        p95_ms: 120,
        p99_ms: 280,
      },
    }),
  ),

  http.get("/api/v1/admin/slo/breaches", () => HttpResponse.json([])),

  http.get("/api/v1/admin/slo/regions", () =>
    HttpResponse.json({
      window_seconds: 30 * 24 * 60 * 60,
      target: 0.999,
      regions: [
        {
          region: "us-east-1",
          total: 500_000,
          successes: 499_750,
          failures: 250,
          availability: 0.9995,
          target: 0.999,
        },
        {
          region: "eu-west-1",
          total: 500_000,
          successes: 499_750,
          failures: 250,
          availability: 0.9995,
          target: 0.999,
        },
      ],
      global_total: 1_000_000,
      global_success: 999_500,
      global_failures: 500,
      global_availability: 0.9995,
    }),
  ),

  http.get("/api/v1/admin/slo/:tenantId", () =>
    HttpResponse.json({
      availability: {
        tenant_id: TENANT_ID,
        window_seconds: 30 * 24 * 60 * 60,
        total: 25_000,
        successes: 24_988,
        failures: 12,
        availability: 0.9995,
        target: 0.999,
      },
      latency: {
        tenant_id: TENANT_ID,
        window_seconds: 30 * 24 * 60 * 60,
        count: 25_000,
        p50_ms: 42,
        p95_ms: 110,
        p99_ms: 240,
      },
    }),
  ),

  http.get("/api/v1/admin/ip-reputation", () =>
    HttpResponse.json({
      ips: [
        {
          ip_id: "ip-1",
          address: "203.0.113.10",
          pool_id: "pool-mature",
          pool_name: "Mature Trusted",
          pool_type: "mature",
          reputation_score: 92,
          daily_volume: 4200,
          bounce_rate: 0.012,
          complaint_rate: 0.0004,
          status: "healthy",
          warmup_day: 0,
          updated_at: relPast(60 * 60),
        },
        {
          ip_id: "ip-2",
          address: "203.0.113.11",
          pool_id: "pool-warm",
          pool_name: "Warming",
          pool_type: "warmup",
          reputation_score: 88,
          daily_volume: 2100,
          bounce_rate: 0.018,
          complaint_rate: 0.0006,
          status: "warming",
          warmup_day: 12,
          updated_at: relPast(2 * 60 * 60),
        },
      ],
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/deliverability/alerts", () =>
    HttpResponse.json({
      alerts: [
        {
          id: "alert-1",
          tenant_id: TENANT_ID,
          alert_type: "bounce_rate",
          severity: "info",
          metric_name: "bounce_rate",
          metric_value: 0.018,
          threshold_value: 0.05,
          message:
            "Bounce rate trending up slightly week over week. Still well under threshold.",
          acknowledged: false,
          created_at: relPast(6 * 60 * 60),
        },
      ],
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/deliverability/thresholds", () =>
    HttpResponse.json({
      thresholds: [
        {
          tenant_id: TENANT_ID,
          metric_name: "bounce_rate",
          warning_threshold: 0.05,
          critical_threshold: 0.1,
          updated_at: relPast(60 * 24 * 60 * 60),
        },
        {
          tenant_id: TENANT_ID,
          metric_name: "complaint_rate",
          warning_threshold: 0.001,
          critical_threshold: 0.003,
          updated_at: relPast(60 * 24 * 60 * 60),
        },
      ],
    }),
  ),

  // ─── Storage placement ─────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/storage/placement", () =>
    HttpResponse.json({
      tenant_id: TENANT_ID,
      policy_ref: "us-only-managed",
      countries: ["US"],
      preferred_provider: "wasabi",
      encryption_mode: "ManagedEncrypted",
      erasure_profile: "ec-4-2",
      updated_at: relPast(45 * 24 * 60 * 60),
    }),
  ),

  http.get("/api/v1/storage/regions", () =>
    HttpResponse.json([
      { code: "us-east-1", name: "US East (N. Virginia)" },
      { code: "us-west-2", name: "US West (Oregon)" },
      { code: "eu-west-1", name: "Europe (Ireland)" },
      { code: "eu-central-1", name: "Europe (Frankfurt)" },
    ]),
  ),

  // ─── Retention / Approvals / Exports ───────────────────────────────
  http.get("/api/v1/tenants/:tenantId/retention", () =>
    HttpResponse.json([
      {
        id: "ret-1",
        tenant_id: TENANT_ID,
        policy_type: "archive",
        retention_days: 365,
        applies_to: "all",
        enabled: true,
        created_at: relPast(60 * 24 * 60 * 60),
        updated_at: relPast(7 * 24 * 60 * 60),
      },
      {
        id: "ret-2",
        tenant_id: TENANT_ID,
        policy_type: "delete",
        retention_days: 1825,
        applies_to: "label",
        target_ref: "auto-archive",
        enabled: true,
        created_at: relPast(45 * 24 * 60 * 60),
        updated_at: relPast(7 * 24 * 60 * 60),
      },
    ]),
  ),

  http.get("/api/v1/tenants/:tenantId/retention/status", () =>
    HttpResponse.json({
      dry_run: false,
      last_evaluated_at: relPast(6 * 60 * 60),
      emails_deleted: 412,
      emails_archived: 18_730,
      errors: 0,
      recent_runs: [
        {
          id: "run-1",
          policy_id: "ret-1",
          rows_scanned: 25_000,
          rows_processed: 18_730,
          rows_archived: 18_730,
          rows_deleted: 0,
          started_at: relPast(7 * 60 * 60),
          completed_at: relPast(6 * 60 * 60),
        },
      ],
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/approvals", () =>
    HttpResponse.json([]),
  ),

  http.get("/api/v1/tenants/:tenantId/approvals/config", () =>
    HttpResponse.json({
      cmk_rotate: true,
      vault_recover: true,
      domain_delete: true,
      data_export: true,
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/exports", () =>
    HttpResponse.json([
      {
        id: "exp-1",
        tenant_id: TENANT_ID,
        requester_id: ADMIN_USER_ID,
        format: "mbox",
        scope: "mailbox",
        scope_ref: "mbx-archive",
        status: "completed",
        download_url:
          "https://exports.kmail.dev/demo/exp-1.mbox.gz",
        created_at: relPast(2 * 24 * 60 * 60),
        started_at: relPast(2 * 24 * 60 * 60 - 60),
        completed_at: relPast(47 * 60 * 60),
      },
      {
        id: "exp-2",
        tenant_id: TENANT_ID,
        requester_id: ADMIN_USER_ID,
        format: "eml",
        scope: "date_range",
        scope_ref: "2026-01-01..2026-03-31",
        status: "running",
        created_at: relPast(2 * 60 * 60),
        started_at: relPast(2 * 60 * 60 - 30),
      },
    ]),
  ),

  // ─── CMK / HSM ─────────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/cmk", () =>
    HttpResponse.json([
      {
        id: "cmk-active",
        tenant_id: TENANT_ID,
        key_fingerprint:
          "SHA256:Sx9G+demo+fingerprint+for+screenshots+only/abcdef",
        public_key_pem:
          "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A...DEMO==\n-----END PUBLIC KEY-----",
        status: "active",
        algorithm: "RSA-OAEP-256",
        created_at: relPast(120 * 24 * 60 * 60),
        updated_at: relPast(30 * 24 * 60 * 60),
      },
    ]),
  ),

  http.get("/api/v1/tenants/:tenantId/cmk/active", () =>
    HttpResponse.json({
      id: "cmk-active",
      tenant_id: TENANT_ID,
      key_fingerprint:
        "SHA256:Sx9G+demo+fingerprint+for+screenshots+only/abcdef",
      public_key_pem:
        "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A...DEMO==\n-----END PUBLIC KEY-----",
      status: "active",
      algorithm: "RSA-OAEP-256",
      created_at: relPast(120 * 24 * 60 * 60),
      updated_at: relPast(30 * 24 * 60 * 60),
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/cmk/hsm", () =>
    HttpResponse.json([
      {
        id: "hsm-1",
        tenant_id: TENANT_ID,
        provider_type: "kmip",
        endpoint: "kmip.acme.com:5696",
        status: "active",
        last_test_at: relPast(6 * 60 * 60),
        created_at: relPast(60 * 24 * 60 * 60),
        updated_at: relPast(6 * 60 * 60),
      },
    ]),
  ),

  // ─── Vault / Protected folders ─────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/vault/folders", () =>
    HttpResponse.json([
      {
        id: "vault-1",
        tenant_id: TENANT_ID,
        user_id: ADMIN_USER_ID,
        folder_name: "Personal — Health",
        encryption_mode: "StrictZK",
        wrapped_dek: "AAAA...DEMO",
        key_algorithm: "XChaCha20-Poly1305",
        nonce: "BBBB...DEMO",
        created_at: relPast(45 * 24 * 60 * 60),
        updated_at: relPast(7 * 24 * 60 * 60),
      },
      {
        id: "vault-2",
        tenant_id: TENANT_ID,
        user_id: ADMIN_USER_ID,
        folder_name: "Legal — Contracts",
        encryption_mode: "StrictZK",
        wrapped_dek: "CCCC...DEMO",
        key_algorithm: "XChaCha20-Poly1305",
        nonce: "DDDD...DEMO",
        created_at: relPast(30 * 24 * 60 * 60),
        updated_at: relPast(2 * 24 * 60 * 60),
      },
    ]),
  ),

  http.get("/api/v1/tenants/:tenantId/protected-folders", () =>
    HttpResponse.json([
      {
        id: "pf-1",
        tenant_id: TENANT_ID,
        owner_id: ADMIN_USER_ID,
        folder_name: "Board materials",
        created_at: relPast(45 * 24 * 60 * 60),
        updated_at: relPast(7 * 24 * 60 * 60),
      },
      {
        id: "pf-2",
        tenant_id: TENANT_ID,
        owner_id: ADMIN_USER_ID,
        folder_name: "M&A pipeline",
        created_at: relPast(30 * 24 * 60 * 60),
        updated_at: relPast(2 * 24 * 60 * 60),
      },
    ]),
  ),

  http.get("/api/v1/tenants/:tenantId/protected-folders/:folderId/access", () =>
    HttpResponse.json([]),
  ),

  http.get(
    "/api/v1/tenants/:tenantId/protected-folders/:folderId/access-log",
    () => HttpResponse.json([]),
  ),

  // ─── SCIM ──────────────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/scim/tokens", () =>
    HttpResponse.json([
      {
        id: "scim-1",
        description: "Okta production sync",
        created_at: relPast(60 * 24 * 60 * 60),
        revoked_at: null,
      },
      {
        id: "scim-2",
        description: "Workday HR sync (revoked)",
        created_at: relPast(120 * 24 * 60 * 60),
        revoked_at: relPast(7 * 24 * 60 * 60),
      },
    ]),
  ),

  // ─── Webhooks ──────────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/webhooks", () =>
    HttpResponse.json([
      {
        id: "wh-1",
        tenant_id: TENANT_ID,
        url: "https://hooks.acme.com/kmail/security",
        events: ["mail.confidential_send", "auth.webauthn_registered"],
        active: true,
        signing_version: "v2",
        created_at: relPast(60 * 24 * 60 * 60),
        updated_at: relPast(2 * 24 * 60 * 60),
      },
      {
        id: "wh-2",
        tenant_id: TENANT_ID,
        url: "https://hooks.acme.com/kmail/audit",
        events: ["audit.entry"],
        active: true,
        signing_version: "v2",
        created_at: relPast(45 * 24 * 60 * 60),
        updated_at: relPast(60 * 60),
      },
    ]),
  ),

  http.get("/api/v1/tenants/:tenantId/webhook-deliveries", () =>
    HttpResponse.json([
      {
        id: "whd-1",
        tenant_id: TENANT_ID,
        endpoint_id: "wh-1",
        event_type: "mail.confidential_send",
        status: "delivered",
        attempts: 1,
        last_status: 200,
        next_retry_at: relPast(0),
        created_at: relPast(60 * 60),
      },
      {
        id: "whd-2",
        tenant_id: TENANT_ID,
        endpoint_id: "wh-2",
        event_type: "audit.entry",
        status: "delivered",
        attempts: 1,
        last_status: 200,
        next_retry_at: relPast(0),
        created_at: relPast(2 * 60 * 60),
      },
    ]),
  ),

  // ─── Onboarding checklist ──────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/onboarding", () =>
    HttpResponse.json({
      tenant_id: TENANT_ID,
      updated_at: relPast(2 * 60 * 60),
      steps: [
        {
          id: "verify-domain",
          title: "Verify your domain",
          description:
            "Add the MX, SPF, DKIM and DMARC records to your DNS provider.",
          status: "complete",
          optional: false,
          completed_at: relPast(170 * 24 * 60 * 60),
          link: "/admin/dns-wizard",
        },
        {
          id: "invite-team",
          title: "Invite your team",
          description: "Add the first 5 users from KChat directory sync.",
          status: "complete",
          optional: false,
          completed_at: relPast(120 * 24 * 60 * 60),
          link: "/admin/users",
        },
        {
          id: "set-plan",
          title: "Choose a plan",
          description: "Upgrade from trial to KChat Mail Pro or Privacy.",
          status: "complete",
          optional: false,
          completed_at: relPast(90 * 24 * 60 * 60),
          link: "/admin/pricing",
        },
        {
          id: "configure-cmk",
          title: "Register a customer-managed key",
          description: "Bring your own key for tenant-scoped envelope encryption.",
          status: "complete",
          optional: true,
          completed_at: relPast(60 * 24 * 60 * 60),
          link: "/admin/cmk",
        },
        {
          id: "enroll-mfa",
          title: "Enroll a hardware security key",
          description: "Phishing-resistant WebAuthn for every admin.",
          status: "complete",
          optional: false,
          completed_at: relPast(45 * 24 * 60 * 60),
          link: "/admin/security",
        },
        {
          id: "review-retention",
          title: "Review retention policies",
          description:
            "Confirm archive/delete defaults match your compliance posture.",
          status: "complete",
          optional: false,
          completed_at: relPast(30 * 24 * 60 * 60),
          link: "/admin/retention",
        },
        {
          id: "configure-webhooks",
          title: "Wire SIEM webhooks",
          description: "Stream audit and security events to Splunk / Datadog.",
          status: "pending",
          optional: true,
          link: "/admin/webhooks",
        },
        {
          id: "first-migration",
          title: "Run your first migration",
          description: "Move existing mailboxes from Gmail or Microsoft 365.",
          status: "pending",
          optional: true,
          link: "/admin/migrations",
        },
      ],
    }),
  ),

  // ─── Search backend ────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/search/backend", () =>
    HttpResponse.json({ backend: "meilisearch" }),
  ),

  // ─── Sieve ─────────────────────────────────────────────────────────
  http.get("/api/v1/tenants/:tenantId/sieve-rules", () =>
    HttpResponse.json({
      rules: [
        {
          id: "sieve-1",
          tenant_id: TENANT_ID,
          name: "Auto-archive newsletters",
          script:
            'require ["fileinto"];\nif header :contains "List-Id" "" {\n  fileinto "Archive";\n}',
          priority: 10,
          enabled: true,
          created_at: relPast(60 * 24 * 60 * 60),
          updated_at: relPast(15 * 24 * 60 * 60),
        },
        {
          id: "sieve-2",
          tenant_id: TENANT_ID,
          name: "Tag invoices",
          script:
            'require ["imap4flags"];\nif header :contains "subject" "Invoice" {\n  addflag "$invoice";\n}',
          priority: 20,
          enabled: true,
          created_at: relPast(45 * 24 * 60 * 60),
          updated_at: relPast(7 * 24 * 60 * 60),
        },
      ],
    }),
  ),

  // ─── WebAuthn / TOTP ───────────────────────────────────────────────
  http.get("/api/v1/auth/webauthn/credentials", () =>
    HttpResponse.json({
      credentials: [
        {
          id: "wa-1",
          name: "YubiKey 5C NFC",
          aaguid: "demo-aaguid",
          last_used_at: relPast(3 * 60 * 60),
          created_at: relPast(45 * 24 * 60 * 60),
        },
        {
          id: "wa-2",
          name: "Touch ID — MacBook Pro",
          aaguid: "demo-aaguid-touchid",
          last_used_at: relPast(20 * 60 * 60),
          created_at: relPast(120 * 24 * 60 * 60),
        },
      ],
    }),
  ),

  http.get("/api/v1/auth/totp/status", () =>
    HttpResponse.json({ enrolled: true, enrolled_at: relPast(120 * 24 * 60 * 60) }),
  ),

  // ─── Migrations / Resource calendars ───────────────────────────────
  http.get("/api/v1/migrations", () =>
    HttpResponse.json({
      jobs: [
        {
          id: "mig-1",
          tenant_id: TENANT_ID,
          source_type: "gmail_imap",
          source_host: "imap.gmail.com",
          source_user: "founder@oldcompany.com",
          destination_user_id: ADMIN_USER_ID,
          status: "completed",
          messages_total: 12_540,
          messages_synced: 12_540,
          started_at: relPast(45 * 24 * 60 * 60),
          completed_at: relPast(44 * 24 * 60 * 60),
          created_at: relPast(45 * 24 * 60 * 60),
        },
      ],
    }),
  ),

  http.get("/api/v1/resource-calendars", () =>
    HttpResponse.json({
      resources: [
        {
          id: "res-1",
          tenant_id: TENANT_ID,
          name: "Boardroom",
          resource_type: "room",
          location: "HQ — 4th floor",
          capacity: 12,
          caldav_id: "caldav-boardroom",
          created_at: relPast(180 * 24 * 60 * 60),
          updated_at: relPast(30 * 24 * 60 * 60),
        },
      ],
    }),
  ),

  http.get("/api/v1/calendars/shared", () =>
    HttpResponse.json({
      shares: [
        {
          id: "share-1",
          tenant_id: TENANT_ID,
          calendar_id: "cal-work",
          owner_account_id: ACCOUNT_ID,
          target_account_id: "acct-alice",
          permission: "readwrite",
          created_at: relPast(45 * 24 * 60 * 60),
        },
      ],
    }),
  ),

  // ─── Confidential send ─────────────────────────────────────────────
  http.get("/api/v1/secure/:token", () =>
    HttpResponse.json({
      id: "secure-demo",
      tenant_id: TENANT_ID,
      sender_id: ADMIN_USER_ID,
      link_token: "demo-token-abc123",
      encrypted_blob_ref:
        "blob://acme-strictzk/2026/04/27/preview-only-encrypted-payload",
      has_password: false,
      expires_at: dayOffset(7, 12, 0),
      max_views: 5,
      view_count: 1,
      revoked: false,
      created_at: relPast(2 * 60 * 60),
    }),
  ),

  http.get("/api/v1/tenants/:tenantId/confidential-send", () =>
    HttpResponse.json([]),
  ),

  // ─── Contacts / GAL ────────────────────────────────────────────────
  http.get("/api/v1/contacts/gal", () =>
    HttpResponse.json([
      {
        email: "alice@acme.com",
        display_name: "Alice Nguyen",
        org: "Engineering",
        phone: "+1-555-0101",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "bob@acme.com",
        display_name: "Bob Martinez",
        org: "Engineering",
        phone: "+1-555-0102",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "cara@acme.com",
        display_name: "Cara Patel",
        org: "Product",
        phone: "+1-555-0103",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "diego@acme.com",
        display_name: "Diego Ramos",
        org: "Customer Success",
        phone: "+1-555-0104",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "erin@acme.com",
        display_name: "Erin Walsh",
        org: "People",
        phone: "+1-555-0105",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "fred@acme.com",
        display_name: "Fred Okafor",
        org: "Finance",
        phone: "+1-555-0106",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "grace@acme.com",
        display_name: "Grace Liu",
        org: "Legal",
        phone: "+1-555-0107",
        last_synced_at: relPast(2 * 60 * 60),
      },
      {
        email: "henry@acme.com",
        display_name: "Henry Schmidt",
        org: "Marketing",
        phone: "+1-555-0108",
        last_synced_at: relPast(2 * 60 * 60),
      },
    ]),
  ),

  // ─── Push / notifications ──────────────────────────────────────────
  http.get("/api/v1/push/preferences", () =>
    HttpResponse.json({
      tenant_id: TENANT_ID,
      user_id: ADMIN_USER_ID,
      mail_new_message: true,
      calendar_event_starting: true,
      calendar_invite_received: true,
      shared_inbox_assigned: true,
      updated_at: relPast(7 * 24 * 60 * 60),
    }),
  ),

  http.get("/api/v1/push/subscriptions", () =>
    HttpResponse.json({ subscriptions: [] }),
  ),

  // ─── Catch-all admin GETs ──────────────────────────────────────────
  // Anything unmocked above returns an empty success so the UI shows
  // "no data" rather than a "failed to fetch" banner.
  http.get("/api/v1/*", () => HttpResponse.json({})),
];

// ─── Audit fixtures (kept at the bottom for readability) ─────────────

const auditEntries = [
  audit("audit-1", relPast(30 * 60), "user.login", "user", ADMIN_USER_ID, "demo@kmail.dev"),
  audit("audit-2", relPast(45 * 60), "user.created", "user", "user-cara", "Demo Admin"),
  audit("audit-3", relPast(2 * 60 * 60), "domain.verified", "domain", DOMAIN_ID, "Demo Admin"),
  audit("audit-4", relPast(3 * 60 * 60), "email.sent", "email", "msg-2", "demo@kmail.dev"),
  audit("audit-5", relPast(6 * 60 * 60), "plan.changed", "tenant", TENANT_ID, "Demo Admin"),
  audit("audit-6", relPast(8 * 60 * 60), "webauthn.registered", "credential", "wa-1", "demo@kmail.dev"),
  audit("audit-7", relPast(12 * 60 * 60), "cmk.rotated", "key", "cmk-active", "Demo Admin"),
  audit("audit-8", relPast(20 * 60 * 60), "retention.run", "policy", "ret-1", "system"),
  audit("audit-9", relPast(28 * 60 * 60), "vault.folder_created", "folder", "vault-1", "demo@kmail.dev"),
  audit("audit-10", relPast(36 * 60 * 60), "scim.token_generated", "token", "scim-1", "Demo Admin"),
];

function audit(
  id: string,
  at: string,
  action: string,
  resource_type: string,
  resource_id: string,
  actor: string,
) {
  return {
    id,
    tenant_id: TENANT_ID,
    actor_id: actor,
    actor_type: actor === "system" ? "system" : "admin",
    action,
    resource_type,
    resource_id,
    metadata: null,
    ip_address: "203.0.113.50",
    user_agent: "Mozilla/5.0 (Macintosh) KMail-Web/1.0",
    prev_hash: "demo-prev",
    entry_hash: "demo-hash",
    created_at: at,
  };
}
