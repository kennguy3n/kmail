/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** When `"true"`, MSW boots before React renders (see main.tsx). */
  readonly VITE_MOCK_API?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
