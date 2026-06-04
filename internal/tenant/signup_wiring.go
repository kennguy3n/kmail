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
	return classifyCreateTenantResult(p.svc.CreateTenant(ctx, in))
}

// classifyCreateTenantResult translates the (tenant, error) pair returned
// by Service.CreateTenant into the signup-flow sentinels.
//
// The order of checks matters. Service.CreateTenant returns a nil tenant
// only when the INSERT itself failed (see internal/tenant/service.go); a
// slug collision there is the genuine "tenant already exists" signal
// (ErrTenantExists). Once the INSERT succeeds it returns a non-nil tenant
// alongside any post-insert hook error — and that hook error can itself be
// a Postgres unique violation (e.g. a billing upsert hitting 23505). So the
// t != nil (partial-provisioning) branch must be evaluated BEFORE the
// isUniqueViolation branch; otherwise a freshly-created tenant whose hook
// raised a 23505 would be misclassified as a pre-existing tenant and its
// valid pointer discarded.
func classifyCreateTenantResult(t *Tenant, err error) (*Tenant, error) {
	if err == nil {
		return t, nil
	}
	// INSERT succeeded but a post-insert provisioning hook failed.
	// Preserve the tenant and signal the partial state so CompleteSignup
	// re-drives the idempotent hooks instead of discarding the pointer and
	// permanently failing the signup over a half-provisioned tenant.
	if t != nil {
		return t, fmt.Errorf("%w: %v", ErrTenantProvisionIncomplete, err)
	}
	// INSERT failed. A unique violation here is a slug collision —
	// the idempotent "tenant already exists" replay signal.
	if isUniqueViolation(err) {
		return nil, ErrTenantExists
	}
	return nil, err
}

