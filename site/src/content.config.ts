import { defineCollection, z } from "astro:content";
import { glob } from "astro/loaders";

/**
 * Blog posts authored directly in the site repo.
 */
const blog = defineCollection({
  loader: glob({ pattern: "**/*.md", base: "./src/content/blog" }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    pubDate: z.coerce.date(),
    author: z.string().default("The KMail team"),
    tags: z.array(z.string()).default([]),
  }),
});

/**
 * Help center articles. The canonical markdown lives in
 * `docs/public/` (so it ships with the repo and is reviewable
 * outside the site build); `scripts/sync-content.mjs` mirrors it
 * into `src/content/help/` before every build. Each article's
 * directory name is its category (e.g. `getting-started/dns-setup`).
 */
const help = defineCollection({
  loader: glob({ pattern: "**/*.md", base: "./src/content/help" }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    category: z.string(),
    order: z.number().default(100),
    updated: z.coerce.date().optional(),
  }),
});

export const collections = { blog, help };
