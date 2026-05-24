import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// See docs/ARCHITECTURE.md §8 for the client protocol topology.
// The dev server proxies `/jmap` to the local Go BFF so the React
// client speaks to exactly the same endpoint in development as in
// production.
//
// Port note: the BFF defaults to `:8088` (see
// `internal/config/config.go` → `KMAIL_API_ADDR`) because host
// port 8080 is already taken by Stalwart in docker-compose. If
// you override the BFF address in your shell, update both target
// URLs below to match.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/jmap": {
        target: "http://localhost:8088",
        changeOrigin: true,
      },
      "/.well-known/jmap": {
        target: "http://localhost:8088",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: true,
  },
});
