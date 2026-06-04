// Package tenant — production wiring for the self-service signup
// flow. These concrete types satisfy the narrow interfaces declared
// in signup.go and are assembled at the composition root
// (cmd/kmail-api/main.go). Keeping them out of signup.go keeps the
// service logic free of database, HTTP, and JMAP detail and lets the
// flow be unit-tested against fakes.
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// --- signup_requests persistence ---------------------------------

// pgxSignupRepository is the Postgres-backed SignupRepository. The
// `signup_requests` table is pre-tenant and carries no RLS policy, so
// these queries run against the pool directly (no SetTenantGUC).
type pgxSignupRepository struct {
	pool *pgxpool.Pool
}

// NewSignupRepository returns a Postgres-backed SignupRepository.
func NewSignupRepository(pool *pgxpool.Pool) SignupRepository {
	return &pgxSignupRepository{pool: pool}
}

const signupColumns = `id::text, email, org_name, plan,
	stripe_checkout_session_id, status, created_at, completed_at`

func scanSignup(row pgx.Row) (*SignupRequest, error) {
	var r SignupRequest
	var completed *time.Time
	if err := row.Scan(
		&r.ID, &r.Email, &r.OrgName, &r.Plan,
		&r.StripeCheckoutSessionID, &r.Status, &r.CreatedAt, &completed,
	); err != nil {
		return nil, err
	}
	r.CompletedAt = completed
	return &r, nil
}

func (p *pgxSignupRepository) Create(ctx context.Context, email, orgName, plan string) (*SignupRequest, error) {
	row := p.pool.QueryRow(ctx, `
		INSERT INTO signup_requests (email, org_name, plan, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING `+signupColumns,
		email, orgName, plan,
	)
	return scanSignup(row)
}

func (p *pgxSignupRepository) GetByID(ctx context.Context, id string) (*SignupRequest, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+signupColumns+`
		FROM signup_requests WHERE id = $1::uuid`, id)
	r, err := scanSignup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSignupNotFound
	}
	return r, err
}

func (p *pgxSignupRepository) GetByCheckoutSession(ctx context.Context, sessionID string) (*SignupRequest, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT `+signupColumns+`
		FROM signup_requests WHERE stripe_checkout_session_id = $1`, sessionID)
	r, err := scanSignup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSignupNotFound
	}
	return r, err
}

func (p *pgxSignupRepository) SetCheckoutSession(ctx context.Context, id, sessionID string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE signup_requests SET stripe_checkout_session_id = $2
		WHERE id = $1::uuid`, id, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSignupNotFound
	}
	return nil
}

func (p *pgxSignupRepository) MarkActive(ctx context.Context, id string, completedAt time.Time) error {
	// Conditional on the row not already being active so a replayed
	// completion is a no-op rather than re-stamping completed_at.
	_, err := p.pool.Exec(ctx, `
		UPDATE signup_requests
		SET status = 'active', completed_at = $2
		WHERE id = $1::uuid AND status <> 'active'`, id, completedAt)
	return err
}

func (p *pgxSignupRepository) MarkFailed(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE signup_requests SET status = 'failed'
		WHERE id = $1::uuid AND status = 'pending'`, id)
	return err
}

// --- tenant provisioning -----------------------------------------

// signupProvisioner adapts the tenant Service to the TenantProvisioner
// interface CompleteSignup drives. CreateTenant / CreateAdminUser
// translate the Postgres unique-violation (23505) into the
// ErrTenantExists / ErrUserExists sentinels that drive idempotency,
// and the two extra operations (GetTenantBySlug, MarkSelfService) that
// aren't on the base Service are implemented here against the pool.
type signupProvisioner struct {
	svc  *Service
	pool *pgxpool.Pool
}

// NewSignupProvisioner returns a TenantProvisioner backed by the
// tenant Service (for the provisioning chain) and the pool (for the
// signup-specific reads/writes).
func NewSignupProvisioner(svc *Service, pool *pgxpool.Pool) TenantProvisioner {
	return &signupProvisioner{svc: svc, pool: pool}
}

func (p *signupProvisioner) CreateTenant(ctx context.Context, in CreateTenantInput) (*Tenant, error) {
	t, err := p.svc.CreateTenant(ctx, in)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrTenantExists
		}
		return nil, err
	}
	return t, nil
}

func (p *signupProvisioner) GetTenantBySlug(ctx context.Context, slug string) (*Tenant, error) {
	var t Tenant
	err := p.pool.QueryRow(ctx, `
		SELECT id::text, name, slug, plan, status, created_at, updated_at
		FROM tenants WHERE slug = $1`, slug).Scan(
		&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant by slug: %w", err)
	}
	return &t, nil
}

func (p *signupProvisioner) MarkSelfService(ctx context.Context, tenantID string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE tenants SET self_service = true WHERE id = $1::uuid`, tenantID)
	return err
}

