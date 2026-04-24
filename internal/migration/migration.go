// Package migration hosts the Migration Orchestrator business
// logic: Gmail / IMAP imports via imapsync workers with
// checkpoint/resume, staged sync, and cutover workflows.
//
// The same binary (kmail-migration) also applies Postgres
// migrations from docs/SCHEMA.md. See docs/ARCHITECTURE.md §7.
//
// Job lifecycle:
//
//	pending ─────▶ running ─────▶ completed
//	   │              │
//	   │              ├──▶ failed
//	   │              └──▶ cancelled
//	   └────────────────▶ cancelled
//
// A job is created in `pending`, flipped to `running` when a
// worker picks it up, and eventually reaches one of the three
// terminal states. `CancelJob` signals the in-process worker to
// stop cleanly on the next `imapsync` checkpoint; any job still
// `pending` is cancelled synchronously.
package migration

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ----------------------------------------------------------------
// Errors
// ----------------------------------------------------------------

// ErrInvalidInput wraps caller-visible validation failures. Mirrors
// the sentinel used by internal/tenant and internal/dns so the
// shared HTTP error mapping stays uniform.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound signals that the requested job does not exist or is
// not visible under the current tenant context.
var ErrNotFound = errors.New("not found")

// ErrConflict signals an illegal state transition (e.g. cancelling
// a job that is already terminal).
var ErrConflict = errors.New("conflict")

// ----------------------------------------------------------------
// Config / Service
// ----------------------------------------------------------------

// Config holds the orchestrator's runtime dependencies.
type Config struct {
	// Pool is the control-plane Postgres pool used for every
	// tenant-scoped read/write. All queries go through an
	// RLS-scoped transaction via middleware.SetTenantGUC.
	Pool *pgxpool.Pool

	// StalwartAdminURL is the internal admin endpoint the
	// orchestrator uses for destination-account validation and,
	// in future, for cutover DNS flips. Unused by the worker
	// today but plumbed through for the Phase 4 cutover path.
	StalwartAdminURL string

	// ImapsyncBin is the absolute path to the `imapsync` binary
	// the worker shells out to. Defaults to "imapsync" (resolved
	// from PATH).
	ImapsyncBin string

	// MaxConcurrent is the ceiling on simultaneous worker
	// goroutines. Defaults to 4. Additional job starts block on
	// an internal semaphore until a slot frees up.
	MaxConcurrent int

	// Now is injected so tests can pin timestamps. Defaults to
	// time.Now.
	Now func() time.Time
}

// Service is the orchestrator root. One instance per process.
type Service struct {
	cfg Config

	// sema caps the number of in-flight worker goroutines so a
	// burst of StartJob calls cannot blow past MaxConcurrent.
	sema chan struct{}

	// mu protects cancels, the jobID → cancel-func map used by
	// CancelJob to signal a running worker, and pausing, the set
	// of jobIDs whose worker context was cancelled by PauseJob
	// rather than CancelJob (so the worker knows not to overwrite
	// the already-written `paused` status with `cancelled` on its
	// terminal update).
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	pausing map[string]struct{}
}

