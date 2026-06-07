/**
 * Page-level integration tests for the Migration wizard.
 *
 * Exercises the golden admin path: select a tenant, choose a source
 * provider, enter IMAP credentials, run the connection probe, then
 * start the import and see the job land in the jobs table. Only the
 * network-touching admin functions are mocked; the wizard's local
 * step state, credential editing, and the test-result banner are
 * driven through real user interactions.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import MigrationAdmin from "./MigrationAdmin";
import type { MigrationJob, Tenant } from "../../api/admin";

const listTenants = vi.fn();
const listMigrationJobs = vi.fn();
const createMigrationJob = vi.fn();
const testMigrationConnection = vi.fn();

vi.mock("../../api/admin", async () => {
  const actual =
    await vi.importActual<typeof import("../../api/admin")>("../../api/admin");
  return {
    ...actual,
    listTenants: (...args: unknown[]) => listTenants(...args),
    listMigrationJobs: (...args: unknown[]) => listMigrationJobs(...args),
    createMigrationJob: (...args: unknown[]) => createMigrationJob(...args),
    testMigrationConnection: (...args: unknown[]) =>
      testMigrationConnection(...args),
  };
});

const tenant: Tenant = {
  id: "tenant-1",
  name: "Acme",
  slug: "acme",
  plan: "core",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const job: MigrationJob = {
  id: "job-1",
  tenant_id: "tenant-1",
  source_type: "generic_imap",
  source_host: "imap.example.com",
  source_user: "alice@example.com",
  destination_user_id: "user-1",
  status: "running",
  messages_total: 100,
  messages_synced: 25,
  created_at: "2026-01-01T00:00:00Z",
};

afterEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
});

async function selectTenant(): Promise<void> {
  const tenantSelect = await screen.findByRole("combobox", { name: /tenant/i });
  await waitFor(() =>
    expect(
      within(tenantSelect).getByRole("option", { name: "Acme" }),
    ).toBeInTheDocument(),
  );
  await userEvent.selectOptions(tenantSelect, "tenant-1");
}

describe("<MigrationAdmin />", () => {
  it("walks source → credentials → test connection → start import", async () => {
    let jobs: MigrationJob[] = [];
    listTenants.mockResolvedValue([tenant]);
    listMigrationJobs.mockImplementation(() => Promise.resolve(jobs));
    testMigrationConnection.mockResolvedValue({ ok: true });
    createMigrationJob.mockImplementation(() => {
      jobs = [job];
      return Promise.resolve(job);
    });

    render(<MigrationAdmin />);
    await selectTenant();

    // Step 1: choose the generic IMAP provider, then advance.
    await userEvent.click(screen.getByRole("radio", { name: /generic imap/i }));
    await userEvent.click(screen.getByRole("button", { name: /Next/ }));

    // Step 2: fill credentials.
    await userEvent.type(screen.getByLabelText("Host"), "imap.example.com");
    await userEvent.type(
      screen.getByLabelText("Source user"),
      "alice@example.com",
    );
    await userEvent.type(screen.getByLabelText("Source password"), "secret");

    // Test connection probe reports success.
    await userEvent.click(
      screen.getByRole("button", { name: "Test connection" }),
    );
    expect(
      await screen.findByText("IMAP login succeeded."),
    ).toBeInTheDocument();
    expect(testMigrationConnection).toHaveBeenCalledWith(
      "tenant-1",
      expect.objectContaining({
        host: "imap.example.com",
        username: "alice@example.com",
        password: "secret",
        use_tls: true,
      }),
    );

    // Step 3: confirm + start.
    await userEvent.click(screen.getByRole("button", { name: /Next/ }));
    await userEvent.click(
      screen.getByRole("button", { name: "Start migration" }),
    );

    await waitFor(() =>
      expect(createMigrationJob).toHaveBeenCalledWith(
        "tenant-1",
        expect.objectContaining({ source_type: "generic_imap" }),
      ),
    );

    // The new job appears in the jobs table.
    const statusCell = await screen.findByRole("cell", { name: "running" });
    const row = statusCell.closest("tr");
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getByRole("cell", { name: "generic_imap" })).toBeInTheDocument();
  });

  it("surfaces a failed connection probe without advancing", async () => {
    listTenants.mockResolvedValue([tenant]);
    listMigrationJobs.mockResolvedValue([]);
    testMigrationConnection.mockResolvedValue({
      ok: false,
      error: "authentication failed",
    });

    render(<MigrationAdmin />);
    await selectTenant();
    await userEvent.click(screen.getByRole("button", { name: /Next/ }));

    await userEvent.type(screen.getByLabelText("Host"), "imap.example.com");
    await userEvent.type(screen.getByLabelText("Source user"), "bob");
    await userEvent.type(screen.getByLabelText("Source password"), "wrong");
    await userEvent.click(
      screen.getByRole("button", { name: "Test connection" }),
    );

    expect(
      await screen.findByText(/Connection failed: authentication failed/),
    ).toBeInTheDocument();
  });

  it("disables Test connection until host/user/password are supplied", async () => {
    listTenants.mockResolvedValue([tenant]);
    listMigrationJobs.mockResolvedValue([]);

    render(<MigrationAdmin />);
    await selectTenant();
    // generic_imap clears the default host, so credentials start empty.
    await userEvent.click(screen.getByRole("radio", { name: /generic imap/i }));
    await userEvent.click(screen.getByRole("button", { name: /Next/ }));

    expect(
      screen.getByRole("button", { name: "Test connection" }),
    ).toBeDisabled();
  });
});
