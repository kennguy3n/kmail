import { ADMIN_API_BASE, requestJSON } from "./admin";

export interface CalendarShare {
  id: string;
  tenant_id: string;
  calendar_id: string;
  owner_account_id: string;
  target_account_id: string;
  permission: "read" | "readwrite" | "admin";
  created_at: string;
}

export interface ResourceCalendar {
  id: string;
  tenant_id: string;
  name: string;
  resource_type: "room" | "equipment" | "vehicle";
  location: string;
  capacity: number;
  caldav_id: string;
  created_at: string;
  updated_at: string;
}

export async function createCalendar(input: {
  name: string;
  calendar_type: "personal" | "shared" | "resource";
  description?: string;
  color?: string;
}): Promise<{ id: string; name: string }> {
  return requestJSON(`${ADMIN_API_BASE}/calendars`, {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function shareCalendar(
  calendarId: string,
  targetAccountId: string,
  permission: CalendarShare["permission"],
): Promise<CalendarShare> {
  return requestJSON(
    `${ADMIN_API_BASE}/calendars/${encodeURIComponent(calendarId)}/share`,
    {
      method: "POST",
      headers: { Accept: "application/json", "Content-Type": "application/json" },
      body: JSON.stringify({ target_account_id: targetAccountId, permission }),
    },
  );
}

export async function listSharedCalendars(): Promise<CalendarShare[]> {
  const res = await requestJSON<{ shares: CalendarShare[] }>(
    `${ADMIN_API_BASE}/calendars/shared`,
    { method: "GET", headers: { Accept: "application/json" } },
  );
  return res.shares ?? [];
}

export async function listResourceCalendars(): Promise<ResourceCalendar[]> {
  const res = await requestJSON<{ resources: ResourceCalendar[] }>(
    `${ADMIN_API_BASE}/resource-calendars`,
    { method: "GET", headers: { Accept: "application/json" } },
  );
  return res.resources ?? [];
}

export async function createResourceCalendar(
  input: Partial<ResourceCalendar>,
): Promise<ResourceCalendar> {
  return requestJSON(`${ADMIN_API_BASE}/resource-calendars`, {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function bookResource(
  calendarId: string,
  start: string,
  end: string,
  subject: string,
): Promise<{ uid: string }> {
  return requestJSON(`${ADMIN_API_BASE}/calendars/${encodeURIComponent(calendarId)}/book`, {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    body: JSON.stringify({ start, end, subject }),
  });
}