// CreateAdminUser creates the first owner user for a self-service
// tenant. The KChat user id and Stalwart account id are synthesized
// deterministically from the (globally unique) email so a replayed
// completion collides on the users.email UNIQUE constraint and
// surfaces ErrUserExists rather than creating a duplicate. The KChat
// account is linked for real when the admin first authenticates
// through KChat SSO; until then the synthesized id is the stable
// placeholder the SSO link upserts against.
func (p *signupProvisioner) CreateAdminUser(ctx context.Context, tenantID, email, displayName string) (*User, error) {
	u, err := p.svc.CreateUser(ctx, tenantID, CreateUserInput{
		KChatUserID:       "selfservice:" + email,
		StalwartAccountID: email,
		Email:             email,
		DisplayName:       displayName,
		Role:              "owner",
		AccountType:       "user",
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrUserExists
		}
		return nil, err
	}
	return u, nil
}

// --- Stripe Checkout ----------------------------------------------

// StripeCheckoutHTTP is the production StripeCheckoutClient. It posts
// to the Stripe REST API directly (form-encoded, basic auth with the
// secret key) so it can be constructed from the existing
// billing.StripeClient's exported fields without the tenant package
// importing billing (which would create an import cycle with the
// billing webhook calling back into this package).
type StripeCheckoutHTTP struct {
	APIKey  string
	BaseURL string // e.g. https://api.stripe.com
	HTTP    *http.Client
}

// CreateCheckoutSession mints a subscription-mode Checkout Session.
func (c *StripeCheckoutHTTP) CreateCheckoutSession(ctx context.Context, p CheckoutSessionParams) (*CheckoutSession, error) {
	if c.APIKey == "" {
		return nil, ErrCheckoutUnavailable
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}

	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	form.Set("customer_email", p.CustomerEmail)
	form.Set("client_reference_id", p.SignupRequestID)
	form.Set("line_items[0][price]", p.PriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("metadata[signup_request_id]", p.SignupRequestID)
	form.Set("metadata[plan]", p.Plan)
	form.Set("subscription_data[metadata][signup_request_id]", p.SignupRequestID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/checkout/sessions",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.APIKey, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe checkout: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("stripe checkout: status %d: %s", resp.StatusCode, truncateForLog(body))
	}
	var out CheckoutSession
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("stripe checkout: decode response: %w", err)
	}
	if out.ID == "" || out.URL == "" {
		return nil, fmt.Errorf("stripe checkout: response missing id/url")
	}
	return &out, nil
}

