// Package search hosts the KMail search abstraction. Stalwart
// owns the actual mailbox indexing — the BFF only manages
// per-tenant backend selection (Meilisearch vs. OpenSearch),
// reindex orchestration, and admin-surface CRUD.
//
// The MVP shipped with an implicit Meilisearch dependency baked
// into Stalwart's `SearchStore` config. Phase 7 adds a
// `SearchBackend` interface so a tenant can opt into OpenSearch
// without bringing the whole fleet along, and so we have a place
// to put the reindex / health surface that's growing in the
// admin console.
//
// Tenants store their selected backend on `tenants.search_backend`
// (migration 039). The Service owns reads/writes against that
// column, plus dispatching to the backend implementation for
// per-message `IndexMessage` / `SearchMessages` / `DeleteIndex` /
// `MigrateIndex` calls.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ErrInvalidInput is returned for caller-visible validation errors.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when a tenant or backend lookup misses.
var ErrNotFound = errors.New("not found")

// ErrBackendUnavailable is returned when a caller asks the
// Service to switch a tenant to a backend whose Go implementation
// is not wired into this BFF process. The name is recognised
// (passes `IsValidBackend`) but `Config.Backends` did not include
// a matching impl. This distinguishes a deployment-time gap
// ("this BFF does not ship `dedicated_opensearch`") from an
// outright typo, so the admin UI can render a clear "not
// available in this deployment" hint without falling through to
// a 500 on every subsequent search call.
var ErrBackendUnavailable = errors.New("backend not available in this deployment")

// Backend names recognised by the service. Stored verbatim in
// `tenants.search_backend`.
//
// The three `shared_*` and `dedicated_*` values were added in
// migration 050 (shared indexes). Older values (`meilisearch`,
// `opensearch`) remain valid for backward compatibility — pre-
// existing tenants stay on whatever they were promoted to under
// the per-tenant-index model until an operator migrates them.
const (
	BackendMeilisearch         = "meilisearch"
	BackendOpenSearch          = "opensearch"
	BackendSharedMeilisearch   = "shared_meilisearch"
	BackendSharedOpenSearch    = "shared_opensearch"
	BackendDedicatedOpenSearch = "dedicated_opensearch"
)

// IsValidBackend reports whether `name` is one of the recognised
// `tenants.search_backend` values. Used by `SetBackend` and by
// the cutover worker before it writes the column so a typo can't
// strand a tenant in an unreachable state.
func IsValidBackend(name string) bool {
	switch name {
	case BackendMeilisearch,
		BackendOpenSearch,
		BackendSharedMeilisearch,
		BackendSharedOpenSearch,
		BackendDedicatedOpenSearch:
		return true
	}
	return false
}

// Message is the per-message indexable shape passed to
// `SearchBackend.IndexMessage`. Stalwart owns the canonical
// message store; the BFF mirrors only the searchable fields.
type Message struct {
	TenantID  string    `json:"tenant_id"`
	MailboxID string    `json:"mailbox_id"`
	MessageID string    `json:"message_id"`
	Subject   string    `json:"subject"`
	Snippet   string    `json:"snippet"`
	From      string    `json:"from"`
	To        []string  `json:"to,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

// SearchHit is one row in a SearchMessages response.
type SearchHit struct {
	MessageID string  `json:"message_id"`
	Subject   string  `json:"subject"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
}

// SearchBackend is the per-backend driver. Implementations live in
// `meilisearch.go` and `opensearch.go`.
type SearchBackend interface {
	// Name returns the backend identifier (e.g. "meilisearch").
	Name() string
	// IndexMessage upserts a single document.
	IndexMessage(ctx context.Context, msg Message) error
	// SearchMessages runs a free-text query against the tenant
	// index and returns at most `limit` hits.
	SearchMessages(ctx context.Context, tenantID, query string, limit int) ([]SearchHit, error)
	// DeleteIndex drops the entire tenant index. Used when a
	// tenant switches backends or churns.
	DeleteIndex(ctx context.Context, tenantID string) error
	// MigrateIndex bulk-imports every message in `msgs` into the
	// tenant index. Used by `Service.Reindex` when switching
	// backends.
	MigrateIndex(ctx context.Context, tenantID string, msgs []Message) error
	// ExportMessages returns every document currently in the
	// tenant index. Used by `Service.Export` during the auto-
	// cutover (Phase 5) so the worker can read messages out of
	// the old backend and push them into the new one. Backends
	// that don't support bulk export return ErrNotSupported.
	ExportMessages(ctx context.Context, tenantID string) ([]Message, error)
}

// ErrNotSupported is returned by SearchBackend implementations
// that can't honour an optional method (e.g. an upstream that has
// no bulk-export API).
var ErrNotSupported = errors.New("backend does not support operation")

// Service manages per-tenant backend selection and reindex jobs.
type Service struct {
	pool     *pgxpool.Pool
	logger   *log.Logger
	backends map[string]SearchBackend
}

