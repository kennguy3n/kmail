package export

import (
	"context"
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

// Worker is the export job runner pool.
type Worker struct {
	svc          *Service
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
			job, err := w.svc.claimNextJob(ctx)
			if err != nil {
				w.logger.Printf("export.worker: claim: %v", err)
				continue
			}
			if job == nil {
				continue
			}
			sem <- struct{}{}
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
