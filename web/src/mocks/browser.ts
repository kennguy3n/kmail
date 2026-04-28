/**
 * MSW browser worker for screenshot capture / local demos.
 *
 * Activated when Vite is started with `VITE_MOCK_API=true` (see
 * `web/src/main.tsx`). The worker intercepts `/jmap/*` and
 * `/api/v1/*` calls so the React UI can render with realistic
 * sample data without the Go BFF running. Bypasses every other
 * request (HMR, static assets, fonts, etc.).
 */
import { setupWorker } from "msw/browser";

import { handlers } from "./handlers";

export const worker = setupWorker(...handlers);
