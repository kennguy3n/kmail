package export

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the Prometheus metric set for the export worker.
// Exposed so callers register the collectors with the same registry
// the BFF serves on `/metrics`.
type Metrics struct {
	JobsCompleted prometheus.Counter
	JobsFailed    prometheus.Counter
	BytesTotal    prometheus.Counter
}

// NewMetrics builds the export metric set and registers it with
// `reg`. Pass nil to skip registration (tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		JobsCompleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_export_jobs_completed_total",
			Help: "Total export jobs that completed successfully.",
		}),
		JobsFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_export_jobs_failed_total",
			Help: "Total export jobs that failed.",
		}),
		BytesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_export_bytes_total",
			Help: "Total bytes of export archives produced (sum of artifact sizes).",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.JobsCompleted, m.JobsFailed, m.BytesTotal)
	}
	return m
}

// defaultStaleTimeout is how long a job may sit in 'running' before
// the worker assumes the claimer died (or failed to record a
// terminal state) and requeues it. It must exceed the longest
// realistic export runtime so an in-flight job is never run twice.
const defaultStaleTimeout = 60 * time.Minute

// jobSource is the slice of *Service the worker drives. Declaring it
// as an interface lets tests inject a fake to assert the worker's
// scheduling and metric-accounting contracts (e.g. that a run the
// service abandoned via errJobNotCurrent is counted as neither
// completed nor failed) without standing up Postgres. *Service
// satisfies it in production.
type jobSource interface {
	RequeueStaleJobs(ctx context.Context, olderThan time.Duration) (int64, error)
	claimNextJob(ctx context.Context) (*Job, error)
	RunExport(ctx context.Context, job Job) (Result, error)
}

// Worker is the export job runner pool.
type Worker struct {
	svc          jobSource
	logger       *log.Logger
	interval     time.Duration
	parallel     int
	metrics      *Metrics
	staleTimeout time.Duration
}

// NewWorker constructs a Worker.
func NewWorker(svc *Service, logger *log.Logger) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{svc: svc, logger: logger, interval: 30 * time.Second, parallel: 2, staleTimeout: defaultStaleTimeout}
}

// WithInterval is a test override.
func (w *Worker) WithInterval(d time.Duration) *Worker { w.interval = d; return w }

// WithParallel is a test override.
func (w *Worker) WithParallel(n int) *Worker { w.parallel = n; return w }

// WithMetrics wires a Prometheus metric set. Pass nil to disable.
func (w *Worker) WithMetrics(m *Metrics) *Worker { w.metrics = m; return w }

// WithStaleTimeout overrides how long a job may stay 'running'
// before it is requeued. A non-positive value disables the sweep.
func (w *Worker) WithStaleTimeout(d time.Duration) *Worker { w.staleTimeout = d; return w }

// Run loops until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.svc == nil {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	sem := make(chan struct{}, w.parallel)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Backstop: recover jobs orphaned in 'running' (claimer
			// died, or a transient DB error blocked the terminal
			// state write) before claiming fresh work.
			if w.staleTimeout > 0 {
				if n, err := w.svc.RequeueStaleJobs(ctx, w.staleTimeout); err != nil {
					w.logger.Printf("export.worker: requeue stale: %v", err)
				} else if n > 0 {
					w.logger.Printf("export.worker: requeued %d stale running job(s)", n)
				}
			}
			// Reserve a worker slot *before* claiming. This keeps two
			// invariants: (1) a claimed ('running') job is never held
			// idle in a local var waiting for a slot to free, and (2)
			// cancellation stays responsive — if both slots are busy with
			// long-running exports we block here on a select that also
			// watches ctx.Done(), instead of blocking on a bare channel
			// send that ignores shutdown.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			job, err := w.svc.claimNextJob(ctx)
			if err != nil {
				<-sem
				w.logger.Printf("export.worker: claim: %v", err)
				continue
			}
			if job == nil {
				<-sem
				continue
			}
			go func(j Job) {
				defer func() { <-sem }()
				w.runOne(ctx, j)
			}(*job)
		}
	}
}

// runOne executes a single job and records metrics for the outcome.
func (w *Worker) runOne(ctx context.Context, j Job) {
	res, err := w.svc.RunExport(ctx, j)
	if err != nil {
		if errors.Is(err, errJobNotCurrent) {
			// The job was requeued by the stale-timeout sweep (and may
			// already be re-claimed) while this run was in flight, so its
			// outcome was intentionally discarded. The authoritative run
			// is the retry — don't count this one as completed or failed.
			w.logger.Printf("export.worker: job %s requeued mid-run; outcome discarded", j.ID)
			return
		}
		w.logger.Printf("export.worker: run %s: %v", j.ID, err)
		if w.metrics != nil {
			w.metrics.JobsFailed.Inc()
		}
		return
	}
	if w.metrics != nil {
		w.metrics.JobsCompleted.Inc()
		w.metrics.BytesTotal.Add(float64(res.ArtifactSizeBytes))
	}
}
