package main

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// supervisorMaxBackoff caps the exponential restart backoff so a
// worker that keeps exiting (e.g. an unreachable downstream) is
// retried at most once per interval rather than hot-looping.
const supervisorMaxBackoff = 30 * time.Second

// workerMetrics are the Prometheus collectors describing the
// worker fleet's supervision state. They are the kmail-worker
// binary's own domain metrics — distinct from the per-worker
// business metrics packages register on the shared registry.
type workerMetrics struct {
	registered prometheus.Gauge
	up         *prometheus.GaugeVec
	restarts   *prometheus.CounterVec
	panics     *prometheus.CounterVec
}

func newWorkerMetrics(reg prometheus.Registerer) *workerMetrics {
	m := &workerMetrics{
		registered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kmail_worker_registered",
			Help: "Number of background workers registered in this kmail-worker process.",
		}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kmail_worker_up",
			Help: "1 while a named background worker is being supervised, 0 once it has stopped.",
		}, []string{"worker"}),
		restarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kmail_worker_restarts_total",
			Help: "Total times a background worker's Run loop returned before shutdown and was restarted.",
		}, []string{"worker"}),
		panics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kmail_worker_panics_total",
			Help: "Total panics recovered from a background worker's Run loop.",
		}, []string{"worker"}),
	}
	reg.MustRegister(m.registered, m.up, m.restarts, m.panics)
	return m
}

// supervisor runs each registered worker in its own goroutine,
// recovers panics, and restarts a worker that returns before its
// context is cancelled (with capped exponential backoff). A worker
// process should be resilient: a single worker crashing must not
// take down the other twelve, and a transient downstream outage
// should not permanently disable a worker.
type supervisor struct {
	metrics *workerMetrics
	logger  *log.Logger
	wg      sync.WaitGroup
	// baseBackoff is the first restart delay; it doubles up to
	// supervisorMaxBackoff on each successive early return. Exposed
	// as a field (rather than a constant) so tests can drive the
	// restart path without real-time sleeps.
	baseBackoff time.Duration
}

func newSupervisor(metrics *workerMetrics, logger *log.Logger) *supervisor {
	return &supervisor{metrics: metrics, logger: logger, baseBackoff: time.Second}
}

// start launches reg under supervision. It returns immediately; the
// worker runs until ctx is cancelled.
func (s *supervisor) start(ctx context.Context, reg workerRegistration) {
	s.wg.Add(1)
	go s.run(ctx, reg)
}

func (s *supervisor) run(ctx context.Context, reg workerRegistration) {
	defer s.wg.Done()
	s.metrics.up.WithLabelValues(reg.name).Set(1)
	defer s.metrics.up.WithLabelValues(reg.name).Set(0)

	backoff := s.baseBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		s.runOnce(ctx, reg)
		if ctx.Err() != nil {
			// Context cancelled: this is a normal shutdown, not a
			// crash. Do not count it as a restart.
			return
		}
		// Unexpected early return (or a recovered panic): restart
		// after a capped backoff so a flapping downstream doesn't
		// turn into a hot loop.
		s.metrics.restarts.WithLabelValues(reg.name).Inc()
		s.logger.Printf("worker %q returned before shutdown; restarting in %s", reg.name, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > supervisorMaxBackoff {
			backoff = supervisorMaxBackoff
		}
	}
}

// runOnce executes a single invocation of the worker's Run loop with
// panic recovery so one worker's panic is isolated from the rest.
func (s *supervisor) runOnce(ctx context.Context, reg workerRegistration) {
	defer func() {
		if rec := recover(); rec != nil {
			s.metrics.panics.WithLabelValues(reg.name).Inc()
			s.logger.Printf("worker %q panicked: %v\n%s", reg.name, rec, debug.Stack())
		}
	}()
	reg.run(ctx)
}

// wait blocks until every supervised worker has returned or the
// timeout elapses. It reports true when all workers drained in time.
func (s *supervisor) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
