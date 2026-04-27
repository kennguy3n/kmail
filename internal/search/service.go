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

// Backend names recognised by the service. Stored verbatim in
// `tenants.search_backend`.
const (
	BackendMeilisearch = "meilisearch"
	BackendOpenSearch  = "opensearch"
)

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
}

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
// the column is NULL or empty we default to BackendMeilisearch so
// existing tenants keep working without a migration backfill.
func (s *Service) GetBackend(ctx context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return BackendMeilisearch, nil
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
		backend = BackendMeilisearch
	}
	return backend, nil
}

// SetBackend updates the tenant's search backend. Validates the
// name against the registered backends so a typo can't put a
// tenant into an unreachable state.
func (s *Service) SetBackend(ctx context.Context, tenantID, backend string) error {
	if tenantID == "" || backend == "" {
		return fmt.Errorf("%w: tenantID and backend required", ErrInvalidInput)
	}
	switch backend {
	case BackendMeilisearch, BackendOpenSearch:
	default:
		return fmt.Errorf("%w: backend must be %q or %q", ErrInvalidInput, BackendMeilisearch, BackendOpenSearch)
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

// Reindex re-imports every message in `msgs` into whatever backend
// the tenant is currently configured for. Phase 7 calls this
// after a SetBackend that flipped the column; in production the
// orchestrator pulls messages out of Stalwart directly.
func (s *Service) Reindex(ctx context.Context, tenantID string, msgs []Message) error {
	name, err := s.GetBackend(ctx, tenantID)
	if err != nil {
		return err
	}
	b, ok := s.backends[name]
	if !ok {
		return fmt.Errorf("%w: backend %q not configured", ErrNotFound, name)
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

// indexNameFor returns the per-tenant index identifier. Both
// backends use the same shape so a tenant migrating between them
// can keep their existing index identifier.
func indexNameFor(tenantID string) string {
	clean := strings.ReplaceAll(tenantID, "-", "")
	return "kmail_" + clean
}

// queryEscape is exported through `url.QueryEscape` but pulled
// into the package so the backend drivers depend on a single
// helper.
var queryEscape = url.QueryEscape
