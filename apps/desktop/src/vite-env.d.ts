/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_KMAIL_BFF_URL?: string;
  readonly VITE_KMAIL_BEARER_TOKEN?: string;
  readonly VITE_KMAIL_DATABASE_PATH?: string;
  readonly VITE_KMAIL_ACCOUNT_ID?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
