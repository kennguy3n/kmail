package retention

import (
	"context"
	"log"
	"time"
)

// Worker ticks daily and evaluates retention for every active
// tenant. Pattern matches `billing.QuotaWorker` /
// `tenant.HealthWorker`.
type Worker struct {
	svc      *Service
	logger   *log.Logger
	interval time.Duration
}

// NewWorker constructs a Worker.
func NewWorker(svc *Service, logger *log.Logger) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{svc: svc, logger: logger, interval: 24 * time.Hour}
}

// WithInterval is a test-only override.
func (w *Worker) WithInterval(d time.Duration) *Worker {
	w.interval = d
	return w
}

// Run loops until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.svc == nil {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.tick(ctx); err != nil {
				w.logger.Printf("retention.worker: %v", err)
			}
		}
	}
}

func (w *Worker) tick(ctx context.Context) error {
	tenants, err := w.svc.ListActiveTenants(ctx)
	if err != nil {
		return err
	}
	for _, id := range tenants {
		if _, err := w.svc.EvaluateRetention(ctx, id); err != nil {
			w.logger.Printf("retention.worker: tenant %s: %v", id, err)
		}
	}
	return nil
}
