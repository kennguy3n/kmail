// enforcement.go — gap-closure Session 1.
//
// The Enforcer turns a single retention Policy into real mailbox
// mutations against a tenant's Stalwart shard, using the shared
// Session-0 `jmap.EmailOperator` abstraction. It pages through the
// tenant's mail older than the policy cutoff and either destroys it
// (`policy_type = "delete"`) or moves the underlying blobs to a
// cold storage tier before destroying the Stalwart-side index entry
// (`policy_type = "archive"`).
//
// Every run is recorded in `retention_enforcement_log` (RLS-scoped)
// and mirrored into the admin audit chain (`internal/audit`).
// Prometheus counters track destroyed / archived volume. A
// dry-run Enforcer performs every read but skips all destructive
// calls, so operators can validate a policy against live traffic
// before flipping it live.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// Page / batch sizes. The query window pulls up to queryPageSize
// IDs per round-trip (matching the EmailOperator default); destroy
// / archive calls chunk those IDs into destroyChunk-sized requests
// so a single JMAP `Email/set` stays well under Stalwart's limits.
const (
	queryPageSize = 500
	destroyChunk  = 100

	// maxPages bounds the paging loop so a misbehaving operator
	// (e.g. one whose destroy silently fails to remove messages)
	// can never spin forever. 500 IDs/page → 2.5M messages/policy,
	// which is far beyond any realistic single-policy sweep.
	maxPages = 5000
)

// errNoProgress is raised when a live sweep observes the exact same
// batch twice in a row: the destroy/archive call reported success
// but the messages did not leave the cutoff window, so continuing
// would loop forever.
var errNoProgress = errors.New("retention: enforcement made no progress (destroy did not remove messages)")

// ColdMover relocates the storage backing the given account-qualified
// message IDs to a cold tier (zk-object-fabric placement move) ahead
// of an archive-policy destroy. Returns the number of objects moved.
// Implementations batch internally as needed.
type ColdMover interface {
	MoveToCold(ctx context.Context, tenantID string, messageIDs []string) (int, error)
}

// auditLogger is the slice of audit.Service the enforcer needs.
// Narrowed to keep retention decoupled from the rest of the audit
// surface and to allow a fake in tests. *audit.Service satisfies it.
type auditLogger interface {
	Log(ctx context.Context, e audit.Entry) (*audit.Entry, error)
}

// Enforcer applies a single retention policy. It is safe for
// concurrent use provided the injected dependencies are.
type Enforcer struct {
	op      jmap.EmailOperator
	cold    ColdMover
	pool    *pgxpool.Pool
	audit   auditLogger
	metrics *Metrics
	logger  *log.Logger
	dryRun  bool

	// now is overridable for deterministic tests.
	now func() time.Time
}

// NewEnforcer builds an Enforcer over the shared email operator.
// When `pool` is non-nil the enforcer logs runs to
// `retention_enforcement_log` and mirrors them into the audit chain
// (via a pool-backed audit.Service); pass a nil pool in unit tests
// that exercise the paging/destroy logic without a database.
func NewEnforcer(op jmap.EmailOperator, pool *pgxpool.Pool, logger *log.Logger) *Enforcer {
	if logger == nil {
		logger = log.Default()
	}
	e := &Enforcer{op: op, pool: pool, logger: logger, now: time.Now}
	if pool != nil {
		e.audit = audit.NewService(pool)
	}
	return e
}

// WithDryRun toggles dry-run mode (default false). A dry-run
// enforcer reads the matching window but performs no destroy /
// archive calls and increments no volume metrics.
func (e *Enforcer) WithDryRun(b bool) *Enforcer { e.dryRun = b; return e }

// WithMetrics wires the Prometheus counter set. The enforcer owns
// the deleted / archived volume counters; the worker owns the
// evaluation / error counters.
func (e *Enforcer) WithMetrics(m *Metrics) *Enforcer { e.metrics = m; return e }

// WithColdMover wires the cold-tier mover used by archive policies.
func (e *Enforcer) WithColdMover(c ColdMover) *Enforcer { e.cold = c; return e }

// WithAudit overrides the audit sink (tests). Production callers
// rely on the pool-backed default from NewEnforcer.
func (e *Enforcer) WithAudit(a auditLogger) *Enforcer { e.audit = a; return e }

// withClock overrides the time source (tests).
func (e *Enforcer) withClock(now func() time.Time) *Enforcer {
	if now != nil {
		e.now = now
	}
	return e
}

