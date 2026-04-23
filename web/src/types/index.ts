/**
 * Shared TypeScript types for the KMail React client.
 *
 * These types describe the shape of the Go BFF's JMAP surface
 * (see docs/JMAP-CONTRACT.md). They are intentionally permissive
 * in Phase 1 and will be tightened when the BFF contract lands.
 */

/** A JMAP method invocation in a batch. */
export type JmapInvocation = [
  method: string,
  args: Record<string, unknown>,
  callId: string,
];

/** The JMAP session object the BFF returns at the session URL. */
export interface JmapSession {
  capabilities: Record<string, unknown>;
  accounts: Record<string, JmapAccount>;
  primaryAccounts: Record<string, string>;
  username: string;
  apiUrl: string;
  downloadUrl: string;
  uploadUrl: string;
  eventSourceUrl: string;
  state: string;
}

/** A JMAP account advertised in the session object. */
export interface JmapAccount {
  name: string;
  isPersonal: boolean;
  isReadOnly: boolean;
  accountCapabilities: Record<string, unknown>;
}

/** KMail tenant plan, mirrored from `tenants.plan` in docs/SCHEMA.md. */
export type TenantPlan = "core" | "pro" | "privacy";

/** Privacy mode for a mailbox or message. */
export type PrivacyMode =
  | "standard"
  | "confidential-send"
  | "zero-access-vault";
