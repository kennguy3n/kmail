import { Link, Outlet } from "react-router-dom";

/**
 * Layout is the shared shell around every KMail page.
 *
 * In Phase 2 this component will host the KChat-embedded chrome,
 * the account switcher, and the shared notification toast surface
 * that subscribes to JMAP push (see docs/JMAP-CONTRACT.md §5). In
 * Phase 1 it is a minimal two-pane shell so the router has
 * something to render.
 */
export default function Layout() {
  return (
    <div className="kmail-shell">
      <nav className="kmail-nav">
        <h1>KMail</h1>
        <ul>
          <li><Link to="/mail">Mail</Link></li>
          <li><Link to="/mail/compose">Compose</Link></li>
          <li><Link to="/mail/shared">Shared inboxes</Link></li>
          <li><Link to="/calendar">Calendar</Link></li>
          <li><Link to="/calendar/new">New event</Link></li>
          <li><Link to="/calendar/shared">Shared calendars</Link></li>
          <li><Link to="/admin/tenant">Tenant admin</Link></li>
          <li><Link to="/admin/domains">Domain admin</Link></li>
          <li><Link to="/admin/dns-wizard">DNS wizard</Link></li>
          <li><Link to="/admin/users">User admin</Link></li>
          <li><Link to="/admin/billing">Billing</Link></li>
          <li><Link to="/admin/audit">Audit log</Link></li>
          <li><Link to="/admin/dmarc">DMARC</Link></li>
          <li><Link to="/admin/ip-reputation">IP reputation</Link></li>
          <li><Link to="/admin/notifications">Notifications</Link></li>
          <li><Link to="/admin/migrations">Migrations</Link></li>
          <li><Link to="/admin/resource-calendars">Resource calendars</Link></li>
        </ul>
      </nav>
      <main className="kmail-main">
        <Outlet />
      </main>
    </div>
  );
}
