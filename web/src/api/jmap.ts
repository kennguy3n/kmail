import type { JmapInvocation, JmapSession } from "../types";

/**
 * JMAP client scaffold.
 *
 * Every method in this module speaks to the Go BFF, not to
 * Stalwart directly. The BFF enforces tenant policy, capability
 * gating, rate limiting, and error mapping — see
 * docs/JMAP-CONTRACT.md for the contract this file implements
 * against.
 *
 * Phase 1: placeholders only. No network calls are wired.
 */

/** Base URL for all BFF-owned endpoints. */
export const JMAP_BASE_URL = "/jmap";

/** Well-known discovery URL; spec-mandated redirect. */
export const JMAP_SESSION_URL = "/.well-known/jmap";

/**
 * Fetch the JMAP session object. Phase 2 implementation.
 *
 * See docs/JMAP-CONTRACT.md §4.1 for the shape the BFF returns.
 */
export async function fetchSession(): Promise<JmapSession> {
  throw new Error("kmail-web: fetchSession() not yet implemented");
}

/**
 * Send a batch of JMAP invocations. Phase 2 implementation.
 *
 * See docs/JMAP-CONTRACT.md §6 for batching rules (max 16
 * invocations, tenant scoping, long-running operation ticketing).
 */
export async function invoke(
  _invocations: JmapInvocation[],
): Promise<never> {
  throw new Error("kmail-web: invoke() not yet implemented");
}
