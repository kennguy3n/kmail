import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

// Flat-config ESLint setup for the KMail React frontend. We use the
// non-type-checked `typescript-eslint` recommended preset (no
// `parserOptions.project`) so a lint run stays fast and does not need
// a successful `tsc` build first — type errors are already gated by
// `npm run build` (`tsc --noEmit && vite build`). ESLint's job here
// is the correctness layer `tsc` does not cover: React hook rules,
// unused-disable directives, and the common JS footguns.
//
// `@typescript-eslint/no-unused-vars` is configured to ignore
// `_`-prefixed bindings, mirroring how `tsconfig.json`'s
// `noUnusedLocals`/`noUnusedParameters` already treat them — the
// codebase uses `_`-prefixed names for the deliberately-unused half
// of a destructure (the "omit a key" idiom) and for unused callback
// params, so the two checkers must agree or `tsc` and `eslint`
// disagree on the same line.
const unusedVars = [
  "error",
  {
    argsIgnorePattern: "^_",
    varsIgnorePattern: "^_",
    caughtErrorsIgnorePattern: "^_",
  },
];

export default tseslint.config(
  // Not source: build output, the MSW-generated worker (checked in so
  // tests can run offline), and Playwright's report/result artifacts.
  {
    ignores: [
      "dist",
      "public/mockServiceWorker.js",
      "playwright-report",
      "test-results",
      "blob-report",
    ],
  },

  // Application + library source. Browser globals plus React hook
  // rules (rules-of-hooks + exhaustive-deps catch real render bugs).
  //
  // `react-refresh/only-export-components` is intentionally OFF: it is
  // a Vite Fast Refresh dev-server nicety (a module mixing component
  // and non-component exports falls back to a full reload during
  // `vite dev`), not a correctness rule. This codebase deliberately
  // co-locates a hook with its provider (`ToastProvider`/`useToast`),
  // a helper with its component (`Avatar`/`initialsFromName`), and a
  // set of message-rendering helpers next to `HtmlMessageBody`. Those
  // exports are imported widely, so enforcing the rule would mean an
  // architectural refactor for zero runtime benefit. The plugin stays
  // registered so the rule can be re-enabled per-file later if any
  // module is split out.
  {
    files: ["src/**/*.{ts,tsx}"],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: "module",
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": "off",
      "@typescript-eslint/no-unused-vars": unusedVars,
    },
  },

  // Test, setup, and MSW mock files run under Vitest (`globals: true`
  // in vitest.config.ts) in a jsdom environment, so they see both the
  // browser globals and the Vitest test globals.
  {
    files: [
      "src/**/*.test.{ts,tsx}",
      "src/test/**/*.{ts,tsx}",
      "src/mocks/**/*.{ts,tsx}",
    ],
    languageOptions: {
      globals: {
        ...globals.browser,
        ...globals.node,
        ...globals.vitest,
      },
    },
  },

  // Node-side tooling: vite/vitest/tailwind/playwright configs at the
  // package root and the Playwright e2e specs (run by the Node test
  // runner, not the browser bundle).
  {
    files: ["*.{ts,js}", "e2e/**/*.ts"],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      sourceType: "module",
      globals: globals.node,
    },
    rules: {
      "@typescript-eslint/no-unused-vars": unusedVars,
    },
  },
);
