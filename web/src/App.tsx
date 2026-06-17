import { Suspense, lazy } from "react";
import { Navigate, Route, Routes } from "react-router-dom";

import Layout from "./components/Layout";
import { RouteFallback } from "./components/RouteFallback";

// Every route page is loaded with React.lazy so each becomes its own
// async chunk instead of being bundled into one ~900 KB entry. The
// heavy TipTap editor (used only by the compose/signature/template
// routes) and the rarely-visited admin screens now load on demand;
// the Suspense boundaries below (and the one around Layout's Outlet)
// render <RouteFallback /> while a chunk is fetched. Layout itself
// stays eager because it is the always-present app shell.
const Inbox = lazy(() => import("./pages/Mail/Inbox"));
const Compose = lazy(() => import("./pages/Mail/Compose"));
const MessageView = lazy(() => import("./pages/Mail/MessageView"));
const SharedInboxView = lazy(() => import("./pages/Mail/SharedInboxView"));
const VaultView = lazy(() => import("./pages/Mail/VaultView"));
const ProtectedFolderView = lazy(() => import("./pages/Mail/ProtectedFolderView"));
const ScheduledSends = lazy(() => import("./pages/Mail/ScheduledSends"));
const Snoozed = lazy(() => import("./pages/Mail/Snoozed"));
const SecurePortal = lazy(() => import("./pages/Mail/SecurePortal"));
const ThreadView = lazy(() => import("./pages/Mail/ThreadView"));
const SignatureEditor = lazy(() => import("./pages/Mail/SignatureEditor"));
const Templates = lazy(() => import("./pages/Mail/Templates"));
const Labels = lazy(() => import("./pages/Mail/Labels"));
const OutOfOffice = lazy(() => import("./pages/Mail/OutOfOffice"));
const Delegation = lazy(() => import("./pages/Mail/Delegation"));
const CalendarView = lazy(() => import("./pages/Calendar/CalendarView"));
const EventCreate = lazy(() => import("./pages/Calendar/EventCreate"));
const SharedCalendars = lazy(() => import("./pages/Calendar/SharedCalendars"));
const TenantAdmin = lazy(() => import("./pages/Admin/TenantAdmin"));
const DomainAdmin = lazy(() => import("./pages/Admin/DomainAdmin"));
const UserAdmin = lazy(() => import("./pages/Admin/UserAdmin"));
const QuotaAdmin = lazy(() => import("./pages/Admin/QuotaAdmin"));
const AuditAdmin = lazy(() => import("./pages/Admin/AuditAdmin"));
const DmarcAdmin = lazy(() => import("./pages/Admin/DmarcAdmin"));
const DnsWizard = lazy(() => import("./pages/Admin/DnsWizard"));
const IpReputationAdmin = lazy(() => import("./pages/Admin/IpReputationAdmin"));
const NotificationPrefs = lazy(() => import("./pages/Admin/NotificationPrefs"));
const MigrationAdmin = lazy(() => import("./pages/Admin/MigrationAdmin"));
const ResourceCalendarAdmin = lazy(() => import("./pages/Admin/ResourceCalendarAdmin"));
const PricingAdmin = lazy(() => import("./pages/Admin/PricingAdmin"));
const PricingPage = lazy(() => import("./pages/Admin/PricingPage"));
const SloAdmin = lazy(() => import("./pages/Admin/SloAdmin"));
const StoragePlacementAdmin = lazy(() => import("./pages/Admin/StoragePlacementAdmin"));
const RetentionAdmin = lazy(() => import("./pages/Admin/RetentionAdmin"));
const ApprovalAdmin = lazy(() => import("./pages/Admin/ApprovalAdmin"));
const ExportAdmin = lazy(() => import("./pages/Admin/ExportAdmin"));
const CmkAdmin = lazy(() => import("./pages/Admin/CmkAdmin"));
const ScimAdmin = lazy(() => import("./pages/Admin/ScimAdmin"));
const WebhookAdmin = lazy(() => import("./pages/Admin/WebhookAdmin"));
const OnboardingChecklist = lazy(() => import("./pages/Admin/OnboardingChecklist"));
const SearchAdmin = lazy(() => import("./pages/Admin/SearchAdmin"));
const DkimAdmin = lazy(() => import("./pages/Admin/DkimAdmin"));
const SieveAdmin = lazy(() => import("./pages/Admin/SieveAdmin"));
const SecuritySettings = lazy(() => import("./pages/Admin/SecuritySettings"));
const EmailAnalytics = lazy(() => import("./pages/Admin/EmailAnalytics"));
const ContactsView = lazy(() => import("./pages/Mail/ContactsView"));
const Signup = lazy(() => import("./pages/Signup"));

// Dev-only component gallery, code-split via a dynamic import so it is
// never part of the production bundle. `import.meta.env.DEV` is
// statically replaced with `false` in production builds, so Rollup
// drops this whole ternary branch — including the `import()` — and the
// Showcase chunk is never emitted.
const Showcase = import.meta.env.DEV
  ? lazy(() => import("./components/Showcase"))
  : null;

