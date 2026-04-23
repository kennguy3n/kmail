# KMail — React Frontend

**License**: Proprietary — All Rights Reserved. See [../LICENSE](../LICENSE).

KMail's React frontend lives here. This is the KChat-embedded Mail
+ Calendar UI. All traffic flows through the Go BFF; the client
never talks to Stalwart directly.

## Stack

- React 18 + TypeScript.
- [Vite](https://vitejs.dev/) build + dev server.
- React Router for client-side routing.

## Structure

```
src/
  main.tsx                  application entrypoint
  App.tsx                   router configuration
  components/
    Layout.tsx              shared shell
  pages/
    Mail/                   inbox, compose, message view
    Calendar/               calendar view, event create
    Admin/                  tenant / domain / user admin
  api/
    jmap.ts                 JMAP client against the BFF
  types/
    index.ts                shared TypeScript types
```

## Scripts

```sh
npm install     # install dependencies
npm run dev     # start Vite dev server on :5173
npm run build   # typecheck and build production bundle
npm run preview # preview the production bundle locally
npm run lint    # lint with eslint (config TBD)
```

## Contracts

- [`../docs/JMAP-CONTRACT.md`](../docs/JMAP-CONTRACT.md) — the JMAP
  surface the BFF exposes. Every network call from the client is a
  JMAP call against that contract.
- [`../docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md) — system
  topology.
- [`../docs/PROPOSAL.md`](../docs/PROPOSAL.md) — product and
  technical design.

## Dev server

The Vite config proxies `/jmap` and `/.well-known/jmap` to
`http://localhost:8080` (the local `kmail-api`). Adjust
`vite.config.ts` if your BFF runs elsewhere.

## Phase 1 status

Every page / API module is a placeholder. The router is wired and
the TypeScript build is clean, so Phase 2 engineers can start
landing real views without restructuring the tree.
