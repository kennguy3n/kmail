// Package tenant — self-service tenant signup (gap-closure Session 3).
//
// This file implements the public, no-auth signup funnel that turns
// a prospective customer into a fully provisioned tenant:
//
//	InitiateSignup  -> persists a `signup_requests` row (pre-tenant,
//	                   no RLS — see migrations/004_self_service_signup.sql)
//	                   and mints a Stripe Checkout Session.
//	CompleteSignup  -> driven by the Stripe `checkout.session.completed`
//	                   webhook; provisions the tenant (CreateTenant chains
//	                   into the zk-object-fabric provisioner, billing
//	                   Lifecycle, and shard assignment), creates the first
//	                   admin user, warms the onboarding checklist, flips
//	                   the signup row to `active`, and sends a welcome
//	                   email. Idempotent: a replayed webhook maps back to
//	                   the same tenant.
//
// Provisioning, persistence, Stripe, onboarding, audit, and mail are
// all expressed as narrow interfaces so the flow is unit-testable
// without a database or any live external service (see
// signup_wiring.go for the production concrete implementations).
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/hex"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/dns"
)

// Signup-specific sentinel errors. ErrInvalidInput / ErrNotFound are
// reused from service.go so handlers map them with the existing
// statusForServiceError switch.
var (
	// ErrSignupNotFound is returned when a checkout session id (or
	// signup id) resolves no `signup_requests` row.
	ErrSignupNotFound = errors.New("signup: request not found")

	// ErrCheckoutUnavailable is returned by InitiateSignup when no
	// Stripe checkout client is wired or no price id is configured
	// for the requested plan, so a checkout URL cannot be minted.
	ErrCheckoutUnavailable = errors.New("signup: checkout unavailable")

	// ErrTenantExists is returned by a TenantProvisioner when the
	// deterministic per-signup slug already exists — the signal
	// CompleteSignup uses to take its idempotent "return the
	// already-provisioned tenant" branch.
	ErrTenantExists = errors.New("signup: tenant already exists")

	// ErrUserExists is returned by a TenantProvisioner when the
	// admin user's (globally unique) email already has a row —
	// the idempotent "admin already created" signal.
	ErrUserExists = errors.New("signup: user already exists")

	// ErrTenantProvisionIncomplete is returned by a TenantProvisioner's
	// CreateTenant when the tenant row was inserted but a post-insert
	// provisioning hook (zk-object-fabric bucket, billing lifecycle)
	// failed. The tenant pointer is still returned alongside it so
	// CompleteSignup can re-drive the idempotent hooks via
	// EnsureProvisioned instead of permanently failing the signup and
	// leaving a half-provisioned tenant behind.
	ErrTenantProvisionIncomplete = errors.New("signup: tenant provisioning incomplete")
)

// PlanTier is a single self-service plan card surfaced on the public
// signup page and validated server-side on InitiateSignup. The IDs
// mirror the `tenants.plan` / `signup_requests.plan` CHECK
// constraint exactly (`core`, `pro`, `privacy`).
type PlanTier struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Features    []string `json:"features"`
}

// PlanCatalog is the authoritative list of self-service plans. The
// price the customer actually pays is governed by the Stripe price
// id wired per-plan at the composition root (KMAIL_STRIPE_PRICE_*),
// so this catalog intentionally carries marketing copy, not amounts.
var PlanCatalog = []PlanTier{
	{
		ID:          "core",
		Name:        "Core",
		Description: "Privacy-first email and calendar for small teams.",
		Features: []string{
			"Encrypted mailboxes",
			"Custom domain",
			"Shared inboxes",
		},
	},
	{
		ID:          "pro",
		Name:        "Pro",
		Description: "Advanced controls and higher quotas for growing businesses.",
		Features: []string{
			"Everything in Core",
			"Higher storage quotas",
			"Confidential send + portals",
			"Priority deliverability",
		},
	},
	{
		ID:          "privacy",
		Name:        "Privacy",
		Description: "Zero-access vaults and client-side encryption for the privacy-obsessed.",
		Features: []string{
			"Everything in Pro",
			"Zero-access (StrictZK) vaults",
			"Client-side encryption keys",
			"Customer-managed key (CMK) support",
		},
	},
}

