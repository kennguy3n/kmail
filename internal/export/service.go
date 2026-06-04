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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/middleware"
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

// Result is what a Runner returns after materialising an export
// archive. The Service persists these fields onto the export_jobs
// row (artifact columns + download_url) and records MessageIDs in
// the export_job_messages join table.
type Result struct {
	// DownloadURL is a short-lived presigned GET for the archive.
	DownloadURL string
	// ArtifactURL is the canonical, stable reference to the stored
	// archive (re-presignable by the admin UI).
	ArtifactURL string
	// ArtifactSizeBytes is the size of the packaged archive.
	ArtifactSizeBytes int64
	// ArtifactChecksum is the lowercase hex SHA-256 of the archive.
	ArtifactChecksum string
	// MessageIDs are the account-qualified IDs included in the
	// archive, recorded for audit / legal-hold reproducibility.
	MessageIDs []string
}

// Runner materialises the archive for one job and returns its
// Result. Wired in main.go; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, job Job) (Result, error)
}

// AuditLogger is the subset of *audit.Service the export service
// writes to when a job changes state. Optional (nil disables audit
// emission).
type AuditLogger interface {
	Log(ctx context.Context, e audit.Entry) (*audit.Entry, error)
}

// Service manages export jobs.
type Service struct {
	pool   *pgxpool.Pool
	runner Runner
	audit  AuditLogger
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

// WithAuditLogger wires the audit-log writer. Pass nil to disable.
func (s *Service) WithAuditLogger(a AuditLogger) *Service {
	s.audit = a
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
		// pgx.ErrNoRows == empty queue, sleep until next tick.
		// Any other error (pool exhaustion, context cancelled,
		// transient network blip, syntax bug) MUST be surfaced so
		// the worker logs it instead of silently stalling.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next job: %w", err)
	}
	return &j, nil
}

// markComplete records the runner result: the artifact columns +
// download_url on the export_jobs row and one export_job_messages
// row per included message. All writes happen in a single
// tenant-scoped transaction so the RLS policies on both tables
// (which require app.tenant_id) are satisfied and the job + its
// message manifest commit atomically.
func (s *Service) markComplete(ctx context.Context, job Job, res Result) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, job.TenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE export_jobs
			SET status = 'completed', download_url = $2, completed_at = now(),
			    artifact_url = $3, artifact_size_bytes = $4, artifact_checksum = $5
			WHERE id = $1::uuid AND tenant_id = $6::uuid
		`, job.ID, res.DownloadURL, res.ArtifactURL, res.ArtifactSizeBytes, res.ArtifactChecksum, job.TenantID)
		if err != nil {
			return fmt.Errorf("update export_jobs: %w", err)
		}
		for _, mid := range res.MessageIDs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO export_job_messages (job_id, tenant_id, message_id)
				VALUES ($1::uuid, $2::uuid, $3)
				ON CONFLICT (job_id, message_id) DO NOTHING
			`, job.ID, job.TenantID, mid); err != nil {
				return fmt.Errorf("insert export_job_messages: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.logAudit(ctx, job, "export.completed", map[string]any{
		"format":              job.Format,
		"scope":               job.Scope,
		"artifact_size_bytes": res.ArtifactSizeBytes,
		"artifact_checksum":   res.ArtifactChecksum,
		"message_count":       len(res.MessageIDs),
	})
	return nil
}

func (s *Service) markFailed(ctx context.Context, job Job, runErr error) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, job.TenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE export_jobs SET status = 'failed', error_message = $2, completed_at = now()
			WHERE id = $1::uuid AND tenant_id = $3::uuid
		`, job.ID, runErr.Error(), job.TenantID)
		return err
	})
	if err != nil {
		return err
	}
	s.logAudit(ctx, job, "export.failed", map[string]any{
		"format": job.Format,
		"scope":  job.Scope,
		"error":  runErr.Error(),
	})
	return nil
}

// logAudit appends a tenant-scoped audit entry for an export state
// change. Best-effort: a logging failure must not fail the export,
// so the error is swallowed (the audit chain is tamper-evident, not
// transactional with the job).
func (s *Service) logAudit(ctx context.Context, job Job, action string, meta map[string]any) {
	if s.audit == nil || job.TenantID == "" {
		return
	}
	actorID := job.RequesterID
	if actorID == "" {
		actorID = "export-worker"
	}
	_, _ = s.audit.Log(ctx, audit.Entry{
		TenantID:     job.TenantID,
		ActorID:      actorID,
		ActorType:    audit.ActorSystem,
		Action:       action,
		ResourceType: "export_job",
		ResourceID:   job.ID,
		Metadata:     meta,
	})
}

// RunExport executes the runner for a job and persists the outcome.
// It returns the Result (zero on failure) plus any error so the
// worker can update metrics and logs.
func (s *Service) RunExport(ctx context.Context, job Job) (Result, error) {
	if s.runner == nil {
		err := errors.New("export: no runner registered")
		if mErr := s.markFailed(ctx, job, err); mErr != nil {
			return Result{}, fmt.Errorf("%w (and mark-failed: %v)", err, mErr)
		}
		return Result{}, err
	}
	res, err := s.runner.Run(ctx, job)
	if err != nil {
		if mErr := s.markFailed(ctx, job, err); mErr != nil {
			return Result{}, fmt.Errorf("%w (and mark-failed: %v)", err, mErr)
		}
		return Result{}, err
	}
	if err := s.markComplete(ctx, job, res); err != nil {
		return Result{}, err
	}
	return res, nil
}