func truncateForLog(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

// --- onboarding ----------------------------------------------------

// ChecklistAdapter adapts an onboarding service to ChecklistInitializer.
type ChecklistAdapter struct {
	fn func(ctx context.Context, tenantID string) error
}

// NewChecklistAdapter wraps a warm-up function (typically a closure
// over onboarding.Service.GetChecklist) as a ChecklistInitializer.
// Returns nil when fn is nil so callers can pass the result straight
// into SignupConfig.Checklist.
func NewChecklistAdapter(fn func(ctx context.Context, tenantID string) error) ChecklistInitializer {
	if fn == nil {
		return nil
	}
	return &ChecklistAdapter{fn: fn}
}

// InitChecklist implements ChecklistInitializer.
func (a *ChecklistAdapter) InitChecklist(ctx context.Context, tenantID string) error {
	return a.fn(ctx, tenantID)
}

// --- welcome email (Stalwart JMAP submission) ---------------------

// jmapSubmitter is the slice of jmap.InternalClient the welcome mailer
// uses. *jmap.InternalClient satisfies it.
type jmapSubmitter interface {
	ResolveAccountID(ctx context.Context, tenantID, kchatUserID string) (string, error)
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

// JMAPWelcomeMailer sends the welcome email via a Stalwart JMAP
// EmailSubmission, sent as a configured system notification mailbox.
// It is wired only when a system sender is configured; otherwise the
// SignupService runs with a nil mailer and skips the welcome email.
type JMAPWelcomeMailer struct {
	submitter         jmapSubmitter
	senderTenantID    string
	senderKChatUserID string
	fromAddress       string
	identityID        string // optional Stalwart Identity id for the submission
	logger            *log.Logger
}

// NewJMAPWelcomeMailer constructs a welcome mailer. Returns nil if any
// required field is missing so callers can pass the result directly
// into SignupConfig.Mailer (a nil mailer disables the welcome email).
func NewJMAPWelcomeMailer(submitter *jmap.InternalClient, senderTenantID, senderKChatUserID, fromAddress, identityID string, logger *log.Logger) WelcomeMailer {
	if submitter == nil || senderTenantID == "" || senderKChatUserID == "" || fromAddress == "" {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}
	return &JMAPWelcomeMailer{
		submitter:         submitter,
		senderTenantID:    senderTenantID,
		senderKChatUserID: senderKChatUserID,
		fromAddress:       fromAddress,
		identityID:        identityID,
		logger:            logger,
	}
}

// SendWelcome builds an Email/set draft + EmailSubmission/set and
// dispatches it to the sender's Stalwart shard.
func (m *JMAPWelcomeMailer) SendWelcome(ctx context.Context, msg WelcomeMessage) error {
	accountID, err := m.submitter.ResolveAccountID(ctx, m.senderTenantID, m.senderKChatUserID)
	if err != nil {
		return fmt.Errorf("welcome mailer: resolve sender account: %w", err)
	}

	subject := fmt.Sprintf("Welcome to KMail, %s", msg.OrgName)
	emailCreate := map[string]any{
		"mailboxIds": map[string]bool{"$drafts": true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       []map[string]string{{"email": m.fromAddress}},
		"to":         []map[string]string{{"email": msg.To}},
		"subject":    subject,
		"bodyValues": map[string]any{
			"body": map[string]string{"value": buildWelcomeBody(msg)},
		},
		"textBody": []map[string]string{{"partId": "body", "type": "text/plain"}},
	}

	submissionCreate := map[string]any{
		"emailId": "#welcome",
		"envelope": map[string]any{
			"mailFrom": map[string]string{"email": m.fromAddress},
			"rcptTo":   []map[string]string{{"email": msg.To}},
		},
	}
	if m.identityID != "" {
		submissionCreate["identityId"] = m.identityID
	}

	req := jmap.JmapRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
			"urn:ietf:params:jmap:submission",
		},
		MethodCalls: [][]any{
			{"Email/set", map[string]any{
				"accountId": accountID,
				"create":    map[string]any{"welcome": emailCreate},
			}, "c0"},
			{"EmailSubmission/set", map[string]any{
				"accountId": accountID,
				"create":    map[string]any{"sub": submissionCreate},
			}, "c1"},
		},
	}

	resp, err := m.submitter.Dispatch(ctx, m.senderTenantID, m.senderKChatUserID, req)
	if err != nil {
		return fmt.Errorf("welcome mailer: dispatch: %w", err)
	}
	if err := resp.FirstCallError(); err != nil {
		return fmt.Errorf("welcome mailer: jmap error: %w", err)
	}
	return nil
}

func buildWelcomeBody(msg WelcomeMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Welcome to KMail!\n\n")
	fmt.Fprintf(&b, "Your %s workspace on the %s plan is ready.\n\n", msg.OrgName, msg.Plan)
	fmt.Fprintf(&b, "Finish setting up your domain and DNS here:\n%s\n", msg.LoginURL)
	if msg.DNSRecords != nil && len(msg.DNSRecords.Records) > 0 {
		fmt.Fprintf(&b, "\nTo send mail from %s, publish these DNS records:\n", msg.DNSRecords.Domain)
		for _, r := range msg.DNSRecords.Records {
			fmt.Fprintf(&b, "  %-5s %s\t%s\n", r.Type, r.Name, r.Value)
		}
	}
	return b.String()
}

// Compile-time assertions that the production types satisfy their
// interfaces.
var (
	_ SignupRepository     = (*pgxSignupRepository)(nil)
	_ TenantProvisioner    = (*signupProvisioner)(nil)
	_ StripeCheckoutClient = (*StripeCheckoutHTTP)(nil)
	_ WelcomeMailer        = (*JMAPWelcomeMailer)(nil)
)