// PlanByID returns the catalog entry for id and whether it exists.
func PlanByID(id string) (PlanTier, bool) {
	for _, p := range PlanCatalog {
		if p.ID == id {
			return p, true
		}
	}
	return PlanTier{}, false
}

// SignupRequest mirrors a row in `signup_requests` plus the derived
// (non-persisted) CheckoutURL InitiateSignup hands back to the UI.
type SignupRequest struct {
	ID                      string     `json:"id"`
	Email                   string     `json:"email"`
	OrgName                 string     `json:"org_name"`
	Plan                    string     `json:"plan"`
	StripeCheckoutSessionID string     `json:"stripe_checkout_session_id,omitempty"`
	Status                  string     `json:"status"`
	CreatedAt               time.Time  `json:"created_at"`
	CompletedAt             *time.Time `json:"completed_at,omitempty"`

	// CheckoutURL is the Stripe-hosted Checkout page the client
	// redirects to. Populated by InitiateSignup; never read back
	// from the DB.
	CheckoutURL string `json:"checkout_url,omitempty"`
}

// SignupStatusView is the minimal, public projection of a signup
// request returned by the unauthenticated GET /signup/{id}/status
// polling endpoint.
//
// That route is gated only by the unguessability of the UUID id, so it
// must not leak anything an attacker who guessed (or was handed) the id
// shouldn't see. It therefore deliberately omits the PII the funnel
// collected (Email, OrgName) and the StripeCheckoutSessionID — the
// polling UI only branches on Status. The remaining fields (plan and
// timestamps) are non-identifying funnel metadata safe to expose.
type SignupStatusView struct {
	ID          string     `json:"id"`
	Plan        string     `json:"plan"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// SignupRepository persists the pre-tenant `signup_requests` table.
// The table carries no tenant_id and no RLS policy (access is gated
// at the handler layer), so the concrete implementation talks to the
// pool directly without SetTenantGUC.
type SignupRepository interface {
	// Create inserts a fresh `pending` row and returns it with the
	// DB-assigned id and created_at populated.
	Create(ctx context.Context, email, orgName, plan string) (*SignupRequest, error)
	// GetByID fetches a row by id; ErrSignupNotFound when absent.
	GetByID(ctx context.Context, id string) (*SignupRequest, error)
	// GetByCheckoutSession fetches a row by Stripe Checkout Session
	// id; ErrSignupNotFound when absent.
	GetByCheckoutSession(ctx context.Context, sessionID string) (*SignupRequest, error)
	// SetCheckoutSession records the Stripe Checkout Session id on a
	// pending row once the session has been minted.
	SetCheckoutSession(ctx context.Context, id, sessionID string) error
	// MarkActive flips a row to `active` and stamps completed_at.
	MarkActive(ctx context.Context, id string, completedAt time.Time) error
	// MarkFailed flips a row to `failed`.
	MarkFailed(ctx context.Context, id string) error
}

// TenantProvisioner is the slice of tenant provisioning CompleteSignup
// drives. CreateTenant deliberately surfaces ErrTenantExists (and
// CreateAdminUser ErrUserExists) on a unique-constraint collision so
// the caller can take the idempotent replay branch without the
// persistence-layer error types leaking up here.
type TenantProvisioner interface {
	CreateTenant(ctx context.Context, in CreateTenantInput) (*Tenant, error)
	GetTenantBySlug(ctx context.Context, slug string) (*Tenant, error)
	MarkSelfService(ctx context.Context, tenantID string) error
	CreateAdminUser(ctx context.Context, tenantID, email, displayName string) (*User, error)
	// EnsureProvisioned re-drives the idempotent post-insert
	// provisioning hooks (zk-object-fabric bucket, billing lifecycle)
	// for an existing tenant. CompleteSignup calls it whenever it did
	// not freshly create the tenant in this attempt — i.e. on a
	// replayed completion or after a prior attempt inserted the row but
	// a hook failed — so a partially-provisioned tenant heals on the
	// next webhook delivery instead of needing manual operator repair.
	EnsureProvisioned(ctx context.Context, tenantID, plan string) error
}

// CheckoutSessionParams is the input to StripeCheckoutClient.
type CheckoutSessionParams struct {
	Plan            string
	PriceID         string
	CustomerEmail   string
	SuccessURL      string
	CancelURL       string
	SignupRequestID string
}

// CheckoutSession is the trimmed result of a Stripe Checkout Session
// creation.
type CheckoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// StripeCheckoutClient mints Stripe Checkout Sessions. Implemented in
// production by StripeCheckoutHTTP (signup_wiring.go); mocked in tests.
type StripeCheckoutClient interface {
	CreateCheckoutSession(ctx context.Context, p CheckoutSessionParams) (*CheckoutSession, error)
}

// ChecklistInitializer warms the onboarding checklist for a freshly
// provisioned tenant. Optional (nil disables the call).
type ChecklistInitializer interface {
	InitChecklist(ctx context.Context, tenantID string) error
}

// AuditLogger is the slice of the audit Service CompleteSignup uses to
// record the tenant-provisioning event. Optional (nil disables).
type AuditLogger interface {
	Log(ctx context.Context, e audit.Entry) (*audit.Entry, error)
}

// WelcomeMessage is the payload for the post-provisioning welcome
// email.
type WelcomeMessage struct {
	To         string
	OrgName    string
	Plan       string
	TenantID   string
	LoginURL   string
	DNSRecords *dns.DomainRecords
}

// WelcomeMailer sends the welcome email via Stalwart JMAP submission.
// Optional (nil disables); failures are best-effort and never fail a
// signup.
type WelcomeMailer interface {
	SendWelcome(ctx context.Context, msg WelcomeMessage) error
}

// DNSRecordGenerator produces the expected DNS records for a custom
// sending domain, embedded in the welcome email. *dns.Service
// satisfies it. Optional (nil disables).
type DNSRecordGenerator interface {
	GenerateRecords(domain string) dns.DomainRecords
}

// SignupMetrics is the Prometheus metric set for the signup funnel.
type SignupMetrics struct {
	Initiated   *prometheus.CounterVec
	Completed   *prometheus.CounterVec
	Failed      prometheus.Counter
	RateLimited prometheus.Counter
	Replays     prometheus.Counter
}

// NewSignupMetrics builds and registers the signup metric set. Pass
// nil reg to skip registration (tests).
func NewSignupMetrics(reg prometheus.Registerer) *SignupMetrics {
	m := &SignupMetrics{
		Initiated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kmail_signup_initiated_total",
			Help: "Self-service signups initiated (checkout session minted), by plan.",
		}, []string{"plan"}),
		Completed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kmail_signup_completed_total",
			Help: "Self-service signups completed (tenant provisioned), by plan.",
		}, []string{"plan"}),
		Failed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_signup_failed_total",
			Help: "Self-service signups that failed during initiate or completion.",
		}),
		RateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_signup_rate_limited_total",
			Help: "Public signup requests rejected by the per-IP rate limiter.",
		}),
		Replays: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_signup_completion_replays_total",
			Help: "checkout.session.completed completions that mapped to an already-provisioned tenant (idempotent replays).",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Initiated, m.Completed, m.Failed, m.RateLimited, m.Replays)
	}
	return m
}

func (m *SignupMetrics) initiated(plan string) {
	if m != nil && m.Initiated != nil {
		m.Initiated.WithLabelValues(plan).Inc()
	}
}

func (m *SignupMetrics) completed(plan string) {
	if m != nil && m.Completed != nil {
		m.Completed.WithLabelValues(plan).Inc()
	}
}

func (m *SignupMetrics) failed() {
	if m != nil && m.Failed != nil {
		m.Failed.Inc()
	}
}

func (m *SignupMetrics) replayed() {
	if m != nil && m.Replays != nil {
		m.Replays.Inc()
	}
}

// SignupConfig wires a SignupService.
type SignupConfig struct {
	Repo        SignupRepository
	Provisioner TenantProvisioner
	Stripe      StripeCheckoutClient
	Checklist   ChecklistInitializer // optional
	Audit       AuditLogger          // optional
	Mailer      WelcomeMailer        // optional
	DNS         DNSRecordGenerator   // optional
	Metrics     *SignupMetrics       // optional

	// PlanPrices maps a plan id to its Stripe price id. Required for
	// InitiateSignup to mint a checkout session.
	PlanPrices map[string]string

	// PublicBaseURL is the externally reachable base (e.g.
	// https://app.kmail.example). Used to build Stripe
	// success/cancel URLs and the welcome-email login link.
	PublicBaseURL string

	Logger *log.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// SignupService implements the self-service signup flow.
type SignupService struct {
	repo       SignupRepository
	prov       TenantProvisioner
	stripe     StripeCheckoutClient
	checklist  ChecklistInitializer
	audit      AuditLogger
	mailer     WelcomeMailer
	dns        DNSRecordGenerator
	metrics    *SignupMetrics
	planPrices map[string]string
	baseURL    string
	logger     *log.Logger
	now        func() time.Time
}

// NewSignupService constructs a SignupService. Repo, Provisioner, and
// Stripe are required; the remaining dependencies are optional.
func NewSignupService(cfg SignupConfig) *SignupService {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SignupService{
		repo:       cfg.Repo,
		prov:       cfg.Provisioner,
		stripe:     cfg.Stripe,
		checklist:  cfg.Checklist,
		audit:      cfg.Audit,
		mailer:     cfg.Mailer,
		dns:        cfg.DNS,
		metrics:    cfg.Metrics,
		planPrices: cfg.PlanPrices,
		baseURL:    strings.TrimRight(cfg.PublicBaseURL, "/"),
		logger:     cfg.Logger,
		now:        cfg.Now,
	}
}

// InitiateSignup validates the request, persists a pending
// `signup_requests` row, mints a Stripe Checkout Session, records the
// session id on the row, and returns the request with CheckoutURL set.
func (s *SignupService) InitiateSignup(ctx context.Context, email, orgName, plan string) (*SignupRequest, error) {
	email = strings.TrimSpace(email)
	orgName = strings.TrimSpace(orgName)
	plan = strings.TrimSpace(plan)

	if err := validateSignupInput(email, orgName, plan); err != nil {
		return nil, err
	}
	if s.stripe == nil {
		return nil, ErrCheckoutUnavailable
	}
	// Stripe's Checkout Session API requires absolute success_url /
	// cancel_url. Without a configured public base URL those would be
	// built as relative paths ("/signup?..."), which Stripe rejects with
	// a 400. Fail fast with a clear configuration error before persisting
	// a signup_requests row, rather than minting a doomed checkout and
	// leaving an orphaned `failed` row behind.
	if s.baseURL == "" {
		return nil, fmt.Errorf("%w: public base URL is not configured", ErrCheckoutUnavailable)
	}
	priceID := s.planPrices[plan]
	if priceID == "" {
		return nil, fmt.Errorf("%w: no Stripe price configured for plan %q", ErrCheckoutUnavailable, plan)
	}

	req, err := s.repo.Create(ctx, email, orgName, plan)
	if err != nil {
		s.metrics.failed()
		return nil, fmt.Errorf("signup: persist request: %w", err)
	}

	session, err := s.stripe.CreateCheckoutSession(ctx, CheckoutSessionParams{
		Plan:            plan,
		PriceID:         priceID,
		CustomerEmail:   email,
		SuccessURL:      s.successURL(req.ID),
		CancelURL:       s.cancelURL(req.ID),
		SignupRequestID: req.ID,
	})
	if err != nil {
		// The checkout couldn't be created — mark the row failed so a
		// status poll surfaces the dead end rather than hanging on
		// `pending` forever.
		if mErr := s.repo.MarkFailed(ctx, req.ID); mErr != nil {
			s.logger.Printf("signup: mark failed after checkout error (id=%s): %v", req.ID, mErr)
		}
		s.metrics.failed()
		return nil, fmt.Errorf("signup: create checkout session: %w", err)
	}

	if err := s.repo.SetCheckoutSession(ctx, req.ID, session.ID); err != nil {
		s.metrics.failed()
		return nil, fmt.Errorf("signup: record checkout session: %w", err)
	}

	req.StripeCheckoutSessionID = session.ID
	req.CheckoutURL = session.URL
	s.metrics.initiated(plan)
	return req, nil
}

// GetStatus returns the minimal public status view for a signup
// request. It projects the persisted row down to SignupStatusView so
// the unauthenticated polling endpoint never serves the collected PII
// (email, org_name) or the Stripe checkout session id.
func (s *SignupService) GetStatus(ctx context.Context, id string) (*SignupStatusView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	req, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &SignupStatusView{
		ID:          req.ID,
		Plan:        req.Plan,
		Status:      req.Status,
		CreatedAt:   req.CreatedAt,
		CompletedAt: req.CompletedAt,
	}, nil
}

// CompleteCheckoutSignup adapts CompleteSignup to the error-only shape
// the billing webhook's SignupCompleter interface expects.
//
// A checkout.session.completed event is delivered for EVERY Stripe
// Checkout Session on the account, not just ones this signup funnel
// minted. When the session id resolves no signup_requests row,
// CompleteSignup returns ErrSignupNotFound — that simply means the
// checkout belongs to some other flow, so it is a no-op here rather than
// an error. Swallowing it (using the sentinel directly, which only this
// package can reach) keeps the webhook from returning 500 and triggering
// Stripe's retry storm for events that were never ours to handle. Any
// other error is a genuine failure and propagates so Stripe redelivers.
func (s *SignupService) CompleteCheckoutSignup(ctx context.Context, stripeCheckoutSessionID string) error {
	_, err := s.CompleteSignup(ctx, stripeCheckoutSessionID)
	if errors.Is(err, ErrSignupNotFound) {
		return nil
	}
	return err
}

// CompleteSignup provisions the tenant for a paid checkout session. It
// is invoked from the Stripe `checkout.session.completed` webhook and
// is idempotent: a replayed event maps back to the same tenant.
//
// The deterministic per-signup slug is the idempotency anchor — the
// tenants.slug UNIQUE constraint makes a concurrent or replayed
// CreateTenant fail with ErrTenantExists, at which point we resolve
// and return the already-provisioned tenant instead of creating a
// second one. The remaining steps (admin user, checklist, status
// flip, welcome email) are each independently idempotent so a
// half-completed prior attempt is safely re-driven.
func (s *SignupService) CompleteSignup(ctx context.Context, stripeCheckoutSessionID string) (*Tenant, error) {
	stripeCheckoutSessionID = strings.TrimSpace(stripeCheckoutSessionID)
	if stripeCheckoutSessionID == "" {
		return nil, fmt.Errorf("%w: stripe checkout session id is required", ErrInvalidInput)
	}

	req, err := s.repo.GetByCheckoutSession(ctx, stripeCheckoutSessionID)
	if err != nil {
		return nil, err
	}

	slug := deterministicSlug(req.OrgName, req.ID)

	// Fast idempotent path: already completed.
	if req.Status == "active" {
		s.metrics.replayed()
		return s.prov.GetTenantBySlug(ctx, slug)
	}

	name := req.OrgName
	if name == "" {
		name = displayNameFromEmail(req.Email)
	}

	tenant, err := s.prov.CreateTenant(ctx, CreateTenantInput{
		Name: name,
		Slug: slug,
		Plan: req.Plan,
	})
	replay := false
	healProvisioning := false
	switch {
	case errors.Is(err, ErrTenantExists):
		// Concurrent / replayed completion — the tenant already
		// exists for this signup. Resolve it and continue driving the
		// remaining (idempotent) steps. Re-drive provisioning too, in
		// case a prior attempt inserted the row but failed a hook.
		replay = true
		healProvisioning = true
		tenant, err = s.prov.GetTenantBySlug(ctx, slug)
	case errors.Is(err, ErrTenantProvisionIncomplete):
		// The tenant row was inserted in THIS attempt but a post-insert
		// provisioning hook failed. The tenant pointer is valid; clear
		// the error and re-drive the idempotent hooks below so we don't
		// mark the signup active over a half-provisioned tenant.
		healProvisioning = true
		err = nil
	}
	if err != nil {
		s.failSignup(ctx, req.ID, "create tenant", err)
		return nil, fmt.Errorf("signup: create tenant: %w", err)
	}

	if healProvisioning {
		if err := s.prov.EnsureProvisioned(ctx, tenant.ID, tenant.Plan); err != nil {
			// Provisioning is still incomplete. Keep the signup in a
			// retryable state (return non-2xx → Stripe redelivers) so a
			// later webhook heals it, rather than marking it active over
			// a tenant whose zk-fabric bucket / billing record is missing.
			s.failSignup(ctx, req.ID, "ensure provisioned", err)
			return nil, fmt.Errorf("signup: ensure provisioned: %w", err)
		}
	}

	if err := s.prov.MarkSelfService(ctx, tenant.ID); err != nil {
		// Non-fatal: the tenant is provisioned; the self_service flag
		// is metadata. Log and continue.
		s.logger.Printf("signup: mark self_service (tenant=%s): %v", tenant.ID, err)
	}

	if _, err := s.prov.CreateAdminUser(ctx, tenant.ID, req.Email, displayNameFromEmail(req.Email)); err != nil {
		if !errors.Is(err, ErrUserExists) {
			s.failSignup(ctx, req.ID, "create admin user", err)
			return nil, fmt.Errorf("signup: create admin user: %w", err)
		}
	}

	if s.checklist != nil {
		if err := s.checklist.InitChecklist(ctx, tenant.ID); err != nil {
			// Best-effort: the checklist is computed lazily on first
			// load anyway, so a warm-up failure must not abort signup.
			s.logger.Printf("signup: init checklist (tenant=%s): %v", tenant.ID, err)
		}
	}

	if err := s.repo.MarkActive(ctx, req.ID, s.now().UTC()); err != nil {
		s.failSignup(ctx, req.ID, "mark active", err)
		return nil, fmt.Errorf("signup: mark active: %w", err)
	}

	s.auditProvisioned(ctx, tenant, req, replay)
	s.sendWelcome(ctx, tenant, req)

	if replay {
		s.metrics.replayed()
	} else {
		s.metrics.completed(req.Plan)
	}
	return tenant, nil
}

func (s *SignupService) failSignup(ctx context.Context, id, stage string, cause error) {
	s.logger.Printf("signup: %s failed (id=%s): %v", stage, id, cause)
	if err := s.repo.MarkFailed(ctx, id); err != nil {
		s.logger.Printf("signup: mark failed (id=%s): %v", id, err)
	}
	s.metrics.failed()
}

func (s *SignupService) auditProvisioned(ctx context.Context, t *Tenant, req *SignupRequest, replay bool) {
	if s.audit == nil {
		return
	}
	if _, err := s.audit.Log(ctx, audit.Entry{
		TenantID:     t.ID,
		ActorType:    audit.ActorSystem,
		Action:       "tenant.self_service_signup",
		ResourceType: "tenant",
		ResourceID:   t.ID,
		Metadata: map[string]any{
			"email":             req.Email,
			"plan":              req.Plan,
			"signup_request_id": req.ID,
			"checkout_session":  req.StripeCheckoutSessionID,
			"replay":            replay,
		},
	}); err != nil {
		s.logger.Printf("signup: audit log (tenant=%s): %v", t.ID, err)
	}
}

func (s *SignupService) sendWelcome(ctx context.Context, t *Tenant, req *SignupRequest) {
	if s.mailer == nil {
		return
	}
	msg := WelcomeMessage{
		To:       req.Email,
		OrgName:  t.Name,
		Plan:     t.Plan,
		TenantID: t.ID,
		LoginURL: s.loginURL(),
	}
	// Best-effort DNS guidance for a custom (non-freemail) sending
	// domain derived from the admin's email.
	if s.dns != nil {
		if domain := customDomainFromEmail(req.Email); domain != "" {
			recs := s.dns.GenerateRecords(domain)
			msg.DNSRecords = &recs
		}
	}
	if err := s.mailer.SendWelcome(ctx, msg); err != nil {
		s.logger.Printf("signup: send welcome email (tenant=%s): %v", t.ID, err)
	}
}

func (s *SignupService) successURL(id string) string {
	return fmt.Sprintf("%s/signup?status=success&id=%s&session_id={CHECKOUT_SESSION_ID}", s.baseURL, id)
}

func (s *SignupService) cancelURL(id string) string {
	return fmt.Sprintf("%s/signup?status=cancelled&id=%s", s.baseURL, id)
}

func (s *SignupService) loginURL() string {
	if s.baseURL == "" {
		return "/admin/dns-wizard"
	}
	return s.baseURL + "/admin/dns-wizard"
}

// --- validation + derivation helpers ---

func validateSignupInput(email, orgName, plan string) error {
	if email == "" {
		return fmt.Errorf("%w: email is required", ErrInvalidInput)
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return fmt.Errorf("%w: email is invalid", ErrInvalidInput)
	}
	if orgName == "" {
		return fmt.Errorf("%w: org_name is required", ErrInvalidInput)
	}
	if len(orgName) > 200 {
		return fmt.Errorf("%w: org_name is too long", ErrInvalidInput)
	}
	if _, ok := PlanByID(plan); !ok {
		return fmt.Errorf("%w: plan must be one of core, pro, privacy", ErrInvalidInput)
	}
	return nil
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases, replaces runs of non-alphanumerics with a
// single hyphen, and trims leading/trailing hyphens.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlugChars.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// deterministicSlug derives a stable, unique tenant slug from the org
// name and the signup request id. The request-id-derived suffix makes
// the slug both globally unique (two orgs named "Acme" don't collide)
// and deterministic per signup (a replayed completion derives the
// identical slug, so the tenants.slug UNIQUE constraint anchors
// idempotency).
func deterministicSlug(orgName, requestID string) string {
	base := slugify(orgName)
	if base == "" {
		base = "tenant"
	}
	sum := sha256.Sum256([]byte(requestID))
	suffix := hex.EncodeToString(sum[:])[:8]
	return base + "-" + suffix
}

// displayNameFromEmail derives a human-friendly display name from the
// local part of an email (e.g. "jane.doe@acme.com" -> "Jane Doe").
func displayNameFromEmail(email string) string {
	local := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		local = email[:at]
	}
	local = strings.NewReplacer(".", " ", "_", " ", "-", " ", "+", " ").Replace(local)
	fields := strings.Fields(local)
	for i, f := range fields {
		fields[i] = strings.ToUpper(f[:1]) + f[1:]
	}
	name := strings.Join(fields, " ")
	if name == "" {
		return email
	}
	return name
}

// freemailDomains are consumer mail providers for which we never
// suggest custom-domain DNS records in the welcome email.
var freemailDomains = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"outlook.com":    true,
	"hotmail.com":    true,
	"live.com":       true,
	"yahoo.com":      true,
	"icloud.com":     true,
	"me.com":         true,
	"proton.me":      true,
	"protonmail.com": true,
	"aol.com":        true,
	"gmx.com":        true,
}

// customDomainFromEmail returns the domain part of email when it looks
// like an organization-owned domain (i.e. not a known freemail
// provider), else "".
func customDomainFromEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.ToLower(email[at+1:])
	if freemailDomains[domain] {
		return ""
	}
	return domain
}