// EnsureProvisioned re-runs the idempotent post-insert provisioning
// hooks (zk-object-fabric bucket, billing lifecycle) against an
// existing tenant. It mirrors the hook sequence in Service.CreateTenant
// and is safe to call repeatedly — CreateBucket / placement PUT and the
// billing OnTenantCreated upsert all no-op when the resource already
// exists for the tenant.
func (p *signupProvisioner) EnsureProvisioned(ctx context.Context, tenantID, plan string) error {
	if p.svc.provisioner != nil {
		if _, err := p.svc.provisioner.Provision(ctx, tenantID, plan); err != nil {
			return fmt.Errorf("zk-fabric provision: %w", err)
		}
	}
	if p.svc.billing != nil {
		if err := p.svc.billing.OnTenantCreated(ctx, tenantID, plan); err != nil {
			return fmt.Errorf("billing.OnTenantCreated: %w", err)
		}
	}
	return nil
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

	// Resolve the sender's real Drafts mailbox id. JMAP (RFC 8620 §5.1)
	// reserves the `$`-prefix for *creation-id back-references* within a
	// single request batch, not for well-known mailbox names — so a
	// literal "$drafts" mailbox id makes the Email/set create land in
	// `notCreated` and the whole welcome send silently no-ops. We must
	// pass the account's actual Drafts mailbox id (RFC 8621 §2 `role`).
	mailboxID, err := m.resolveDraftsMailbox(ctx, accountID)
	if err != nil {
		return fmt.Errorf("welcome mailer: resolve drafts mailbox: %w", err)
	}

	subject := fmt.Sprintf("Welcome to KMail, %s", msg.OrgName)
	emailCreate := map[string]any{
		"mailboxIds": map[string]bool{mailboxID: true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       []map[string]string{{"email": m.fromAddress}},
		"to":         []map[string]string{{"email": msg.To}},
		"subject":    subject,
		"bodyValues": map[string]any{
			"body": map[string]string{"value": buildWelcomeBody(msg)},
		},
		"textBody": []map[string]string{{"partId": "body", "type": "text/plain"}},
	}

	// RFC 8621 §7 makes identityId a required property on
	// EmailSubmission creation. When an operator hasn't pinned one via
	// KMAIL_SIGNUP_WELCOME_IDENTITY, resolve the sender's real identity
	// (preferring the one matching the from address) rather than omitting
	// the property — a strictly-conformant Stalwart config would reject a
	// submission with no identityId, silently dropping the welcome email.
	identityID := m.identityID
	if identityID == "" {
		resolved, rErr := m.resolveIdentityID(ctx, accountID)
		if rErr != nil {
			// Best-effort: the account may have no identity provisioned
			// yet. Fall back to omitting identityId and let the server
			// auto-select, preserving prior behavior instead of failing
			// the (best-effort) welcome email outright.
			m.logger.Printf("welcome mailer: resolve identity (account=%s): %v; submitting without identityId", accountID, rErr)
		} else {
			identityID = resolved
		}
	}

	submissionCreate := map[string]any{
		"emailId": "#welcome",
		"envelope": map[string]any{
			"mailFrom": map[string]string{"email": m.fromAddress},
			"rcptTo":   []map[string]string{{"email": msg.To}},
		},
	}
	if identityID != "" {
		submissionCreate["identityId"] = identityID
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
				// Don't leave the welcome message sitting in Drafts once
				// Stalwart has handed it off — mirrors the undosend worker.
				"onSuccessDestroyEmail": []any{"#welcome"},
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
	// A JMAP set call returns HTTP 200 even when a create is rejected —
	// the failure surfaces in `notCreated` / `notSent` (RFC 8620 §5.3),
	// not as a method-level error — so inspect those explicitly.
	if _, args, ok := resp.CallByID("c0"); ok {
		if err := firstSetError("Email/set", args, "notCreated"); err != nil {
			return fmt.Errorf("welcome mailer: %w", err)
		}
	}
	if _, args, ok := resp.CallByID("c1"); ok {
		if err := firstSetError("EmailSubmission/set", args, "notCreated"); err != nil {
			return fmt.Errorf("welcome mailer: %w", err)
		}
	}
	return nil
}

// resolveDraftsMailbox returns the account's Drafts mailbox id. It
// prefers the mailbox whose JMAP `role` is "drafts" (RFC 8621 §2) and
// falls back to the first mailbox so the transient welcome draft still
// has a valid home (it is destroyed on successful submission anyway).
func (m *JMAPWelcomeMailer) resolveDraftsMailbox(ctx context.Context, accountID string) (string, error) {
	req := jmap.JmapRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
		},
		MethodCalls: [][]any{
			{"Mailbox/get", map[string]any{
				"accountId":  accountID,
				"ids":        nil,
				"properties": []string{"id", "role"},
			}, "m0"},
		},
	}
	resp, err := m.submitter.Dispatch(ctx, m.senderTenantID, m.senderKChatUserID, req)
	if err != nil {
		return "", err
	}
	if err := resp.FirstCallError(); err != nil {
		return "", err
	}
	_, args, ok := resp.CallByID("m0")
	if !ok {
		return "", fmt.Errorf("mailbox/get: response missing m0 call")
	}
	list, _ := args["list"].([]any)
	var fallback string
	for _, item := range list {
		mb, _ := item.(map[string]any)
		id, _ := mb["id"].(string)
		if id == "" {
			continue
		}
		role, _ := mb["role"].(string)
		if strings.EqualFold(role, "drafts") {
			return id, nil
		}
		if fallback == "" {
			fallback = id
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("mailbox/get: account %s has no mailboxes", accountID)
}

// resolveIdentityID returns a valid JMAP Identity id for the sender,
// preferring the identity whose email matches the configured from
// address and falling back to the first identity. RFC 8621 §7 requires
// identityId on EmailSubmission creation; this is used only when no
// identity id is configured so strictly-conformant servers still accept
// the submission.
func (m *JMAPWelcomeMailer) resolveIdentityID(ctx context.Context, accountID string) (string, error) {
	req := jmap.JmapRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:submission",
		},
		MethodCalls: [][]any{
			{"Identity/get", map[string]any{
				"accountId":  accountID,
				"ids":        nil,
				"properties": []string{"id", "email"},
			}, "i0"},
		},
	}
	resp, err := m.submitter.Dispatch(ctx, m.senderTenantID, m.senderKChatUserID, req)
	if err != nil {
		return "", err
	}
	if err := resp.FirstCallError(); err != nil {
		return "", err
	}
	_, args, ok := resp.CallByID("i0")
	if !ok {
		return "", fmt.Errorf("identity/get: response missing i0 call")
	}
	list, _ := args["list"].([]any)
	var fallback string
	for _, item := range list {
		idn, _ := item.(map[string]any)
		id, _ := idn["id"].(string)
		if id == "" {
			continue
		}
		email, _ := idn["email"].(string)
		if strings.EqualFold(email, m.fromAddress) {
			return id, nil
		}
		if fallback == "" {
			fallback = id
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("identity/get: account %s has no identities", accountID)
}

// firstSetError returns a descriptive error for the first rejected
// entry in a JMAP set response's `notCreated`/`notUpdated`/`notSent`
// map, or nil when there are none.
func firstSetError(method string, args map[string]any, key string) error {
	rejected, _ := args[key].(map[string]any)
	for id, v := range rejected {
		entry, _ := v.(map[string]any)
		typ, _ := entry["type"].(string)
		desc, _ := entry["description"].(string)
		return fmt.Errorf("%s %s[%s]: %s: %s", method, key, id, typ, desc)
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
