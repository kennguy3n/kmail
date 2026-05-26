// Email Snooze (WS5) client surface.
//
// Backed by `internal/snooze/handlers.go`. The Inbox row-action
// + MessageView toolbar invoke `snoozeEmail`; the Snoozed page
// uses `listSnoozes` + `wakeSnooze` for the row-level cancel
// (eager wake) action.

import { DEV_BEARER_TOKEN } from "./jmap";

/** Server-side status values for a snooze row. */
export type SnoozeStatus =
  | "snoozed"
  | "unsnoozed"
  | "cancelled"
  | "failed";

/** Wire body for `POST /api/v1/snooze`. */
export interface SnoozeRequest {
  email_id: string;
  /**
   * The email's current mailbox membership as a JMAP `mailboxIds`
   * map (`{"mb-inbox": true, ...}`). The worker / wake handler
   * restores exactly this set when the snooze fires.
   */
  original_mailbox_ids: Record<string, boolean>;
  /** The hidden "Snoozed" mailbox id the email is moved into. */
  snoozed_mailbox_id: string;
  /** Wake time as an ISO-8601 string. */
  snooze_until: string;
  /**
   * If true (default), `$seen` is cleared on wake so the email
   * resurfaces as unread.
   */
  mark_unread_on_wake?: boolean;
}

/** Shape returned by every snooze endpoint. */
export interface SnoozeSnapshot {
  id: string;
  status: SnoozeStatus;
  email_id: string;
  snoozed_mailbox_id: string;
  snooze_until: string;
  mark_unread_on_wake: boolean;
  attempts: number;
  last_error?: string;
  woken_at?: string;
  created_at: string;
}

interface ListResponse {
  snoozes: SnoozeSnapshot[];
}

/** Snooze an email. Throws on validation / dispatch errors. */
export async function snoozeEmail(req: SnoozeRequest): Promise<SnoozeSnapshot> {
  const res = await fetch("/api/v1/snooze", {
    method: "POST",
    credentials: "include",
    headers: {
      Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
      "Content-Type": "application/json",
      Accept: "application/json",
    },
    body: JSON.stringify(req),
  });
  if (res.status !== 201) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`snoozeEmail: ${res.status} ${text}`);
  }
  return (await res.json()) as SnoozeSnapshot;
}

/** List every snooze owned by the authenticated user (newest first). */
export async function listSnoozes(): Promise<SnoozeSnapshot[]> {
  const res = await fetch("/api/v1/snoozed", {
    method: "GET",
    credentials: "include",
    headers: {
      Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
      Accept: "application/json",
    },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`listSnoozes: ${res.status} ${text}`);
  }
  const body = (await res.json()) as ListResponse;
  return body.snoozes ?? [];
}

/** Fetch a single snooze by id. */
export async function getSnooze(id: string): Promise<SnoozeSnapshot> {
  const res = await fetch(`/api/v1/snoozed/${encodeURIComponent(id)}`, {
    method: "GET",
    credentials: "include",
    headers: {
      Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
      Accept: "application/json",
    },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`getSnooze: ${res.status} ${text}`);
  }
  return (await res.json()) as SnoozeSnapshot;
}

/**
 * Wake a snoozed email immediately — applies the reverse JMAP
 * patch and flips the row to `cancelled`. Idempotent: returns
 * `{ cancelled: true }` whether the row was previously snoozed
 * (just-woken) or already terminal (no-op).
 */
export async function wakeSnooze(id: string): Promise<{ cancelled: boolean }> {
  const res = await fetch(`/api/v1/snoozed/${encodeURIComponent(id)}`, {
    method: "DELETE",
    credentials: "include",
    headers: {
      Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
      Accept: "application/json",
    },
  });
  if (res.status === 200) {
    return { cancelled: true };
  }
  const text = await res.text().catch(() => res.statusText);
  throw new Error(`wakeSnooze: ${res.status} ${text}`);
}

/** Quick-pick presets the Inbox snooze menu offers. */
export interface SnoozePreset {
  label: string;
  /** Compute the wake timestamp relative to `now`. */
  resolve: (now: Date) => Date;
}

/**
 * The default preset set: "Later today", "Tomorrow morning",
 * "This weekend", "Next week". Picked timestamps follow the
 * conventional Gmail/Outlook semantics so users have a familiar
 * mental model.
 */
export function defaultSnoozePresets(): SnoozePreset[] {
  return [
    {
      label: "Later today (3 hours)",
      resolve: (now) => addMinutes(now, 3 * 60),
    },
    {
      label: "Tomorrow morning (8 AM)",
      resolve: (now) => nextMorning(now, 8),
    },
    {
      label: "This weekend (Sat 8 AM)",
      resolve: (now) => nextWeekday(now, 6, 8),
    },
    {
      label: "Next week (Mon 8 AM)",
      resolve: (now) => nextWeekday(now, 1, 8),
    },
  ];
}

function addMinutes(d: Date, minutes: number): Date {
  return new Date(d.getTime() + minutes * 60_000);
}

/** Next occurrence of HH:00 the day after `d` in user-local time. */
function nextMorning(d: Date, hour: number): Date {
  const out = new Date(d);
  out.setDate(out.getDate() + 1);
  out.setHours(hour, 0, 0, 0);
  return out;
}

/**
 * Next occurrence of the given weekday at HH:00 in user-local
 * time. `targetWeekday` is JS-style (0 = Sunday, 6 = Saturday).
 * If `d` already falls on the target weekday, we still skip to
 * the following week — the preset means "next" Saturday, not
 * "today if Saturday".
 */
function nextWeekday(d: Date, targetWeekday: number, hour: number): Date {
  const out = new Date(d);
  let delta = (targetWeekday - out.getDay() + 7) % 7;
  if (delta === 0) delta = 7;
  out.setDate(out.getDate() + delta);
  out.setHours(hour, 0, 0, 0);
  return out;
}