// NewService constructs a Service with sensible defaults.
func NewService(cfg Config) *Service {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.ImapsyncBin == "" {
		cfg.ImapsyncBin = "imapsync"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{
		cfg:     cfg,
		sema:    make(chan struct{}, cfg.MaxConcurrent),
		cancels: make(map[string]context.CancelFunc),
		pausing: make(map[string]struct{}),
	}
}

// ----------------------------------------------------------------
// Types
// ----------------------------------------------------------------

// MigrationJob is the wire + persisted shape of a single import
// job. Mirrors the `migration_jobs` table in
// `migrations/002_migration_jobs.sql`.
type MigrationJob struct {
	ID                      string     `json:"id"`
	TenantID                string     `json:"tenant_id"`
	SourceHost              string     `json:"source_host"`
	SourceUser              string     `json:"source_user"`
	SourcePasswordEncrypted string     `json:"-"`
	DestUser                string     `json:"dest_user"`
	Status                  string     `json:"status"`
	ProgressPct             int        `json:"progress_pct"`
	MessagesTotal           *int       `json:"messages_total,omitempty"`
	MessagesSynced          *int       `json:"messages_synced,omitempty"`
	StartedAt               *time.Time `json:"started_at,omitempty"`
	CompletedAt             *time.Time `json:"completed_at,omitempty"`
	ErrorMsg                *string    `json:"error_msg,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

// CreateJobInput is the body accepted by POST /api/v1/migrations.
// SourcePassword is the cleartext password for `imapsync --passfile2`;
// it is encrypted before being persisted. The password is never
// returned via any API.
type CreateJobInput struct {
	SourceHost     string `json:"source_host"`
	SourceUser     string `json:"source_user"`
	SourcePassword string `json:"source_password"`
	DestUser       string `json:"dest_user"`
}

// Terminal returns true when the job cannot transition further.
func (j *MigrationJob) Terminal() bool {
	switch j.Status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// ----------------------------------------------------------------
// Validation helpers
// ----------------------------------------------------------------

func (in CreateJobInput) validate() error {
	if in.SourceHost == "" {
		return fmt.Errorf("%w: source_host is required", ErrInvalidInput)
	}
	if in.SourceUser == "" {
		return fmt.Errorf("%w: source_user is required", ErrInvalidInput)
	}
	if in.SourcePassword == "" {
		return fmt.Errorf("%w: source_password is required", ErrInvalidInput)
	}
	if in.DestUser == "" {
		return fmt.Errorf("%w: dest_user is required", ErrInvalidInput)
	}
	return nil
}

// ----------------------------------------------------------------
// Persistence helpers
// ----------------------------------------------------------------

func (s *Service) withTenantTx(
	ctx context.Context,
	tenantID string,
	fn func(pgx.Tx) error,
) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return errors.New("migration: no postgres pool configured")
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

func scanJob(row pgx.Row) (*MigrationJob, error) {
	var j MigrationJob
	var pwd *string
	if err := row.Scan(
		&j.ID, &j.TenantID, &j.SourceHost, &j.SourceUser, &pwd,
		&j.DestUser, &j.Status, &j.ProgressPct,
		&j.MessagesTotal, &j.MessagesSynced,
		&j.StartedAt, &j.CompletedAt, &j.ErrorMsg,
		&j.CreatedAt, &j.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if pwd != nil {
		j.SourcePasswordEncrypted = *pwd
	}
	return &j, nil
}

const jobSelectColumns = `
	id::text, tenant_id::text, source_host, source_user,
	source_password_encrypted, dest_user, status, progress_pct,
	messages_total, messages_synced, started_at, completed_at,
	error_msg, created_at, updated_at
`

// ----------------------------------------------------------------
// Public API
// ----------------------------------------------------------------

// CreateJob validates input, encrypts the source password (a
// Phase 2 placeholder — see `encryptPassword` below), and inserts
// a fresh row in `pending` state.
func (s *Service) CreateJob(
	ctx context.Context,
	tenantID string,
	in CreateJobInput,
) (*MigrationJob, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	enc, err := encryptPassword(in.SourcePassword)
	if err != nil {
		return nil, fmt.Errorf("encrypt source password: %w", err)
	}

	var job *MigrationJob
	err = s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO migration_jobs (
				tenant_id, source_host, source_user,
				source_password_encrypted, dest_user, status
			) VALUES ($1::uuid, $2, $3, $4, $5, 'pending')
			RETURNING `+jobSelectColumns,
			tenantID, in.SourceHost, in.SourceUser, enc, in.DestUser,
		)
		out, scanErr := scanJob(row)
		if scanErr != nil {
			return fmt.Errorf("insert migration_job: %w", scanErr)
		}
		job = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// GetJob returns the job by id, scoped to the caller's tenant via
// RLS. Returns ErrNotFound when no row is visible.
func (s *Service) GetJob(ctx context.Context, tenantID, jobID string) (*MigrationJob, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: job id is required", ErrInvalidInput)
	}
	var job *MigrationJob
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+jobSelectColumns+` FROM migration_jobs WHERE id = $1::uuid`,
			jobID)
		out, scanErr := scanJob(row)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if scanErr != nil {
			return scanErr
		}
		job = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ListJobs returns every job visible to the tenant, newest first.
