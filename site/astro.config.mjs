// @ts-check
import { defineConfig } from "astro/config";
import sitemap from "@astrojs/sitemap";

// The public marketing/docs site is a fully static build. It is
// published by the existing release pipeline (see site/README.md);
// nothing here deploys to the internet at build time.
//
// `site` is the canonical production origin used to generate
// absolute URLs in the sitemap and the incident RSS/Atom feed.
// Override with the `PUBLIC_SITE_URL` env var in CI if the
// deployment origin differs.
const SITE_URL = process.env.PUBLIC_SITE_URL ?? "https://kmail.kchat.dev";

export default defineConfig({
  site: SITE_URL,
  output: "static",
  trailingSlash: "ignore",
  integrations: [sitemap()],
  build: {
    // Emit `/help/dns-setup/index.html` style files so the static
    // host serves clean URLs without a server-side router.
    format: "directory",
  },
});
