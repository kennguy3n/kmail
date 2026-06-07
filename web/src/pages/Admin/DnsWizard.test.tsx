/**
 * Page-level integration tests for the DNS Wizard.
 *
 * The wizard composes three admin reads — tenant list, domain list,
 * and the derived `getDnsWizardStatus` (records + verification) — and
 * walks the operator through the seven required records. These tests
 * mock only the network-touching admin functions and assert on the
 * rendered walkthrough: selecting a tenant/domain loads the steps,
 * the active step shows its record, Verify re-fetches status and
 * flips the badge to "Verified", and a fully-verified domain shows
 * the success summary.
 *
 * Queries are by role / accessible name so they survive the
 * Tailwind + Radix migration.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import DnsWizard from "./DnsWizard";
import type {
  DnsWizardStatus,
  TenantDomain,
  Tenant,
} from "../../api/admin";

const listTenants = vi.fn();
const listDomains = vi.fn();
const getDnsWizardStatus = vi.fn();

vi.mock("../../api/admin", async () => {
  const actual =
    await vi.importActual<typeof import("../../api/admin")>("../../api/admin");
  return {
    ...actual,
    listTenants: (...args: unknown[]) => listTenants(...args),
    listDomains: (...args: unknown[]) => listDomains(...args),
    getDnsWizardStatus: (...args: unknown[]) => getDnsWizardStatus(...args),
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

const domain: TenantDomain = {
  id: "dom-1",
  tenant_id: "tenant-1",
  domain: "acme.test",
  verified: false,
  mx_verified: false,
  spf_verified: false,
  dkim_verified: false,
  dmarc_verified: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function statusFixture(allVerified: boolean): DnsWizardStatus {
  return {
    allVerified,
    steps: [
      {
        key: "mx",
        label: "MX records",
        verified: allVerified,
        record: {
          type: "MX",
          name: "acme.test",
          value: "10 mx.kmail.test",
          ttl: 3600,
          priority: 10,
        },
      },
      {
        key: "spf",
        label: "SPF (TXT)",
        verified: allVerified,
        record: {
          type: "TXT",
          name: "acme.test",
          value: "v=spf1 include:kmail.test -all",
        },
      },
    ],
  };
}

afterEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
});

async function selectTenantAndDomain(): Promise<void> {
  listTenants.mockResolvedValue([tenant]);
  listDomains.mockResolvedValue([domain]);
  render(<DnsWizard />);

  // Tenant select is populated once listTenants resolves.
  const tenantSelect = await screen.findByRole("combobox", { name: /tenant/i });
  await waitFor(() =>
    expect(within(tenantSelect).getByRole("option", { name: "Acme" })).toBeInTheDocument(),
  );
  await userEvent.selectOptions(tenantSelect, "tenant-1");

  const domainSelect = await screen.findByRole("combobox", { name: /domain/i });
  await waitFor(() =>
    expect(
      within(domainSelect).getByRole("option", { name: "acme.test" }),
    ).toBeInTheDocument(),
  );
  await userEvent.selectOptions(domainSelect, "dom-1");
}

describe("<DnsWizard />", () => {
  it("loads the wizard steps and shows the active step's record", async () => {
    getDnsWizardStatus.mockResolvedValue(statusFixture(false));
    await selectTenantAndDomain();

    // First step (MX) is active and renders its record value.
    expect(
      await screen.findByRole("heading", { name: /Step 1 \/ 2: MX records/ }),
    ).toBeInTheDocument();
    expect(screen.getByText("10 mx.kmail.test")).toBeInTheDocument();
    expect(getDnsWizardStatus).toHaveBeenCalledWith("tenant-1", "dom-1");
  });

  it("lists outstanding (unverified) records while not all verified", async () => {
    getDnsWizardStatus.mockResolvedValue(statusFixture(false));
    await selectTenantAndDomain();

    const outstanding = await screen.findByRole("heading", {
      name: "Outstanding records",
    });
    expect(outstanding).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "All records verified" })).toBeNull();
  });

  it("navigates to the next step with the Next control", async () => {
    getDnsWizardStatus.mockResolvedValue(statusFixture(false));
    await selectTenantAndDomain();
    await screen.findByRole("heading", { name: /Step 1 \/ 2: MX records/ });

    await userEvent.click(screen.getByRole("button", { name: /Next/ }));
    expect(
      screen.getByRole("heading", { name: /Step 2 \/ 2: SPF \(TXT\)/ }),
    ).toBeInTheDocument();
  });

  it("re-fetches status on Verify and reflects a fully-verified domain", async () => {
    getDnsWizardStatus
      .mockResolvedValueOnce(statusFixture(false))
      .mockResolvedValueOnce(statusFixture(true));
    await selectTenantAndDomain();
    await screen.findByRole("heading", { name: "Outstanding records" });

    fireEvent.click(screen.getByRole("button", { name: /Verify all records/ }));

    expect(
      await screen.findByRole("heading", { name: "All records verified" }),
    ).toBeInTheDocument();
    // Two calls: the initial load + the explicit Verify.
    await waitFor(() => expect(getDnsWizardStatus).toHaveBeenCalledTimes(2));
  });
});