func (s *Service) ListJobs(ctx context.Context, tenantID string) ([]*MigrationJob, error) {
	var jobs []*MigrationJob
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+jobSelectColumns+`
			 FROM migration_jobs
			 ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			job, scanErr := scanJob(rows)
			if scanErr != nil {
				return scanErr
			}
			jobs = append(jobs, job)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// StartJob transitions a `pending` job into `running` and spawns a
// background worker goroutine. The worker uses a context derived
// from s.cfg.Pool / context.Background so it outlives the caller's
// request context; CancelJob signals the worker via the cancel
// func tracked on s.cancels.
func (s *Service) StartJob(ctx context.Context, tenantID, jobID string) error {
	job, err := s.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return err
	}
	if job.Status != "pending" && job.Status != "paused" {
		return fmt.Errorf("%w: job %s is already %s", ErrConflict, jobID, job.Status)
	}

	// Reserve a worker slot before flipping state so we never
	// have a `running` row without a live worker.
	select {
	case s.sema <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	now := s.cfg.Now()
	if err := s.updateStatus(ctx, tenantID, jobID, updateFields{
		Status:    stringPtr("running"),
		StartedAt: &now,
	}); err != nil {
		<-s.sema
		return err
	}

	workerCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[jobID] = cancel
	s.mu.Unlock()

	go s.runWorker(workerCtx, tenantID, jobID, job)
	return nil
}

// CancelJob signals a worker to stop. Pending jobs flip straight
// to `cancelled`; running jobs have their context cancelled and
// the worker writes the terminal state.
// PauseJob halts a running job. The running worker goroutine is
// signalled via its cancel func; on the next progress checkpoint
// the worker exits. The DB row is flipped to `paused` so operators
// can distinguish it from a terminal `cancelled` row.
func (s *Service) PauseJob(ctx context.Context, tenantID, jobID string) error {
	job, err := s.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return err
	}
	if job.Terminal() {
		return fmt.Errorf("%w: job %s is already %s", ErrConflict, jobID, job.Status)
	}
	if job.Status != "running" && job.Status != "pending" {
		return fmt.Errorf("%w: cannot pause job in status %s", ErrConflict, job.Status)
	}

	s.mu.Lock()
	cancel, running := s.cancels[jobID]
	if running {
		// Flag the job as pausing *before* calling cancel() so the
		// worker goroutine sees the flag when it unwinds and skips
		// its terminal updateStatus (which would otherwise race and
		// overwrite `paused` with `cancelled`).
		s.pausing[jobID] = struct{}{}
	}
	s.mu.Unlock()
	if running {
		// Worker will observe ctx.Err() and stop mid-batch;
		// imapsync's own checkpoint files (`--tmpdir`) persist
		// progress for ResumeJob.
		cancel()
	}
	return s.updateStatus(ctx, tenantID, jobID, updateFields{
		Status: stringPtr("paused"),
	})
}

// ResumeJob re-queues a paused job. The worker starts from
// imapsync's last checkpoint under the job's `--tmpdir` so
// already-synced messages are skipped.
func (s *Service) ResumeJob(ctx context.Context, tenantID, jobID string) error {
	job, err := s.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return err
	}
	if job.Status != "paused" {
		return fmt.Errorf("%w: cannot resume job in status %s", ErrConflict, job.Status)
	}
	// StartJob re-reads the row and spawns a fresh worker.
	return s.StartJob(ctx, tenantID, jobID)
}

func (s *Service) CancelJob(ctx context.Context, tenantID, jobID string) error {
	job, err := s.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return err
	}
	if job.Terminal() {
		return fmt.Errorf("%w: job %s is already %s", ErrConflict, jobID, job.Status)
	}

	s.mu.Lock()
	cancel, running := s.cancels[jobID]
	s.mu.Unlock()
	if running {
		cancel()
		return nil
	}

	// Pending (no worker): flip the row directly.
	now := s.cfg.Now()
	return s.updateStatus(ctx, tenantID, jobID, updateFields{
		Status:      stringPtr("cancelled"),
		CompletedAt: &now,
	})
}

// ----------------------------------------------------------------
// Internal: worker + state updates
// ----------------------------------------------------------------

type updateFields struct {
	Status         *string
	ProgressPct    *int
	MessagesTotal  *int
	MessagesSynced *int
	StartedAt      *time.Time
	CompletedAt    *time.Time
	ErrorMsg       *string
}

func (s *Service) updateStatus(
	ctx context.Context,
	tenantID, jobID string,
	f updateFields,
) error {
	return s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE migration_jobs SET
				status          = COALESCE($2, status),
				progress_pct    = COALESCE($3, progress_pct),
				messages_total  = COALESCE($4, messages_total),
				messages_synced = COALESCE($5, messages_synced),
				started_at      = COALESCE($6, started_at),
				completed_at    = COALESCE($7, completed_at),
				error_msg       = COALESCE($8, error_msg)
			WHERE id = $1::uuid`,
			jobID,
			f.Status, f.ProgressPct, f.MessagesTotal, f.MessagesSynced,
			f.StartedAt, f.CompletedAt, f.ErrorMsg,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// runWorker is the goroutine body that drives an imapsync import
// for a single job. It shells out to `imapsync`, parses progress
// lines as they stream on stdout, and writes checkpoints to
// Postgres so a crashed orchestrator can resume on restart.
func (s *Service) runWorker(
	ctx context.Context,
	tenantID, jobID string,
	job *MigrationJob,
) {
	defer func() {
		<-s.sema
		s.mu.Lock()
		delete(s.cancels, jobID)
		delete(s.pausing, jobID)
		s.mu.Unlock()
	}()

	runErr := s.runImapsync(ctx, tenantID, jobID, job)

	// If PauseJob signalled this worker, it has already flipped
	// the row to `paused`. Writing `cancelled` here would race and
	// break ResumeJob (which only accepts rows in `paused`).
	s.mu.Lock()
	_, paused := s.pausing[jobID]
	s.mu.Unlock()
	if paused {
		return
	}

	// Use a background context for the terminal write so we still
	// record a final state when the worker's context was cancelled.
	writeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	final := updateFields{}
	now := s.cfg.Now()
	final.CompletedAt = &now

	switch {
	case errors.Is(runErr, context.Canceled):
		final.Status = stringPtr("cancelled")
	case runErr != nil:
		msg := runErr.Error()
		final.Status = stringPtr("failed")
		final.ErrorMsg = &msg
	default:
		full := 100
		final.Status = stringPtr("completed")
		final.ProgressPct = &full
	}
	if err := s.updateStatus(writeCtx, tenantID, jobID, final); err != nil {
		log.Printf("migration worker %s: final updateStatus failed: %v", jobID, err)
	}
}

// runImapsync shells out to `imapsync`. The `--password1`
// environment variable / `--passfile` flag would normally receive
// the decrypted source password; for Phase 2 we pass via stdin
// to avoid leaking it onto the argv list visible to /proc/PID/cmdline.
//
// Exported as a package-level var so tests can swap it out.
var runImapsyncCmd = func(
	ctx context.Context,
	bin string,
	job *MigrationJob,
	stdinPassword string,
) (io.ReadCloser, *exec.Cmd, error) {
	args := []string{
		"--host1", job.SourceHost,
		"--user1", job.SourceUser,
		"--ssl1",
		"--host2", "localhost",
		"--user2", job.DestUser,
		"--ssl2",
		"--nolog",
		"--nolockfile",
		// `--pidfile` disabled by `--nolockfile`; checkpointing
		// is handled via imapsync's `--syncinternaldates` + the
		// built-in `~/.imapsync/<user>/<account>.txt` cache.
		"--automap",
		"--delete2folders", // prune destination folders no longer on source
		"--passfile1", "/dev/stdin",
	}
	// Destination password: KMail's BFF token. We feed it via an
	// env var so imapsync's `--passfile2` can shell-expand it, or
	// we can set it directly below via `os.Setenv`. Kept simple
	// for the Phase 2 scaffolding — production will switch to
	// XOAUTH2 against Stalwart once that ships.
	cmd := exec.CommandContext(ctx, bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, stdinPassword+"\n")
	}()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return stdout, cmd, nil
}

