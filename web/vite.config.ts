import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

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
  plugins: [react(), tailwindcss()],
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
    rollupOptions: {
      output: {
        // Route pages are already code-split via React.lazy (see
        // src/App.tsx), which is the bulk of the win. These manual
        // groups keep the shared dependencies that every route pulls
        // in out of the per-route chunks and in a stable, long-cached
        // vendor file:
        //   - react-vendor: the React runtime + router, imported by
        //     every chunk, so a content hash only churns on a React
        //     upgrade.
        //   - editor: the TipTap/ProseMirror rich-text stack, which is
        //     large and only reached from the compose/signature/
        //     template routes, so it stays a lazily-loaded chunk.
        manualChunks(id) {
          if (!id.includes("node_modules")) return undefined;
          if (
            id.includes("/react/") ||
            id.includes("/react-dom/") ||
            id.includes("/react-router") ||
            id.includes("/scheduler/")
          ) {
            return "react-vendor";
          }
          if (id.includes("@tiptap") || id.includes("prosemirror")) {
            return "editor";
          }
          return undefined;
        },
      },
    },
  },
});
