import { Navigate, Route, Routes } from "react-router-dom";

import Layout from "./components/Layout";
import Inbox from "./pages/Mail/Inbox";
import Compose from "./pages/Mail/Compose";
import MessageView from "./pages/Mail/MessageView";
import SharedInboxView from "./pages/Mail/SharedInboxView";
import CalendarView from "./pages/Calendar/CalendarView";
import EventCreate from "./pages/Calendar/EventCreate";
import SharedCalendars from "./pages/Calendar/SharedCalendars";
import TenantAdmin from "./pages/Admin/TenantAdmin";
import DomainAdmin from "./pages/Admin/DomainAdmin";
import UserAdmin from "./pages/Admin/UserAdmin";
import QuotaAdmin from "./pages/Admin/QuotaAdmin";
import AuditAdmin from "./pages/Admin/AuditAdmin";
import DmarcAdmin from "./pages/Admin/DmarcAdmin";
import DnsWizard from "./pages/Admin/DnsWizard";
import IpReputationAdmin from "./pages/Admin/IpReputationAdmin";
import NotificationPrefs from "./pages/Admin/NotificationPrefs";
import MigrationAdmin from "./pages/Admin/MigrationAdmin";
import ResourceCalendarAdmin from "./pages/Admin/ResourceCalendarAdmin";
import PricingAdmin from "./pages/Admin/PricingAdmin";

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
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/mail" replace />} />

        <Route path="mail" element={<Inbox />} />
        <Route path="mail/compose" element={<Compose />} />
        <Route path="mail/shared" element={<SharedInboxView />} />
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

        <Route path="*" element={<Navigate to="/mail" replace />} />
      </Route>
    </Routes>
  );
}