// Config wires NewService.
type Config struct {
	Pool     *pgxpool.Pool
	Logger   *log.Logger
	Backends []SearchBackend
}

// NewService builds a Service. If no backends are passed, the
// service operates in metadata-only mode — backend lookups still
// work but Index/Search/Delete/Migrate calls return ErrNotFound
// for the resolved backend.
func NewService(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	bs := map[string]SearchBackend{}
	for _, b := range cfg.Backends {
		if b == nil {
			continue
		}
		bs[b.Name()] = b
	}
	return &Service{pool: cfg.Pool, logger: cfg.Logger, backends: bs}
}

// GetBackend returns the configured backend name for a tenant. If
// the column is NULL or empty we default to BackendSharedMeilisearch
// — that matches the migration-049 column default for newly-
// provisioned tenants and is the cheapest backend to land on if a
// row is somehow missing its setting.
func (s *Service) GetBackend(ctx context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return BackendSharedMeilisearch, nil
	}
	var backend string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `SELECT COALESCE(search_backend, '') FROM tenants WHERE id = $1::uuid`, tenantID)
		return row.Scan(&backend)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get backend: %w", err)
	}
	if backend == "" {
		backend = BackendSharedMeilisearch
	}
	return backend, nil
}

// AvailableBackends returns the names of every backend whose Go
// implementation is wired into this Service, sorted
// alphabetically. Used by the admin UI to render the backend
// selector without exposing values that would silently fail on
// the first search call after a flip.
func (s *Service) AvailableBackends() []string {
	names := make([]string, 0, len(s.backends))
	for name := range s.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsBackendAvailable reports whether a backend with the given
// name is registered on this Service. Distinct from
// `IsValidBackend`: a value can be syntactically valid (e.g.
// `dedicated_opensearch`) but unavailable in this BFF process if
// the deployment did not ship that implementation.
func (s *Service) IsBackendAvailable(name string) bool {
	_, ok := s.backends[name]
	return ok
}

// SetBackend updates the tenant's search backend. Validates the
// name against the recognised values so a typo can't put a
// tenant into an unreachable state, and against the wired
// implementations so the admin UI cannot strand a tenant on a
// backend this BFF does not ship.
func (s *Service) SetBackend(ctx context.Context, tenantID, backend string) error {
	if tenantID == "" || backend == "" {
		return fmt.Errorf("%w: tenantID and backend required", ErrInvalidInput)
	}
	if !IsValidBackend(backend) {
		return fmt.Errorf("%w: backend %q is not a recognised value", ErrInvalidInput, backend)
	}
	// Belt-and-suspenders against the case where migration 050's
	// CHECK constraint admits a value (e.g. `dedicated_opensearch`)
	// that this BFF does not have an implementation for. Without
	// this guard the flip succeeds and every subsequent
	// IndexMessage / SearchMessages call returns the generic
	// `backend not configured` 404. Failing the flip itself with a
	// distinct sentinel lets the admin UI tell the operator
	// exactly why the value is unselectable. Runs BEFORE the pool
	// short-circuit so the same rejection applies in metadata-only
	// mode (e.g. unit tests that build a Service with no pool).
	if !s.IsBackendAvailable(backend) {
		return fmt.Errorf("%w: %q", ErrBackendUnavailable, backend)
	}
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE tenants SET search_backend = $2 WHERE id = $1::uuid`, tenantID, backend)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Export returns every indexed message for `tenantID` from the
// tenant's CURRENT backend. Used by the Phase 5 auto-cutover
// worker as its MessageSource: read from Meilisearch, flip the
// backend column, push into OpenSearch.
func (s *Service) Export(ctx context.Context, tenantID string) ([]Message, error) {
	name, err := s.GetBackend(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	b, ok := s.backends[name]
	if !ok {
		return nil, fmt.Errorf("%w: backend %q not configured", ErrNotFound, name)
	}
	return b.ExportMessages(ctx, tenantID)
}

// Reindex re-imports every message in `msgs` into whatever backend
// the tenant is currently configured for. Phase 7 calls this
// after a SetBackend that flipped the column; in production the
// orchestrator pulls messages out of Stalwart directly.
func (s *Service) Reindex(ctx context.Context, tenantID string, msgs []Message) error {
	name, err := s.GetBackend(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.reindexInto(ctx, tenantID, name, msgs)
}

// ReindexTo bulk-imports `msgs` into a SPECIFIC named backend,
// ignoring the tenant's currently-configured `search_backend`
// column. Used by the Phase 5 auto-cutover worker so it can warm
// the destination index BEFORE flipping the column — if the
// reindex fails, the tenant stays readable on the old backend and
// the worker simply retries on the next tick. Validates `backend`
// against the registered backend names so a typo can't write to
// a non-existent destination.
func (s *Service) ReindexTo(ctx context.Context, tenantID, backend string, msgs []Message) error {
	if tenantID == "" || backend == "" {
		return fmt.Errorf("%w: tenantID and backend required", ErrInvalidInput)
	}
	if !IsValidBackend(backend) {
		return fmt.Errorf("%w: backend %q is not a recognised value", ErrInvalidInput, backend)
	}
	// Same gating as SetBackend: refuse to write into a backend
	// this BFF does not ship, so the cutover worker fails fast
	// with a clear error instead of silently no-op'ing.
	if !s.IsBackendAvailable(backend) {
		return fmt.Errorf("%w: %q", ErrBackendUnavailable, backend)
	}
	return s.reindexInto(ctx, tenantID, backend, msgs)
}

// reindexInto is the shared body for Reindex and ReindexTo. It
// always wipes the destination index first so a retry after a
// half-written run produces a consistent index (no orphan
// documents from the previous attempt).
func (s *Service) reindexInto(ctx context.Context, tenantID, backendName string, msgs []Message) error {
	b, ok := s.backends[backendName]
	if !ok {
		return fmt.Errorf("%w: backend %q not configured", ErrNotFound, backendName)
	}
	if err := b.DeleteIndex(ctx, tenantID); err != nil {
		return fmt.Errorf("reindex: delete: %w", err)
	}
	if len(msgs) == 0 {
		return nil
	}
	return b.MigrateIndex(ctx, tenantID, msgs)
}

// IndexMessage upserts a single message via the tenant's backend.
func (s *Service) IndexMessage(ctx context.Context, msg Message) error {
	name, err := s.GetBackend(ctx, msg.TenantID)
	if err != nil {
		return err
	}
	b, ok := s.backends[name]
	if !ok {
		return fmt.Errorf("%w: backend %q not configured", ErrNotFound, name)
	}
	return b.IndexMessage(ctx, msg)
}

// Search runs a free-text query against the tenant's backend.
func (s *Service) Search(ctx context.Context, tenantID, query string, limit int) ([]SearchHit, error) {
	name, err := s.GetBackend(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	b, ok := s.backends[name]
	if !ok {
		return nil, fmt.Errorf("%w: backend %q not configured", ErrNotFound, name)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return b.SearchMessages(ctx, tenantID, query, limit)
}

// httpJSON is a small helper that POSTs / PUTs / GETs JSON. Both
// backend drivers use it.
func httpJSON(ctx context.Context, client *http.Client, method, endpoint string, headers http.Header, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return err
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %d %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("unmarshal: %w", err)
		}
	}
	return nil
}

// indexNameFor returns the per-tenant index identifier. The
// dedicated (per-tenant) backends use this directly; the shared
// backends derive their index from the tenant's Stalwart shard
// instead (see `sharedIndexNameFor`).
func indexNameFor(tenantID string) string {
	clean := strings.ReplaceAll(tenantID, "-", "")
	return "kmail_" + clean
}

// sharedIndexNameFor returns the shared-index identifier for a
// given Stalwart shard. Every tenant assigned to that shard
// shares this one index; tenant isolation is enforced at query
// time via a `tenant_id` filter (see `SharedMeilisearchBackend`
// and `SharedOpenSearchBackend`).
//
// Shard IDs are UUIDs in production; we strip the hyphens to
// keep the index name compatible with the strictest of
// Meilisearch's identifier rules (alphanumerics, `-`, `_`).
func sharedIndexNameFor(shardID string) string {
	clean := strings.ReplaceAll(shardID, "-", "")
	return "kmail_shared_" + clean
}

// ShardResolver resolves the Stalwart shard ID a tenant is
// assigned to. The shared backends use this to derive their
// index name. Production wires this against
// `tenant.ShardService.GetTenantShardID`; tests inject a fake
// that returns a fixed ID without touching the database.
type ShardResolver interface {
	ShardForTenant(ctx context.Context, tenantID string) (string, error)
}

// ShardResolverFunc adapts a closure into the ShardResolver
// interface for inline wiring in main.go and tests.
type ShardResolverFunc func(ctx context.Context, tenantID string) (string, error)

// ShardForTenant implements ShardResolver.
func (f ShardResolverFunc) ShardForTenant(ctx context.Context, tenantID string) (string, error) {
	return f(ctx, tenantID)
}

// pathEscape is `url.PathEscape` exposed through the package so
// the backend drivers depend on a single helper. Path escaping is
// required (rather than QueryEscape) when the caller is building a
// path segment (`/_doc/{id}`) where a literal space must be
// `%20`, not `+`.
var pathEscape = url.PathEscape

// queryEscape is `url.QueryEscape` exposed through the package
// for the same reason as `pathEscape`. Use this for the VALUE of
// a `?key=value` URL parameter so reserved characters that are
// legal in a path (`=`, `'`, `&`) are encoded — otherwise the
// Meilisearch / OpenSearch URL parser would interpret them as
// structural delimiters and either return 400 or, worse, silently
// truncate the filter expression and run the query without it.
var queryEscape = url.QueryEscape