/**
 * App is the KMail React entrypoint.
 *
 * It mounts the shared {@link Layout} shell and routes requests to
 * the Mail, Calendar, and Admin placeholder pages. The BFF contract
 * the underlying pages speak to is pinned in
 * docs/JMAP-CONTRACT.md; this file owns only the URL shape.
 */
export default function App() {
  return (
    // Outer boundary for the full-page routes that render outside the
    // Layout shell (secure portal, signup); the in-shell routes are
    // caught by the Suspense around Layout's Outlet so the nav stays
    // mounted while their chunk loads.
    <Suspense fallback={<RouteFallback />}>
      <Routes>
        {/* Confidential Send portal lives outside the Layout shell —
            the recipient is unauthenticated and should not see the
            KMail nav or admin chrome. */}
        <Route path="secure/:token" element={<SecurePortal />} />

        {/* Self-service signup funnel — public, pre-auth, outside the
            Layout shell (no tenant/session exists yet). */}
        <Route path="signup" element={<Signup />} />

        <Route element={<Layout />}>
          <Route index element={<Navigate to="/mail" replace />} />

          <Route path="mail" element={<Inbox />} />
          <Route path="mail/priority" element={<Inbox />} />
          <Route path="mail/compose" element={<Compose />} />
          <Route path="mail/shared" element={<SharedInboxView />} />
          <Route path="mail/vault" element={<VaultView />} />
          <Route path="mail/protected-folders" element={<ProtectedFolderView />} />
          <Route path="mail/scheduled" element={<ScheduledSends />} />
          <Route path="mail/snoozed" element={<Snoozed />} />
          <Route path="mail/signatures" element={<SignatureEditor />} />
          <Route path="mail/templates" element={<Templates />} />
          <Route path="mail/labels" element={<Labels />} />
          <Route path="mail/out-of-office" element={<OutOfOffice />} />
          <Route path="mail/delegation" element={<Delegation />} />
          <Route path="mail/thread/:threadId" element={<ThreadView />} />
          <Route path="mail/:mailboxId/:emailId" element={<MessageView />} />

          <Route path="calendar" element={<CalendarView />} />
          <Route path="calendar/new" element={<EventCreate />} />
          <Route path="calendar/shared" element={<SharedCalendars />} />
          <Route path="calendar/:eventId" element={<CalendarView />} />
          <Route path="calendar/:eventId/edit" element={<EventCreate />} />

          <Route path="admin/tenant" element={<TenantAdmin />} />
          <Route path="admin/domains" element={<DomainAdmin />} />
          <Route path="admin/dns-wizard" element={<DnsWizard />} />
          <Route path="admin/users" element={<UserAdmin />} />
          <Route path="admin/billing" element={<QuotaAdmin />} />
          <Route path="admin/audit" element={<AuditAdmin />} />
          <Route path="admin/dmarc" element={<DmarcAdmin />} />
          <Route path="admin/ip-reputation" element={<IpReputationAdmin />} />
          <Route path="admin/notifications" element={<NotificationPrefs />} />
          <Route path="admin/migrations" element={<MigrationAdmin />} />
          <Route path="admin/resource-calendars" element={<ResourceCalendarAdmin />} />
          <Route path="admin/pricing" element={<PricingAdmin />} />
          <Route path="admin/pricing-plans" element={<PricingPage />} />
          <Route path="admin/slo" element={<SloAdmin />} />
          <Route path="admin/storage-placement" element={<StoragePlacementAdmin />} />
          <Route path="admin/retention" element={<RetentionAdmin />} />
          <Route path="admin/approvals" element={<ApprovalAdmin />} />
          <Route path="admin/exports" element={<ExportAdmin />} />
          <Route path="admin/cmk" element={<CmkAdmin />} />
          <Route path="admin/scim" element={<ScimAdmin />} />
          <Route path="admin/webhooks" element={<WebhookAdmin />} />
          <Route path="admin/onboarding" element={<OnboardingChecklist />} />
          <Route path="admin/search" element={<SearchAdmin />} />
          <Route path="admin/dkim" element={<DkimAdmin />} />
          <Route path="admin/sieve" element={<SieveAdmin />} />
          <Route path="admin/security" element={<SecuritySettings />} />
          <Route path="admin/email-analytics" element={<EmailAnalytics />} />
          <Route path="contacts" element={<ContactsView />} />

          {/* Non-production component gallery (WS1) for visual QA.
              Dev-only: in a production build this route is omitted, so
              `/showcase` falls through to the catch-all redirect below. */}
          {Showcase && (
            <Route
              path="showcase"
              element={
                <Suspense fallback={null}>
                  <Showcase />
                </Suspense>
              }
            />
          )}

          <Route path="*" element={<Navigate to="/mail" replace />} />
        </Route>
      </Routes>
    </Suspense>
  );
}
