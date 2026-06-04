/**
 * Component tests for SearchAdmin.tsx focused on the cutover
 * surface added in Session 5. The backend-selector cards are
 * exercised elsewhere; this file pins the load-bearing cutover
 * contract:
 *
 *   - The history table renders one row per `search_cutover_jobs`
 *     entry returned by GET .../search/cutover.
 *   - "Start cutover" POSTs the chosen target backend and reflects
 *     the terminal job state back to the operator.
 *   - A failed cutover (thrown AdminApiError) surfaces the error and
 *     still refreshes the history so the operator sees the `failed`
 *     row.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import SearchAdmin, { type CutoverJob } from "./SearchAdmin";

const listTenants = vi.fn();
const getSearchBackend = vi.fn();
const listAvailableSearchBackends = vi.fn();
const requestJSON = vi.fn();

vi.mock("../../api/admin", async () => {
  // Keep the real `ADMIN_API_BASE` / `adminAuthHeaders` / error
  // class / backend constants — the component imports them as
  // values — and stub only the network-touching functions.
  const actual =
    await vi.importActual<typeof import("../../api/admin")>("../../api/admin");
  return {
    ...actual,
    listTenants: (...args: unknown[]) => listTenants(...args),
    getSearchBackend: (...args: unknown[]) => getSearchBackend(...args),
    listAvailableSearchBackends: (...args: unknown[]) =>
      listAvailableSearchBackends(...args),
    requestJSON: (...args: unknown[]) => requestJSON(...args),
  };
});

const tenant = {
  id: "tenant-1",
  name: "Acme",
  slug: "acme",
  plan: "core",
  status: "active",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

const completedJob: CutoverJob = {
  tenant_id: "tenant-1",
  target_backend: "shared_opensearch",
  cutover_state: "completed",
  mailbox_size: 200000,
  threshold: 100000,
  started_at: "2026-01-01T00:00:00Z",
  completed_at: "2026-01-01T00:05:00Z",
  failure_count: 0,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:05:00Z",
};

afterEach(() => {
  vi.clearAllMocks();
  if (typeof window !== "undefined") {
    window.localStorage.clear();
  }
});

// routeRequestJSON dispatches the mocked requestJSON by method so a
// single mock can serve both the history GET and the trigger POST.
function routeRequestJSON(
  onPost?: (body: unknown) => unknown,
  jobs: CutoverJob[] = [],
): void {
  requestJSON.mockImplementation((url: string, init?: RequestInit) => {
    const method = (init?.method ?? "GET").toUpperCase();
    if (url.endsWith("/search/cutover") && method === "GET") {
      return Promise.resolve({ jobs });
    }
    if (url.endsWith("/search/cutover") && method === "POST") {
      const body = init?.body ? JSON.parse(String(init.body)) : {};
      return Promise.resolve(onPost ? onPost(body) : completedJob);
    }
    return Promise.reject(new Error(`unexpected request: ${method} ${url}`));
  });
}

async function renderSearchAdmin(): Promise<void> {
  listTenants.mockResolvedValueOnce([tenant]);
  getSearchBackend.mockResolvedValue({ backend: "shared_meilisearch" });
  listAvailableSearchBackends.mockResolvedValue([
    "shared_meilisearch",
    "shared_opensearch",
  ]);
  render(<SearchAdmin />);
  // Wait for the cutover section (rendered inside `{config && ...}`)
  // to appear, which implies the backend config has settled.
  await screen.findByRole("heading", { name: "Cutover", level: 3 });
}

describe("<SearchAdmin /> cutover", () => {
  it("renders the cutover history table from the API", async () => {
    routeRequestJSON(undefined, [completedJob]);
    await renderSearchAdmin();

    // The history row shows target + state. Use the `cell` role so
    // we match the `<td>` value rather than the `<th>` "Completed"
    // column header.
    const row = (await screen.findByRole("cell", { name: "Completed" })).closest("tr");
    expect(row).not.toBeNull();
    expect(row).toHaveTextContent("shared_opensearch");
  });

  it("shows the empty state when no cutovers have run", async () => {
    routeRequestJSON(undefined, []);
    await renderSearchAdmin();

    expect(
      await screen.findByText("No cutovers have run for this tenant."),
    ).toBeInTheDocument();
  });

  it("posts the selected target backend and reports the result", async () => {
    const onPost = vi.fn(() => completedJob);
    routeRequestJSON(onPost, []);
    await renderSearchAdmin();

    fireEvent.change(await screen.findByRole("combobox", { name: /target backend/i }), {
      target: { value: "shared_opensearch" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start cutover" }));

    await waitFor(() => {
      expect(onPost).toHaveBeenCalledWith({ target_backend: "shared_opensearch" });
    });
    expect(
      await screen.findByText(/Cutover to shared_opensearch completed\./),
    ).toBeInTheDocument();
  });

  it("resets the manual-cutover target when the tenant is switched", async () => {
    // Two tenants so we can drive the picker. Both sit on
    // shared_meilisearch, so shared_opensearch is a valid target for
    // either — meaning a stale selection would NOT be filtered out of
    // the <select> and would keep the trigger enabled if it survived
    // the switch.
    const tenant2 = { ...tenant, id: "tenant-2", name: "Beta", slug: "beta" };
    listTenants.mockResolvedValueOnce([tenant, tenant2]);
    getSearchBackend.mockResolvedValue({ backend: "shared_meilisearch" });
    listAvailableSearchBackends.mockResolvedValue([
      "shared_meilisearch",
      "shared_opensearch",
    ]);
    routeRequestJSON(undefined, []);
    render(<SearchAdmin />);
    await screen.findByRole("heading", { name: "Cutover", level: 3 });

    const target = await screen.findByRole("combobox", { name: /target backend/i });
    fireEvent.change(target, { target: { value: "shared_opensearch" } });
    expect((target as HTMLSelectElement).value).toBe("shared_opensearch");
    expect(screen.getByRole("button", { name: "Start cutover" })).toBeEnabled();

    // Switch tenants via the picker.
    fireEvent.change(screen.getByRole("combobox", { name: /tenant/i }), {
      target: { value: "tenant-2" },
    });

    await waitFor(() => {
      expect(getSearchBackend).toHaveBeenCalledWith("tenant-2");
    });
    // The stale target must not carry over: select resets to "" and
    // the trigger is disabled until the operator picks a target for
    // the new tenant.
    expect((target as HTMLSelectElement).value).toBe("");
    expect(screen.getByRole("button", { name: "Start cutover" })).toBeDisabled();
  });

  it("clears the prior tenant's cutover history (no stale flash) on switch", async () => {
    // Tenant-1 has a completed cutover; tenant-2's history GET is held
    // open so we can observe the window between the switch and the new
    // response. Without resetting `cutoverJobs` to null in `reload`,
    // tenant-1's row would stay visible under tenant-2 during that gap.
    const tenant2 = { ...tenant, id: "tenant-2", name: "Beta", slug: "beta" };
    listTenants.mockResolvedValueOnce([tenant, tenant2]);
    getSearchBackend.mockResolvedValue({ backend: "shared_meilisearch" });
    listAvailableSearchBackends.mockResolvedValue([
      "shared_meilisearch",
      "shared_opensearch",
    ]);
    let releaseTenant2Jobs!: (jobs: { jobs: CutoverJob[] }) => void;
    const tenant2Jobs = new Promise<{ jobs: CutoverJob[] }>((resolve) => {
      releaseTenant2Jobs = resolve;
    });
    requestJSON.mockImplementation((url: string, init?: RequestInit) => {
      const method = (init?.method ?? "GET").toUpperCase();
      if (method === "GET" && url.includes("tenant-2")) {
        return tenant2Jobs;
      }
      if (method === "GET") {
        return Promise.resolve({ jobs: [completedJob] });
      }
      return Promise.reject(new Error(`unexpected request: ${method} ${url}`));
    });
    render(<SearchAdmin />);
    await screen.findByRole("heading", { name: "Cutover", level: 3 });
    // Tenant-1's completed row is on screen.
    expect(await screen.findByRole("cell", { name: "Completed" })).toBeInTheDocument();

    fireEvent.change(screen.getByRole("combobox", { name: /tenant/i }), {
      target: { value: "tenant-2" },
    });

    await waitFor(() => {
      expect(getSearchBackend).toHaveBeenCalledWith("tenant-2");
    });
    // While tenant-2's history is still loading, tenant-1's row must be
    // gone and the loading affordance shown — not a stale cross-tenant flash.
    expect(screen.queryByRole("cell", { name: "Completed" })).toBeNull();
    expect(screen.getByText("Loading…")).toBeInTheDocument();

    releaseTenant2Jobs({ jobs: [] });
    expect(
      await screen.findByText("No cutovers have run for this tenant."),
    ).toBeInTheDocument();
  });

  it("surfaces a failed cutover and keeps the tenant readable", async () => {
    const { AdminApiError } =
      await vi.importActual<typeof import("../../api/admin")>("../../api/admin");
    requestJSON.mockImplementation((url: string, init?: RequestInit) => {
      const method = (init?.method ?? "GET").toUpperCase();
      if (method === "GET") {
        return Promise.resolve({
          jobs: [{ ...completedJob, cutover_state: "failed", last_error: "validate: not searchable" }],
        });
      }
      return Promise.reject(
        new AdminApiError(url, 500, "validate: not searchable"),
      );
    });
    await renderSearchAdmin();

    fireEvent.change(await screen.findByRole("combobox", { name: /target backend/i }), {
      target: { value: "shared_opensearch" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start cutover" }));

    expect(await screen.findByText(/validate: not searchable/)).toBeInTheDocument();
    // History refreshed with the failed row.
    expect(await screen.findByText("Failed")).toBeInTheDocument();
  });
});
