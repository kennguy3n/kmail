// Package onboarding — Phase 5 guided checklist.
//
// Computes a per-tenant onboarding checklist by querying the
// existing tables (`domains`, `users`, `tenants`, `migration_jobs`,
// `shared_inboxes`, `billing_events`). Optional steps that an
// admin has explicitly skipped are persisted in
// `onboarding_progress`.
package onboarding

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Status values.
type StepStatus string

const (
	StatusPending  StepStatus = "pending"
	StatusComplete StepStatus = "complete"
	StatusSkipped  StepStatus = "skipped"
)

// Step is the public per-step view.
type Step struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      StepStatus `json:"status"`
	Optional    bool       `json:"optional"`
	Link        string     `json:"link,omitempty"`
}

// Checklist is the response shape.
type Checklist struct {
	TenantID  string    `json:"tenant_id"`
	Steps     []Step    `json:"steps"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Service implements the onboarding API.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// GetChecklist computes the checklist for a tenant.
func (s *Service) GetChecklist(ctx context.Context, tenantID string) (*Checklist, error) {
	if tenantID == "" {
		return nil, errors.New("onboarding: tenant required")
	}
	if s.pool == nil {
		return nil, errors.New("onboarding: pool not configured")
	}
	out := Checklist{TenantID: tenantID, UpdatedAt: time.Now().UTC()}
	stats, skipped, err := s.queryStats(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out.Steps = []Step{
		stepFromBool("add_domain", "Add custom domain",
			"Connect your sending domain to KMail.", false,
			"/admin/domains", stats.HasDomain, skipped),
		stepFromBool("verify_dns", "Verify DNS records",
			"Publish MX, SPF, DKIM, and DMARC records.", false,
			"/admin/domains", stats.AllDNSVerified, skipped),
		stepFromBool("create_user", "Create first user",
			"Add at least one user to your tenant.", false,
			"/admin/users", stats.HasUser, skipped),
		stepFromBool("send_test_email", "Send a test email",
			"Verify deliverability by sending one message.", false,
			"/admin/deliverability", stats.HasSend, skipped),
		stepFromBool("shared_inbox", "Set up shared inbox",
			"Optional: create a team mailbox.", true,
			"/admin/shared-inboxes", stats.HasSharedInbox, skipped),
		stepFromBool("billing_plan", "Configure billing plan",
			"Pick Core, Pro, or Privacy.", false,
			"/admin/billing", stats.HasPlan, skipped),
		stepFromBool("start_migration", "Start migration",
			"Optional: import mail from your existing provider.", true,
			"/admin/migrations", stats.HasMigration, skipped),
		stepFromBool("invite_team", "Invite team members",
			"Optional: bring your team to KMail.", true,
			"/admin/users", stats.UserCount > 1, skipped),
	}
	return &out, nil
}

// SkipStep marks an optional step as skipped.
func (s *Service) SkipStep(ctx context.Context, tenantID, stepID string) error {
	if tenantID == "" || stepID == "" {
		return errors.New("onboarding: tenant + step required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO onboarding_progress (tenant_id, step_id)
			VALUES ($1::uuid, $2)
			ON CONFLICT (tenant_id, step_id) DO NOTHING
		`, tenantID, stepID)
		return err
	})
}

// UnskipStep clears the skipped flag.
func (s *Service) UnskipStep(ctx context.Context, tenantID, stepID string) error {
	if tenantID == "" || stepID == "" {
		return errors.New("onboarding: tenant + step required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM onboarding_progress WHERE tenant_id = $1::uuid AND step_id = $2
		`, tenantID, stepID)
		return err
	})
}

// stats is the bag of booleans GetChecklist computes from the DB.
type stats struct {
	HasDomain      bool
	AllDNSVerified bool
	HasUser        bool
	HasSend        bool
	HasSharedInbox bool
	HasPlan        bool
	HasMigration   bool
	UserCount      int
}

func (s *Service) queryStats(ctx context.Context, tenantID string) (stats, map[string]bool, error) {
	var st stats
	skipped := map[string]bool{}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Domains.
		_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM domains WHERE tenant_id = $1::uuid)`, tenantID).Scan(&st.HasDomain)
		_ = tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM domains
				WHERE tenant_id = $1::uuid
				  AND mx_verified AND spf_verified AND dkim_verified AND dmarc_verified
			)
		`, tenantID).Scan(&st.AllDNSVerified)
		// Users.
		_ = tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM users WHERE tenant_id = $1::uuid AND account_type = 'user' AND status = 'active'
		`, tenantID).Scan(&st.UserCount)
		st.HasUser = st.UserCount > 0
		// Plan.
		var plan string
		if err := tx.QueryRow(ctx, `SELECT plan FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&plan); err == nil {
			st.HasPlan = plan != ""
		}
		// Migration.
		_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM migration_jobs WHERE tenant_id = $1::uuid)`, tenantID).Scan(&st.HasMigration)
		// Shared inbox.
		_ = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE tenant_id = $1::uuid AND account_type = 'shared_inbox')`, tenantID).Scan(&st.HasSharedInbox)
		// Send activity (best-effort: any audit log entry tagged
		// with a send action). audit_log is canonical for user
		// actions across the BFF, so it's a more durable signal
		// than billing_events.
		_ = tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM audit_log
				WHERE tenant_id = $1::uuid
				  AND action IN ('email_send', 'email.send', 'jmap_send', 'mail_sent')
			)
		`, tenantID).Scan(&st.HasSend)

		// Skipped steps.
		rows, err := tx.Query(ctx, `SELECT step_id FROM onboarding_progress WHERE tenant_id = $1::uuid`, tenantID)
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			skipped[id] = true
		}
		return nil
	})
	return st, skipped, err
}

func stepFromBool(id, title, description string, optional bool, link string, complete bool, skipped map[string]bool) Step {
	st := StatusPending
	if complete {
		st = StatusComplete
	} else if skipped[id] {
		st = StatusSkipped
	}
	return Step{
		ID:          id,
		Title:       title,
		Description: description,
		Status:      st,
		Optional:    optional,
		Link:        link,
	}
}
