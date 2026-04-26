package adminproxy

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/kmail/internal/audit"
)

// ExpiryWorker walks `admin_access_sessions` once a minute and
// emits a `session_expired` audit entry for any row whose
// `expires_at` has passed without an explicit revoke. The row's
// `expired_at` column is set so the worker never duplicates the
// audit entry on the next tick.
type ExpiryWorker struct {
	pool     *pgxpool.Pool
	audit    *audit.Service
	logger   *log.Logger
	interval time.Duration
	metric   prometheus.Counter
}

// NewExpiryWorker constructs an ExpiryWorker. Defaults to 60s.
func NewExpiryWorker(pool *pgxpool.Pool, auditSvc *audit.Service, logger *log.Logger) *ExpiryWorker {
	if logger == nil {
		logger = log.Default()
	}
	return &ExpiryWorker{pool: pool, audit: auditSvc, logger: logger, interval: 60 * time.Second}
}

// WithInterval is a test-only override.
func (w *ExpiryWorker) WithInterval(d time.Duration) *ExpiryWorker {
	w.interval = d
	return w
}

// WithMetric wires a Prometheus counter that increments once per
// expired session detected.
func (w *ExpiryWorker) WithMetric(reg prometheus.Registerer) *ExpiryWorker {
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kmail_admin_sessions_expired_total",
		Help: "Number of admin proxy sessions auto-expired by the watcher.",
	})
	if reg != nil {
		reg.MustRegister(c)
	}
	w.metric = c
	return w
}

// Run loops until ctx is cancelled.
func (w *ExpiryWorker) Run(ctx context.Context) {
	if w == nil || w.pool == nil {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := w.tick(ctx); err != nil {
				w.logger.Printf("adminproxy.expiry: %v", err)
			} else if n > 0 {
				w.logger.Printf("adminproxy.expiry: marked %d sessions expired", n)
			}
		}
	}
}

// tick claims any newly-expired sessions and emits audit rows.
// Exposed for tests.
func (w *ExpiryWorker) tick(ctx context.Context) (int, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT id::text, tenant_id::text, admin_user_id, scope, started_at, expires_at
		FROM admin_access_sessions
		WHERE revoked_at IS NULL
		  AND expired_at IS NULL
		  AND expires_at < now()
		ORDER BY expires_at
		LIMIT 100
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type expired struct {
		id, tenantID, adminUser, scope string
		startedAt, expiresAt           time.Time
	}
	var batch []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.tenantID, &e.adminUser, &e.scope, &e.startedAt, &e.expiresAt); err != nil {
			return 0, err
		}
		batch = append(batch, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	processed := 0
	for _, e := range batch {
		if err := w.processOne(ctx, e.id, e.tenantID, e.adminUser, e.scope, e.startedAt, e.expiresAt); err != nil {
			w.logger.Printf("adminproxy.expiry: session %s: %v", e.id, err)
			continue
		}
		processed++
		if w.metric != nil {
			w.metric.Inc()
		}
	}
	return processed, nil
}

func (w *ExpiryWorker) processOne(ctx context.Context, id, tenantID, adminUser, scope string, startedAt, expiresAt time.Time) error {
	return pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		// Atomically mark the row expired so concurrent workers /
		// retries can't double-emit. Skip if someone else got it
		// (rows == 0).
		ct, err := tx.Exec(ctx, `
			UPDATE admin_access_sessions
			SET expired_at = now()
			WHERE id = $1::uuid AND expired_at IS NULL AND revoked_at IS NULL
		`, id)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return nil
		}
		if w.audit != nil {
			_, err = w.audit.Log(ctx, audit.Entry{
				TenantID:     tenantID,
				ActorID:      adminUser,
				ActorType:    audit.ActorSystem,
				Action:       "session_expired",
				ResourceType: "admin_access_session",
				ResourceID:   id,
				Metadata: map[string]any{
					"scope":      scope,
					"started_at": startedAt.UTC().Format(time.RFC3339),
					"expires_at": expiresAt.UTC().Format(time.RFC3339),
				},
			})
		}
		return err
	})
}
