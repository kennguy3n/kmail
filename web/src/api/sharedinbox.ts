import { ADMIN_API_BASE, adminAuthHeaders, requestJSON } from "./admin";

export type AssignmentStatus =
  | "open"
  | "in_progress"
  | "waiting"
  | "resolved"
  | "closed";

export interface EmailAssignment {
  id: string;
  tenant_id: string;
  shared_inbox_id: string;
  email_id: string;
  assignee_user_id: string;
  status: AssignmentStatus;
  created_at: string;
  updated_at: string;
}

export interface InternalNote {
  id: string;
  tenant_id: string;
  shared_inbox_id: string;
  email_id: string;
  author_user_id: string;
  note_text: string;
  created_at: string;
}

export async function listAssignments(
  inboxId: string,
  opts: { status?: AssignmentStatus; assigneeUserId?: string } = {},
): Promise<EmailAssignment[]> {
  const params = new URLSearchParams();
  if (opts.status) params.set("status", opts.status);
  if (opts.assigneeUserId) params.set("assignee_user_id", opts.assigneeUserId);
  const res = await requestJSON<{ assignments: EmailAssignment[] }>(
    `${ADMIN_API_BASE}/shared-inboxes/${encodeURIComponent(inboxId)}/assignments?${params}`,
    { method: "GET", headers: adminAuthHeaders(undefined, { Accept: "application/json" }) },
  );
  return res.assignments ?? [];
}

export async function assignEmail(
  inboxId: string,
  emailId: string,
  assigneeUserId: string,
): Promise<EmailAssignment> {
  return requestJSON<EmailAssignment>(
    `${ADMIN_API_BASE}/shared-inboxes/${encodeURIComponent(inboxId)}/emails/${encodeURIComponent(emailId)}/assign`,
    {
      method: "POST",
      headers: adminAuthHeaders(undefined, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ assignee_user_id: assigneeUserId }),
    },
  );
}

export async function unassignEmail(inboxId: string, emailId: string): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/shared-inboxes/${encodeURIComponent(inboxId)}/emails/${encodeURIComponent(emailId)}/assign`,
    { method: "DELETE", headers: adminAuthHeaders() },
  );
}

export async function setStatus(
  inboxId: string,
  emailId: string,
  status: AssignmentStatus,
): Promise<EmailAssignment> {
  return requestJSON<EmailAssignment>(
    `${ADMIN_API_BASE}/shared-inboxes/${encodeURIComponent(inboxId)}/emails/${encodeURIComponent(emailId)}/status`,
    {
      method: "PUT",
      headers: adminAuthHeaders(undefined, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ status }),
    },
  );
}

export async function listNotes(inboxId: string, emailId: string): Promise<InternalNote[]> {
  const res = await requestJSON<{ notes: InternalNote[] }>(
    `${ADMIN_API_BASE}/shared-inboxes/${encodeURIComponent(inboxId)}/emails/${encodeURIComponent(emailId)}/notes`,
    { method: "GET", headers: adminAuthHeaders(undefined, { Accept: "application/json" }) },
  );
  return res.notes ?? [];
}

export async function addNote(
  inboxId: string,
  emailId: string,
  noteText: string,
): Promise<InternalNote> {
  return requestJSON<InternalNote>(
    `${ADMIN_API_BASE}/shared-inboxes/${encodeURIComponent(inboxId)}/emails/${encodeURIComponent(emailId)}/notes`,
    {
      method: "POST",
      headers: adminAuthHeaders(undefined, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ note_text: noteText }),
    },
  );
}