// EnforcePolicy applies one policy for one tenant and returns the
// resulting run record. The returned error is non-nil when the
// sweep failed partway through; the run still carries whatever
// counts were achieved before the failure so the caller can record
// partial progress.
func (e *Enforcer) EnforcePolicy(ctx context.Context, tenantID string, policy Policy) (*EnforcementRun, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("retention: tenantID is required")
	}
	if e.op == nil {
		return nil, errors.New("retention: enforcer has no email operator")
	}

	run := &EnforcementRun{PolicyID: policy.ID, StartedAt: e.now().UTC()}
	if e.dryRun {
		run.Notes = "dry_run=true"
	}
	// Open the log row up-front so an in-flight (or crashed) run is
	// visible to the admin status card. A logging failure is
	// non-fatal: the sweep still runs and metrics/audit still fire.
	if id, startedAt, err := e.startLog(ctx, tenantID, policy.ID, run.Notes); err != nil {
		e.logger.Printf("retention: open enforcement log (tenant %s policy %s): %v", tenantID, policy.ID, err)
	} else {
		run.ID = id
		run.StartedAt = startedAt
	}

	mailbox, err := scopeMailbox(policy)
	if err != nil {
		return e.complete(ctx, tenantID, policy, run, err)
	}
	cutoff := e.now().Add(-time.Duration(policy.RetentionDays) * 24 * time.Hour)

	var prev []string
	for page := 0; ; page++ {
		if page >= maxPages {
			return e.complete(ctx, tenantID, policy, run, fmt.Errorf("retention: exceeded %d pages", maxPages))
		}
		ids, qerr := e.op.QueryEmailsByDate(ctx, tenantID, mailbox, cutoff, queryPageSize)
		if qerr != nil {
			return e.complete(ctx, tenantID, policy, run, fmt.Errorf("query emails: %w", qerr))
		}
		if len(ids) == 0 {
			break
		}
		// Live sweeps page by re-querying: destroyed messages drop
		// out of the cutoff window so the next call returns the next
		// batch. If the identical batch comes back, destroy isn't
		// actually removing anything — bail rather than loop.
		if !e.dryRun && equalStrings(ids, prev) {
			return e.complete(ctx, tenantID, policy, run, errNoProgress)
		}
		prev = ids
		run.EmailsProcessed += len(ids)

		if e.dryRun {
			// Nothing is destroyed, so re-querying would return the
			// same page forever. Report the first page as a lower
			// bound and stop.
			break
		}

		deleted, archived, aerr := e.applyBatches(ctx, tenantID, policy.PolicyType, ids)
		run.EmailsDeleted += deleted
		run.EmailsArchived += archived
		if aerr != nil {
			return e.complete(ctx, tenantID, policy, run, aerr)
		}
	}

	return e.complete(ctx, tenantID, policy, run, nil)
}

// applyBatches chunks ids into destroyChunk-sized requests and
// destroys (delete) or moves-then-destroys (archive) each chunk.
func (e *Enforcer) applyBatches(ctx context.Context, tenantID, policyType string, ids []string) (deleted, archived int, err error) {
	for start := 0; start < len(ids); start += destroyChunk {
		end := min(start+destroyChunk, len(ids))
		batch := ids[start:end]

		switch policyType {
		case "delete":
			if err = e.op.DestroyEmails(ctx, tenantID, batch); err != nil {
				return deleted, archived, fmt.Errorf("destroy: %w", err)
			}
			deleted += len(batch)
		case "archive":
			if e.cold == nil {
				return deleted, archived, errors.New("retention: archive policy requires a cold-tier mover")
			}
			if _, err = e.cold.MoveToCold(ctx, tenantID, batch); err != nil {
				return deleted, archived, fmt.Errorf("archive move: %w", err)
			}
			// Cold tier now owns the bytes; drop the Stalwart-side
			// index entry so the message no longer counts against
			// the live mailbox (and falls out of the cutoff window).
			if err = e.op.DestroyEmails(ctx, tenantID, batch); err != nil {
				return deleted, archived, fmt.Errorf("archive destroy: %w", err)
			}
			archived += len(batch)
		default:
			return deleted, archived, fmt.Errorf("retention: unsupported policy_type %q", policyType)
		}
	}
	return deleted, archived, nil
}

// complete finalises a run: it stamps completion, persists the log
// row, increments volume metrics (live mode only), and writes the
// audit entry. It returns the run plus the supplied runErr so
// callers can `return e.complete(...)` directly.
func (e *Enforcer) complete(ctx context.Context, tenantID string, policy Policy, run *EnforcementRun, runErr error) (*EnforcementRun, error) {
	done := e.now().UTC()
	run.CompletedAt = &done
	if runErr != nil {
		run.Error = runErr.Error()
	}

	if err := e.updateLog(ctx, tenantID, run); err != nil {
		e.logger.Printf("retention: finalise enforcement log (tenant %s policy %s): %v", tenantID, policy.ID, err)
	}

	// Count whatever was actually destroyed / archived, even on a
	// partial failure — those deletes really happened.
	if !e.dryRun {
		e.incDeleted(run.EmailsDeleted)
		e.incArchived(run.EmailsArchived)
	}

	e.writeAudit(ctx, tenantID, policy, run, runErr)
	return run, runErr
}

