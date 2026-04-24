package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics wires the Prometheus collectors for the BFF. Collectors
// are registered with a dedicated registry so tests can construct
// multiple copies without the default registry panic on duplicate
// registration.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	JMAPProxyDuration   prometheus.Histogram
	ActiveTenants       prometheus.Gauge
	SeatsTotal          *prometheus.GaugeVec
}

// NewMetrics builds a Metrics collector set and registers it with a
// fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kmail_http_requests_total",
				Help: "Total HTTP requests handled by the BFF, labeled by method, path, and status.",
			},
			[]string{"method", "path", "status"},
		),
		HTTPRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kmail_http_request_duration_seconds",
				Help:    "HTTP request duration in seconds, by method and path.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "path"},
		),
		JMAPProxyDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "kmail_jmap_proxy_duration_seconds",
				Help:    "JMAP upstream proxy duration in seconds.",
				Buckets: prometheus.DefBuckets,
			},
		),
		ActiveTenants: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "kmail_active_tenants",
				Help: "Number of tenants currently in status=active.",
			},
		),
		SeatsTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "kmail_seats_total",
				Help: "Total paid seats across all tenants, labeled by plan.",
			},
			[]string{"plan"},
		),
	}
	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.JMAPProxyDuration,
		m.ActiveTenants,
		m.SeatsTotal,
	)
	return m
}

// Handler returns the `/metrics` HTTP handler. Mount it
// unauthenticated on the BFF so Prometheus can scrape.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// pathLabelLimit caps the cardinality of the `path` label on the
// per-request metrics — arbitrary path values blow up Prometheus.
const pathLabelLimit = 128

var pathLabelPool = sync.Pool{
	New: func() any { return make([]byte, 0, pathLabelLimit) },
}

// Middleware returns an http middleware that records
// `kmail_http_requests_total` and `kmail_http_request_duration_seconds`
// for every request. The `path` label uses `r.URL.Path` (no
// templating) so callers should route through Go's `http.ServeMux`
// (which already collapses `/api/v1/tenants/{id}` into the matched
// pattern) before hitting this middleware.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		elapsed := time.Since(start).Seconds()
		path := truncateLabel(r.URL.Path)
		status := strconv.Itoa(sr.status)
		m.HTTPRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		m.HTTPRequestDuration.WithLabelValues(r.Method, path).Observe(elapsed)
	})
}

func truncateLabel(v string) string {
	if len(v) <= pathLabelLimit {
		return v
	}
	return v[:pathLabelLimit]
}
