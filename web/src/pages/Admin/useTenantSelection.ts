import { useEffect, useState } from "react";

import { AdminApiError, listTenants, type Tenant } from "../../api/admin";

/**
 * Storage key for the currently-selected tenant. Persisted across
 * reloads so flipping between `/admin/domains` and `/admin/users`
 * does not reset the selection.
 */
const STORAGE_KEY = "kmail.admin.selectedTenantId";

function readStoredTenantId(): string | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

function writeStoredTenantId(id: string | null): void {
  if (typeof window === "undefined") return;
  try {
    if (id === null) {
      window.localStorage.removeItem(STORAGE_KEY);
    } else {
      window.localStorage.setItem(STORAGE_KEY, id);
    }
  } catch {
    // storage unavailable (private browsing, quota) — no-op.
  }
}

export interface TenantSelection {
  tenants: Tenant[] | null;
  selectedTenantId: string | null;
  selectedTenant: Tenant | null;
  isLoading: boolean;
  error: string | null;
  selectTenant: (id: string) => void;
  reload: () => void;
}

/**
 * Hook that loads the tenant list and tracks the currently-selected
 * tenant. Shared by `DomainAdmin` and `UserAdmin` so both pages
 * agree on which tenant they are scoped to. The selection survives
 * reloads via `localStorage` but falls back to the first tenant in
 * the list if the stored ID is stale.
 *
 * The hook does *not* render a picker; each page chooses how to
 * surface the selection. Keeping the picker out of this module lets
 * each admin page position the control to match its layout.
 */
export function useTenantSelection(): TenantSelection {
  const [tenants, setTenants] = useState<Tenant[] | null>(null);
  const [selectedTenantId, setSelectedTenantId] = useState<string | null>(
    () => readStoredTenantId(),
  );
  const [isLoading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Incrementing `reloadNonce` triggers the effect below to
  // re-fetch the tenant list. Callers drive it via `reload()`.
  const [reloadNonce, setReloadNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    listTenants()
      .then((list) => {
        if (cancelled) return;
        setTenants(list);
        setSelectedTenantId((current) => {
          if (current && list.some((t) => t.id === current)) {
            return current;
          }
          const next = list[0]?.id ?? null;
          writeStoredTenantId(next);
          return next;
        });
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        const msg =
          e instanceof AdminApiError
            ? e.message
            : e instanceof Error
              ? e.message
              : String(e);
        setError(msg);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [reloadNonce]);

  const selectedTenant =
    tenants?.find((t) => t.id === selectedTenantId) ?? null;

  const selectTenant = (id: string): void => {
    setSelectedTenantId(id);
    writeStoredTenantId(id);
  };

  const reload = (): void => {
    setReloadNonce((n) => n + 1);
  };

  return {
    tenants,
    selectedTenantId,
    selectedTenant,
    isLoading,
    error,
    selectTenant,
    reload,
  };
}
