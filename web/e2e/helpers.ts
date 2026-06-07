/**
 * Shared helpers for the KMail Playwright E2E suite.
 *
 * Every spec drives the real React SPA against the in-browser MSW
 * mock layer (`src/mocks/`). The dev server is booted by Playwright's
 * `webServer` block with `VITE_MOCK_API=true`, so the service worker
 * is registered before React mounts and all `/jmap` + `/api/v1/*`
 * traffic resolves from the mock handlers.
 *
 * The helpers below centralise the two things every spec needs:
 *  - waiting for the service worker to be controlling the page, so a
 *    spec never races the very first mocked request, and
 *  - the admin tenant picker, which gates every admin page behind a
 *    single "Acme Corp" tenant.
 */
import { expect, type Page } from "@playwright/test";

/** The single demo tenant exposed by the mock `GET /api/v1/tenants`. */
export const TENANT_NAME = "Acme Corp";

/**
 * Id of that tenant. Admin pages render the tenant option label
 * differently (`"Acme Corp"` vs `"Acme Corp (acme)"`), so specs
 * select by the stable option *value* (the tenant id) rather than by
 * label text.
 */
export const TENANT_ID = "00000000-0000-0000-0000-000000000001";

/**
 * Navigate to `path` and wait until the MSW service worker controls
 * the page. `main.tsx` calls `worker.start()` before `createRoot`,
 * but the worker only intercepts fetches once it is *controlling* the
 * client; on a cold load that can land a tick after React's first
 * render. Waiting on `serviceWorker.controller` keeps specs
 * deterministic without arbitrary sleeps.
 */
export async function gotoMocked(page: Page, path: string): Promise<void> {
  await page.goto(path);
  await page.waitForFunction(() => {
    const sw = navigator.serviceWorker;
    return !!sw && sw.controller !== null;
  });
}

/**
 * Select the demo tenant in an admin page's "Tenant" picker and wait
 * for the selection to take. Admin pages persist the choice via
 * `useTenantSelection`, so callers can rely on tenant-scoped data
 * loading immediately afterwards.
 */
export async function selectTenant(page: Page): Promise<void> {
  const picker = page.getByLabel(/tenant/i).first();
  await expect(picker).toBeVisible();
  await picker.selectOption(TENANT_ID);
}
