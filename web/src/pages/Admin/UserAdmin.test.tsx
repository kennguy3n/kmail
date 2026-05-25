/**
 * Component tests for UserAdmin.tsx focused on the inline alias
 * manager. The user list / edit / delete surface is exercised by
 * the lower-level admin client tests; this file pins the
 * load-bearing alias contract end-to-end:
 *
 *   - Expand "Aliases" → lists the user's aliases.
 *   - Add alias form posts to the tenant scope.
 *   - Delete confirmation gates the destructive action behind a
 *     second click.
 *   - 409 surfaces from `createAlias` show up in the row error
 *     slot instead of crashing.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import UserAdmin from "./UserAdmin";

const listTenants = vi.fn();
const listUsers = vi.fn();
const listUserAliases = vi.fn();
const createAlias = vi.fn();
const deleteAlias = vi.fn();

vi.mock("../../api/admin", async () => {
  // Pull in the real module so we keep the `AdminApiError`
  // class and `ADMIN_API_BASE` constant — the component imports
  // them as a value, not a type.
  const actual =
    await vi.importActual<typeof import("../../api/admin")>("../../api/admin");
  return {
    ...actual,
    listTenants: (...args: unknown[]) => listTenants(...args),
    listUsers: (...args: unknown[]) => listUsers(...args),
    listUserAliases: (...args: unknown[]) => listUserAliases(...args),
    createAlias: (...args: unknown[]) => createAlias(...args),
    deleteAlias: (...args: unknown[]) => deleteAlias(...args),
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

const user = {
  id: "user-1",
  tenant_id: "tenant-1",
  kchat_user_id: "k-1",
  stalwart_account_id: "sa-1",
  email: "alice@acme.com",
  display_name: "Alice",
  role: "member",
  status: "active",
  account_type: "user",
  quota_bytes: 0,
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

const alias = {
  id: "alias-1",
  tenant_id: "tenant-1",
  user_id: "user-1",
  alias_email: "alice.alt@acme.com",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

afterEach(() => {
  vi.clearAllMocks();
  // useTenantSelection persists the picker via localStorage; reset
  // between tests so each one boots into the same initial state.
  if (typeof window !== "undefined") {
    window.localStorage.clear();
  }
});

async function renderAndExpandAliases(): Promise<void> {
  listTenants.mockResolvedValueOnce([tenant]);
  listUsers.mockResolvedValueOnce([user]);
  render(<UserAdmin />);
  expect(await screen.findByText("Alice")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Aliases" }));
  await waitFor(() => {
    expect(listUserAliases).toHaveBeenCalledWith("tenant-1", "user-1");
  });
}

describe("<UserAdmin /> alias manager", () => {
  it("lists existing aliases when the manager is expanded", async () => {
    listUserAliases.mockResolvedValueOnce([alias]);
    await renderAndExpandAliases();

    expect(await screen.findByText("alice.alt@acme.com")).toBeInTheDocument();
  });

  it("posts a new alias and renders it on success", async () => {
    listUserAliases.mockResolvedValueOnce([]);
    const created = { ...alias, alias_email: "alice.new@acme.com" };
    createAlias.mockResolvedValueOnce(created);

    await renderAndExpandAliases();

    fireEvent.change(screen.getByLabelText(/Add alias/i), {
      target: { value: "alice.new@acme.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    await waitFor(() => {
      expect(createAlias).toHaveBeenCalledWith("tenant-1", {
        user_id: "user-1",
        alias_email: "alice.new@acme.com",
      });
    });
    expect(await screen.findByText("alice.new@acme.com")).toBeInTheDocument();
  });

  it("requires a confirm click before deleting an alias", async () => {
    listUserAliases.mockResolvedValueOnce([alias]);
    deleteAlias.mockResolvedValueOnce(undefined);

    await renderAndExpandAliases();

    // Scope to the alias list so the alias "Delete" button is
    // distinguishable from the user-row "Delete" button.
    const aliasRow = (
      await screen.findByText("alice.alt@acme.com")
    ).closest("li") as HTMLElement;
    expect(aliasRow).not.toBeNull();
    const aliasScope = within(aliasRow);

    // First click arms the confirm; the API must not fire yet.
    fireEvent.click(aliasScope.getByRole("button", { name: "Delete" }));
    expect(deleteAlias).not.toHaveBeenCalled();

    // Second click on the now-visible "Confirm delete" actually
    // fires the DELETE and removes the row.
    fireEvent.click(
      aliasScope.getByRole("button", { name: "Confirm delete" }),
    );
    await waitFor(() => {
      expect(deleteAlias).toHaveBeenCalledWith("tenant-1", "alias-1");
    });
    await waitFor(() => {
      expect(screen.queryByText("alice.alt@acme.com")).not.toBeInTheDocument();
    });
  });

  it("surfaces a 409 from createAlias in the manager error slot", async () => {
    listUserAliases.mockResolvedValueOnce([]);
    createAlias.mockRejectedValueOnce(new Error("alias email already in use"));

    await renderAndExpandAliases();

    fireEvent.change(screen.getByLabelText(/Add alias/i), {
      target: { value: "dup@acme.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    expect(await screen.findByText(/already in use/)).toBeInTheDocument();
  });
});
