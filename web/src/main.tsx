import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import App from "./App";
import { ToastProvider } from "./components/ToastProvider";
import { initTheme } from "./hooks/useTheme";

// Global design-system stylesheet (tokens + themes + reset). Imported
// once here so every component can read the CSS custom properties.
import "./styles/global.css";

// Resolve and apply the persisted/system theme before first paint so
// there is no flash of the wrong colour scheme.
initTheme();

/**
 * When `VITE_MOCK_API=true` is set, lazy-load MSW and start the
 * browser worker before mounting React. Used by
 * `scripts/capture-screenshots.mjs` so the demo screenshot routes
 * render with realistic sample data instead of "Failed to fetch"
 * banners. The dynamic import keeps `msw` (and the generated
 * `mockServiceWorker.js`) out of every production bundle.
 */
async function prepare(): Promise<void> {
  if (import.meta.env.VITE_MOCK_API === "true") {
    try {
      const { worker, resetMockState } = await import("./mocks/browser");
      await worker.start({ onUnhandledRequest: "bypass" });
      // Escape hatch for long-lived manual mock sessions: clear the
      // in-memory stores (notes, vault folders, migration jobs) created
      // while poking around, without a hard reload. Mock builds only.
      (window as Window & { __resetMockState?: () => void }).__resetMockState =
        resetMockState;
    } catch (err) {
      // Surface the failure but still render the app — a broken
      // MSW boot must not produce a blank page.
      console.error("KMail: MSW worker failed to start", err);
    }
  }
}

const rootElement = document.getElementById("root");
if (!rootElement) {
  throw new Error("KMail: #root element not found");
}

void prepare().finally(() => {
  ReactDOM.createRoot(rootElement).render(
    <React.StrictMode>
      <BrowserRouter>
        <ToastProvider>
          <App />
        </ToastProvider>
      </BrowserRouter>
    </React.StrictMode>,
  );
});
