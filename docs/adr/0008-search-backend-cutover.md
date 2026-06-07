# 0008 — A pluggable search backend with Meilisearch→OpenSearch cutover

- **Status**: Accepted
- **Related**: `internal/search/` (`opensearch.go`), [`../../deploy/prometheus/prometheus.yml`](../../deploy/prometheus/prometheus.yml) (`kmail_search_cutover_*`)

## Context

KMail needs full-text mailbox search. Meilisearch is excellent for a
fast start (small, simple to operate, great for the dev compose stack
and early scale). At larger scale, operators often standardise on
OpenSearch/Elasticsearch for sharding, ops tooling, and ecosystem fit.
Committing irreversibly to one engine would be a mistake.

## Decision

Define a **`SearchBackend` interface** with multiple implementations —
Meilisearch (default/early) and OpenSearch
(`internal/search/opensearch.go`, which wraps OpenSearch's REST API and
can run behind an `opensearch-proxy` sidecar for request signing). Index
migration from Meilisearch to OpenSearch is handled by a **search
cutover worker** (a `kmail-worker` job) that backfills and switches the
active backend online, exporting `kmail_search_cutover_*` metrics.

## Consequences

- The search engine is an operational choice, not an architectural
  lock-in; deployments can run Meilisearch or OpenSearch.
- The search index is **derived data** — rebuildable from mail — so it
  is not backed up; recovery means reindexing (see
  [backup & restore](../operator/backup-restore.md#derived-stores-no-restore-needed)).
- Cutover health must be watched (`KmailSearchCutoverFailing` alert) so
  a stalled migration is caught (see
  [monitoring](../operator/monitoring.md#alert-rules)).
- Two backends mean two code paths to keep behind the interface — the
  cost of avoiding a one-way door.
