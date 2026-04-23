import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// See docs/ARCHITECTURE.md §8 for the client protocol topology.
// The dev server proxies `/jmap` to the local Go BFF so the React
// client speaks to exactly the same endpoint in development as in
// production.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/jmap": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      "/.well-known/jmap": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: true,
  },
});