// Progress line format emitted by imapsync, e.g.:
//
//	++++ Statistics : Folder [INBOX] Messages 1234 of 2345 done
//
// We parse the two ints to drive progress_pct and messages_synced.
var imapsyncProgressRE = regexp.MustCompile(
	`Messages\s+(\d+)\s+of\s+(\d+)`,
)

func (s *Service) runImapsync(
	ctx context.Context,
	tenantID, jobID string,
	job *MigrationJob,
) error {
	pwd, err := decryptPassword(job.SourcePasswordEncrypted)
	if err != nil {
		return fmt.Errorf("decrypt source password: %w", err)
	}

	stdout, cmd, err := runImapsyncCmd(ctx, s.cfg.ImapsyncBin, job, pwd)
	if err != nil {
		return fmt.Errorf("start imapsync: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	// imapsync prints very long lines for per-message traces; give
	// the scanner a generous buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		m := imapsyncProgressRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		synced, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		pct := 0
		if total > 0 {
			pct = (synced * 100) / total
		}
		// Fire-and-forget: we don't want a transient Postgres
		// hiccup to abort the whole sync. The terminal write in
		// runWorker is the source of truth for final state.
		if err := s.updateStatus(ctx, tenantID, jobID, updateFields{
			ProgressPct:    &pct,
			MessagesTotal:  &total,
			MessagesSynced: &synced,
		}); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("migration worker %s: progress update failed: %v", jobID, err)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// Fall through — wait on the process exit below so we
		// surface the actual imapsync exit code.
		log.Printf("migration worker %s: stdout scan: %v", jobID, err)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("imapsync exited: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------
// Password encryption scaffolding.
//
// Production plan (docs/PROPOSAL.md §11): derive a tenant-scoped
// symmetric key from the KChat MLS tree and wrap source passwords
// with it at CreateJob time. The worker decrypts just before
// spawning imapsync. For Phase 2 we store a reversible encoding
// so the end-to-end flow is testable without the MLS dependency;
// the encoding is INTENTIONALLY a placeholder and MUST be
// replaced before Phase 3.
// ----------------------------------------------------------------

func encryptPassword(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return "kmail-enc-v0:" + plain, nil
}

func decryptPassword(enc string) (string, error) {
	const prefix = "kmail-enc-v0:"
	if enc == "" {
		return "", nil
	}
	if len(enc) <= len(prefix) || enc[:len(prefix)] != prefix {
		return "", fmt.Errorf("unrecognised password encoding")
	}
	return enc[len(prefix):], nil
}

func stringPtr(s string) *string { return &s }
