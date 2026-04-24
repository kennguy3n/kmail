import { Navigate, Route, Routes } from "react-router-dom";

import Layout from "./components/Layout";
import Inbox from "./pages/Mail/Inbox";
import Compose from "./pages/Mail/Compose";
import MessageView from "./pages/Mail/MessageView";
import CalendarView from "./pages/Calendar/CalendarView";
import EventCreate from "./pages/Calendar/EventCreate";
import TenantAdmin from "./pages/Admin/TenantAdmin";
import DomainAdmin from "./pages/Admin/DomainAdmin";
import UserAdmin from "./pages/Admin/UserAdmin";

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
        <Route path="mail/:mailboxId/:emailId" element={<MessageView />} />

        <Route path="calendar" element={<CalendarView />} />
        <Route path="calendar/new" element={<EventCreate />} />
        <Route path="calendar/:eventId" element={<CalendarView />} />
        <Route path="calendar/:eventId/edit" element={<EventCreate />} />

        <Route path="admin/tenant" element={<TenantAdmin />} />
        <Route path="admin/domains" element={<DomainAdmin />} />
        <Route path="admin/users" element={<UserAdmin />} />

        <Route path="*" element={<Navigate to="/mail" replace />} />
      </Route>
    </Routes>
  );
}