func (e *Enforcer) incDeleted(n int) {
	if n > 0 && e.metrics != nil {
		e.metrics.EmailsDeleted.Add(float64(n))
	}
}

func (e *Enforcer) incArchived(n int) {
	if n > 0 && e.metrics != nil {
		e.metrics.EmailsArchived.Add(float64(n))
	}
}

// startLog inserts the open enforcement-log row and returns its id
// plus the DB-assigned start time. Wrapped in an RLS-scoped tx so
// the row is attributable to the tenant.
func (e *Enforcer) startLog(ctx context.Context, tenantID, policyID, notes string) (string, time.Time, error) {
	if e.pool == nil {
		return "", e.now().UTC(), nil
	}
	var (
		id        string
		startedAt time.Time
	)
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO retention_enforcement_log (tenant_id, policy_id, notes)
			VALUES ($1::uuid, $2::uuid, $3)
			RETURNING id::text, started_at
		`, tenantID, policyID, notes).Scan(&id, &startedAt)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return id, startedAt.UTC(), nil
}

// updateLog stamps the final counts on the run row. The UPDATE runs
// inside an RLS-scoped tx — without the tenant GUC the row-level
// policy hides the row and the update silently affects zero rows.
func (e *Enforcer) updateLog(ctx context.Context, tenantID string, run *EnforcementRun) error {
	if e.pool == nil || run.ID == "" {
		return nil
	}
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE retention_enforcement_log
			SET emails_processed = $2,
			    emails_deleted   = $3,
			    emails_archived  = $4,
			    completed_at     = now(),
			    error            = $5,
			    notes            = COALESCE(NULLIF($6, ''), notes)
			WHERE id = $1::uuid
		`, run.ID, run.EmailsProcessed, run.EmailsDeleted, run.EmailsArchived, run.Error, run.Notes)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("retention: enforcement log row %s not found for tenant %s", run.ID, tenantID)
		}
		return nil
	})
}

// writeAudit mirrors the run into the tamper-evident audit chain.
func (e *Enforcer) writeAudit(ctx context.Context, tenantID string, p Policy, run *EnforcementRun, runErr error) {
	if e.audit == nil {
		return
	}
	meta := map[string]any{
		"policy_type":      p.PolicyType,
		"retention_days":   p.RetentionDays,
		"applies_to":       p.AppliesTo,
		"emails_processed": run.EmailsProcessed,
		"emails_deleted":   run.EmailsDeleted,
		"emails_archived":  run.EmailsArchived,
		"dry_run":          e.dryRun,
	}
	if p.TargetRef != "" {
		meta["target_ref"] = p.TargetRef
	}
	if runErr != nil {
		meta["error"] = runErr.Error()
	}
	if _, err := e.audit.Log(ctx, audit.Entry{
		TenantID:     tenantID,
		ActorID:      "retention-worker",
		ActorType:    audit.ActorSystem,
		Action:       "retention.enforce",
		ResourceType: "retention_policy",
		ResourceID:   p.ID,
		Metadata:     meta,
	}); err != nil {
		e.logger.Printf("retention: audit log (tenant %s policy %s): %v", tenantID, p.ID, err)
	}
}

// scopeMailbox maps a policy's applies_to / target_ref onto the
// JMAP mailbox filter the operator understands. "all" sweeps the
// whole tenant; "mailbox" scopes to one JMAP mailbox. "label" has
// no JMAP equivalent the operator exposes, so it is rejected rather
// than silently falling back to an unscoped (whole-tenant) sweep —
// that fallback would delete every message past the cutoff, far
// beyond the labelled subset the admin intended.
func scopeMailbox(p Policy) (string, error) {
	switch p.AppliesTo {
	case "all":
		return "", nil
	case "mailbox":
		if strings.TrimSpace(p.TargetRef) == "" {
			return "", errors.New("retention: mailbox policy missing target_ref")
		}
		return p.TargetRef, nil
	case "label":
		return "", errors.New("retention: label-scoped policies are not supported by the JMAP enforcer")
	default:
		return "", fmt.Errorf("retention: unknown applies_to %q", p.AppliesTo)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
