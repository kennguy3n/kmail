package export

import (
	"context"
	"log"
	"time"
)

// Worker is the export job runner pool.
type Worker struct {
	svc      *Service
	logger   *log.Logger
	interval time.Duration
	parallel int
}

// NewWorker constructs a Worker.
func NewWorker(svc *Service, logger *log.Logger) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{svc: svc, logger: logger, interval: 30 * time.Second, parallel: 2}
}

// WithInterval is a test override.
func (w *Worker) WithInterval(d time.Duration) *Worker { w.interval = d; return w }

// WithParallel is a test override.
func (w *Worker) WithParallel(n int) *Worker { w.parallel = n; return w }

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
				if err := w.svc.RunExport(ctx, j); err != nil {
					w.logger.Printf("export.worker: run %s: %v", j.ID, err)
				}
			}(*job)
		}
	}
}
