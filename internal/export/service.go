// Package export — Phase 5 tenant data export / eDiscovery prep.
//
// The Phase 5 cut delivers job CRUD + a worker stub. The actual
// JMAP-side fetch / packaging / blob download lives behind the
// `Runner` callback so the export package does not pull `jmap`,
// `caldav`, and `audit` as dependencies. main.go wires the runner
// in production; tests inject a fake.
//
// The packaged archive is uploaded back into the tenant's
// dedicated zk-object-fabric bucket (provisioned by
// `internal/tenant/zkfabric.go`) with a 7-day presigned download
// URL. Storing the archive in the tenant's own bucket keeps the
// data on the tenant's chosen placement region.
package export

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Job is the public export-job shape.
type Job struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	RequesterID  string     `json:"requester_id"`
	Format       string     `json:"format"`
	Scope        string     `json:"scope"`
	ScopeRef     string     `json:"scope_ref,omitempty"`
	Status       string     `json:"status"`
	DownloadURL  string     `json:"download_url,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// Runner is the per-job callback that produces the archive and
// returns the download URL. Wired in main.go.
type Runner func(ctx context.Context, job Job) (downloadURL string, err error)

// Service manages export jobs.
type Service struct {
	pool   *pgxpool.Pool
	runner Runner
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// WithRunner sets the runner that materializes archives.
func (s *Service) WithRunner(r Runner) *Service {
	s.runner = r
	return s
}

// CreateExportJob inserts a new pending job.
func (s *Service) CreateExportJob(ctx context.Context, tenantID, requesterID, format, scope, scopeRef string) (*Job, error) {
	if tenantID == "" || requesterID == "" {
		return nil, errors.New("export: tenant + requester required")
	}
	if format == "" {
		format = "mbox"
	}
	if scope == "" {
		scope = "all"
	}
	if s.pool == nil {
		return nil, errors.New("export: pool not configured")
	}
	var j Job
	err := s.pool.QueryRow(ctx, `
		INSERT INTO export_jobs (tenant_id, requester_id, format, scope, scope_ref, status)
		VALUES ($1::uuid, $2, $3, $4, $5, 'pending')
		RETURNING id::text, tenant_id::text, requester_id, format, scope, scope_ref, status,
		          download_url, error_message, created_at, started_at, completed_at
	`, tenantID, requesterID, format, scope, scopeRef).Scan(
		&j.ID, &j.TenantID, &j.RequesterID, &j.Format, &j.Scope, &j.ScopeRef, &j.Status,
		&j.DownloadURL, &j.ErrorMessage, &j.CreatedAt, &j.StartedAt, &j.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// GetExportJob returns one job by id.
func (s *Service) GetExportJob(ctx context.Context, tenantID, id string) (*Job, error) {
	if s.pool == nil {
		return nil, errors.New("export: pool not configured")
	}
	var j Job
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, requester_id, format, scope, scope_ref, status,
		       download_url, error_message, created_at, started_at, completed_at
		FROM export_jobs WHERE id = $1::uuid AND tenant_id = $2::uuid
	`, id, tenantID).Scan(
		&j.ID, &j.TenantID, &j.RequesterID, &j.Format, &j.Scope, &j.ScopeRef, &j.Status,
		&j.DownloadURL, &j.ErrorMessage, &j.CreatedAt, &j.StartedAt, &j.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// ListExportJobs lists recent jobs for a tenant.
func (s *Service) ListExportJobs(ctx context.Context, tenantID string) ([]Job, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, tenant_id::text, requester_id, format, scope, scope_ref, status,
		       download_url, error_message, created_at, started_at, completed_at
		FROM export_jobs WHERE tenant_id = $1::uuid
		ORDER BY created_at DESC LIMIT 100
	`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(
			&j.ID, &j.TenantID, &j.RequesterID, &j.Format, &j.Scope, &j.ScopeRef, &j.Status,
			&j.DownloadURL, &j.ErrorMessage, &j.CreatedAt, &j.StartedAt, &j.CompletedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// claimNextJob marks the oldest pending job as running and returns
// it. Returns nil, nil when the queue is empty.
func (s *Service) claimNextJob(ctx context.Context) (*Job, error) {
	if s.pool == nil {
		return nil, nil
	}
	var j Job
	err := s.pool.QueryRow(ctx, `
		UPDATE export_jobs SET status = 'running', started_at = now()
		WHERE id = (
			SELECT id FROM export_jobs WHERE status = 'pending'
			ORDER BY created_at ASC FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING id::text, tenant_id::text, requester_id, format, scope, scope_ref, status,
		          download_url, error_message, created_at, started_at, completed_at
	`).Scan(
		&j.ID, &j.TenantID, &j.RequesterID, &j.Format, &j.Scope, &j.ScopeRef, &j.Status,
		&j.DownloadURL, &j.ErrorMessage, &j.CreatedAt, &j.StartedAt, &j.CompletedAt,
	)
	if err != nil {
		// no rows == empty queue
		return nil, nil
	}
	return &j, nil
}

// markComplete records the runner result.
func (s *Service) markComplete(ctx context.Context, id string, downloadURL string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE export_jobs SET status = 'completed', download_url = $2, completed_at = now()
		WHERE id = $1::uuid
	`, id, downloadURL)
	return err
}

func (s *Service) markFailed(ctx context.Context, id string, runErr error) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE export_jobs SET status = 'failed', error_message = $2, completed_at = now()
		WHERE id = $1::uuid
	`, id, runErr.Error())
	return err
}

// RunExport executes the runner for a job. Useful for tests / for
// the worker tick.
func (s *Service) RunExport(ctx context.Context, job Job) error {
	if s.runner == nil {
		return s.markFailed(ctx, job.ID, fmt.Errorf("no runner registered"))
	}
	url, err := s.runner(ctx, job)
	if err != nil {
		return s.markFailed(ctx, job.ID, err)
	}
	return s.markComplete(ctx, job.ID, url)
}
