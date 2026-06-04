package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/dns"
)

// --- in-memory fakes -----------------------------------------------

type fakeSignupRepo struct {
	mu      sync.Mutex
	seq     int
	byID    map[string]*SignupRequest
	bySess  map[string]string // session id -> request id
	failSet map[string]bool
}

func newFakeSignupRepo() *fakeSignupRepo {
	return &fakeSignupRepo{
		byID:    map[string]*SignupRequest{},
		bySess:  map[string]string{},
		failSet: map[string]bool{},
	}
}

func (r *fakeSignupRepo) Create(_ context.Context, email, orgName, plan string) (*SignupRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	id := fmt.Sprintf("req-%d", r.seq)
	req := &SignupRequest{
		ID:        id,
		Email:     email,
		OrgName:   orgName,
		Plan:      plan,
		Status:    "pending",
		CreatedAt: time.Now().UTC(),
	}
	// store a copy so callers mutating the returned pointer don't
	// corrupt repo state.
	cp := *req
	r.byID[id] = &cp
	return req, nil
}

func (r *fakeSignupRepo) GetByID(_ context.Context, id string) (*SignupRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[id]
	if !ok {
		return nil, ErrSignupNotFound
	}
	cp := *req
	return &cp, nil
}

func (r *fakeSignupRepo) GetByCheckoutSession(_ context.Context, sessionID string) (*SignupRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.bySess[sessionID]
	if !ok {
		return nil, ErrSignupNotFound
	}
	req := r.byID[id]
	cp := *req
	return &cp, nil
}

func (r *fakeSignupRepo) SetCheckoutSession(_ context.Context, id, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[id]
	if !ok {
		return ErrSignupNotFound
	}
	req.StripeCheckoutSessionID = sessionID
	r.bySess[sessionID] = id
	return nil
}

func (r *fakeSignupRepo) MarkActive(_ context.Context, id string, completedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[id]
	if !ok {
		return ErrSignupNotFound
	}
	if req.Status != "active" {
		req.Status = "active"
		t := completedAt
		req.CompletedAt = &t
	}
	return nil
}

func (r *fakeSignupRepo) MarkFailed(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req, ok := r.byID[id]; ok && req.Status == "pending" {
		req.Status = "failed"
	}
	return nil
}

type fakeStripe struct {
	mu       sync.Mutex
	calls    int
	lastParm CheckoutSessionParams
	err      error
}

func (f *fakeStripe) CreateCheckoutSession(_ context.Context, p CheckoutSessionParams) (*CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls++
	f.lastParm = p
	return &CheckoutSession{
		ID:  fmt.Sprintf("cs_test_%d", f.calls),
		URL: fmt.Sprintf("https://checkout.stripe.test/%d", f.calls),
	}, nil
}

type fakeProvisioner struct {
	mu              sync.Mutex
	bySlug          map[string]*Tenant
	createCalls     int
	userCalls       int
	ensureCalls     int
	seq             int
	forceExistsOnce bool // first CreateTenant returns ErrTenantExists
	usersByTenant   map[string]bool

	// partialOnce makes the first CreateTenant insert the tenant row but
	// return ErrTenantProvisionIncomplete (a non-nil tenant alongside the
	// error), modelling Service.CreateTenant's partial-success return
	// when a post-insert hook fails.
	partialOnce bool
	// ensureErrs is consumed one entry per EnsureProvisioned call; a
	// non-nil entry makes that call fail (modelling a still-failing
	// provisioning hook on retry).
	ensureErrs []error
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{
		bySlug:        map[string]*Tenant{},
		usersByTenant: map[string]bool{},
	}
}

func (p *fakeProvisioner) CreateTenant(_ context.Context, in CreateTenantInput) (*Tenant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createCalls++
	if p.forceExistsOnce {
		p.forceExistsOnce = false
		return nil, ErrTenantExists
	}
	if _, ok := p.bySlug[in.Slug]; ok {
		return nil, ErrTenantExists
	}
	p.seq++
	t := &Tenant{
		ID:     fmt.Sprintf("tenant-%d", p.seq),
		Name:   in.Name,
		Slug:   in.Slug,
		Plan:   in.Plan,
		Status: "active",
	}
	p.bySlug[in.Slug] = t
	if p.partialOnce {
		p.partialOnce = false
		// Row inserted, but a post-insert hook failed: return the tenant
		// AND the partial-provisioning signal, exactly like the
		// production signupProvisioner.CreateTenant does.
		return t, fmt.Errorf("%w: zk-fabric provision: boom", ErrTenantProvisionIncomplete)
	}
	return t, nil
}

func (p *fakeProvisioner) EnsureProvisioned(_ context.Context, _, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureCalls++
	if len(p.ensureErrs) > 0 {
		err := p.ensureErrs[0]
		p.ensureErrs = p.ensureErrs[1:]
		return err
	}
	return nil
}

func (p *fakeProvisioner) GetTenantBySlug(_ context.Context, slug string) (*Tenant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.bySlug[slug]
	if !ok {
		return nil, ErrNotFound
	}
	return t, nil
}

func (p *fakeProvisioner) MarkSelfService(_ context.Context, _ string) error { return nil }

func (p *fakeProvisioner) CreateAdminUser(_ context.Context, tenantID, email, _ string) (*User, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.userCalls++
	if p.usersByTenant[tenantID] {
		return nil, ErrUserExists
	}
	p.usersByTenant[tenantID] = true
	return &User{ID: "user-" + tenantID, TenantID: tenantID, Email: email, Role: "owner"}, nil
}

type fakeChecklist struct{ calls int }

func (f *fakeChecklist) InitChecklist(_ context.Context, _ string) error {
	f.calls++
	return nil
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (f *fakeAudit) Log(_ context.Context, e audit.Entry) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return &e, nil
}

type fakeMailer struct {
	mu   sync.Mutex
	sent []WelcomeMessage
}

func (f *fakeMailer) SendWelcome(_ context.Context, msg WelcomeMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

type fakeDNS struct{}

func (fakeDNS) GenerateRecords(domain string) dns.DomainRecords {
	return dns.DomainRecords{
		Domain:  domain,
		Records: []dns.DomainRecord{{Type: "MX", Name: domain, Value: "mx.kmail.test"}},
	}
}

// newTestService assembles a SignupService over the supplied fakes
// with all optional subsystems wired and a fixed clock.
func newTestService(repo SignupRepository, prov TenantProvisioner, stripe StripeCheckoutClient, opts ...func(*SignupConfig)) *SignupService {
	cfg := SignupConfig{
		Repo:          repo,
		Provisioner:   prov,
		Stripe:        stripe,
		PlanPrices:    map[string]string{"core": "price_core", "pro": "price_pro", "privacy": "price_privacy"},
		PublicBaseURL: "https://app.kmail.test",
		Now:           func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	for _, o := range opts {
		o(&cfg)
	}
	return NewSignupService(cfg)
}

// --- InitiateSignup ------------------------------------------------

func TestInitiateSignup_Valid(t *testing.T) {
	repo := newFakeSignupRepo()
	stripe := &fakeStripe{}
	svc := newTestService(repo, newFakeProvisioner(), stripe)

	req, err := svc.InitiateSignup(context.Background(), "founder@acme.com", "Acme Inc", "pro")
	if err != nil {
		t.Fatalf("InitiateSignup: %v", err)
	}
	if req.CheckoutURL == "" {
		t.Fatal("expected a checkout URL")
	}
	if req.StripeCheckoutSessionID == "" {
		t.Fatal("expected the checkout session id to be recorded")
	}
	if req.Status != "pending" {
		t.Fatalf("status = %q, want pending", req.Status)
	}
	if stripe.lastParm.PriceID != "price_pro" {
		t.Fatalf("price id = %q, want price_pro", stripe.lastParm.PriceID)
	}
	if stripe.lastParm.SignupRequestID != req.ID {
		t.Fatalf("client_reference id = %q, want %q", stripe.lastParm.SignupRequestID, req.ID)
	}
	// The recorded session id must be resolvable for the webhook.
	if _, err := repo.GetByCheckoutSession(context.Background(), req.StripeCheckoutSessionID); err != nil {
		t.Fatalf("session not indexed: %v", err)
	}
}

func TestInitiateSignup_InvalidInput(t *testing.T) {
	svc := newTestService(newFakeSignupRepo(), newFakeProvisioner(), &fakeStripe{})
	cases := []struct {
		name, email, org, plan string
	}{
		{"bad email", "not-an-email", "Acme", "core"},
		{"empty email", "", "Acme", "core"},
		{"empty org", "a@acme.com", "", "core"},
		{"unknown plan", "a@acme.com", "Acme", "enterprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.InitiateSignup(context.Background(), tc.email, tc.org, tc.plan)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestInitiateSignup_CheckoutUnavailable(t *testing.T) {
	// No Stripe client wired.
	svc := newTestService(newFakeSignupRepo(), newFakeProvisioner(), nil)
	_, err := svc.InitiateSignup(context.Background(), "a@acme.com", "Acme", "core")
	if !errors.Is(err, ErrCheckoutUnavailable) {
		t.Fatalf("err = %v, want ErrCheckoutUnavailable", err)
	}
}

func TestInitiateSignup_NoPriceForPlan(t *testing.T) {
	svc := newTestService(newFakeSignupRepo(), newFakeProvisioner(), &fakeStripe{}, func(c *SignupConfig) {
		c.PlanPrices = map[string]string{"core": "price_core"} // pro missing
	})
	_, err := svc.InitiateSignup(context.Background(), "a@acme.com", "Acme", "pro")
	if !errors.Is(err, ErrCheckoutUnavailable) {
		t.Fatalf("err = %v, want ErrCheckoutUnavailable", err)
	}
}

func TestInitiateSignup_MissingPublicBaseURL(t *testing.T) {
	// Without a public base URL the success/cancel URLs would be relative
	// paths Stripe rejects. InitiateSignup must fail fast before persisting
	// a row or calling Stripe.
	repo := newFakeSignupRepo()
	stripe := &fakeStripe{}
	svc := newTestService(repo, newFakeProvisioner(), stripe, func(c *SignupConfig) {
		c.PublicBaseURL = ""
	})

	_, err := svc.InitiateSignup(context.Background(), "a@acme.com", "Acme", "core")
	if !errors.Is(err, ErrCheckoutUnavailable) {
		t.Fatalf("err = %v, want ErrCheckoutUnavailable", err)
	}
	if stripe.calls != 0 {
		t.Fatalf("stripe calls = %d, want 0 (must fail before minting checkout)", stripe.calls)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.byID) != 0 {
		t.Fatalf("persisted %d signup rows, want 0 (must fail before persisting)", len(repo.byID))
	}
}

func TestInitiateSignup_CheckoutErrorMarksFailed(t *testing.T) {
	repo := newFakeSignupRepo()
	stripe := &fakeStripe{err: errors.New("stripe down")}
	svc := newTestService(repo, newFakeProvisioner(), stripe)

	_, err := svc.InitiateSignup(context.Background(), "a@acme.com", "Acme", "core")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The pending row should have been flipped to failed.
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, r := range repo.byID {
		if r.Status != "failed" {
			t.Fatalf("status = %q, want failed", r.Status)
		}
	}
}

// --- CompleteSignup ------------------------------------------------

// seedPending runs InitiateSignup and returns the resulting session id.
func seedPending(t *testing.T, svc *SignupService, repo *fakeSignupRepo, email, org, plan string) string {
	t.Helper()
	req, err := svc.InitiateSignup(context.Background(), email, org, plan)
	if err != nil {
		t.Fatalf("seed InitiateSignup: %v", err)
	}
	return req.StripeCheckoutSessionID
}

func TestCompleteSignup_ProvisionsTenant(t *testing.T) {
	repo := newFakeSignupRepo()
	prov := newFakeProvisioner()
	checklist := &fakeChecklist{}
	auditLog := &fakeAudit{}
	mailer := &fakeMailer{}
	svc := newTestService(repo, prov, &fakeStripe{}, func(c *SignupConfig) {
		c.Checklist = checklist
		c.Audit = auditLog
		c.Mailer = mailer
		c.DNS = fakeDNS{}
	})

	sess := seedPending(t, svc, repo, "founder@acme.com", "Acme Inc", "pro")
	tn, err := svc.CompleteSignup(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompleteSignup: %v", err)
	}
	if tn == nil || tn.ID == "" {
		t.Fatal("expected a provisioned tenant")
	}
	if prov.createCalls != 1 || prov.userCalls != 1 {
		t.Fatalf("createCalls=%d userCalls=%d, want 1/1", prov.createCalls, prov.userCalls)
	}
	if checklist.calls != 1 {
		t.Fatalf("checklist calls = %d, want 1", checklist.calls)
	}
	if len(auditLog.entries) != 1 || auditLog.entries[0].Action != "tenant.self_service_signup" {
		t.Fatalf("audit entries = %+v", auditLog.entries)
	}
	if auditLog.entries[0].ActorType != audit.ActorSystem {
		t.Fatalf("actor = %v, want system", auditLog.entries[0].ActorType)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("welcome emails = %d, want 1", len(mailer.sent))
	}
	if mailer.sent[0].DNSRecords == nil || mailer.sent[0].DNSRecords.Domain != "acme.com" {
		t.Fatalf("welcome DNS records = %+v, want acme.com", mailer.sent[0].DNSRecords)
	}
	// Signup row flipped active.
	got, _ := repo.GetByCheckoutSession(context.Background(), sess)
	if got.Status != "active" || got.CompletedAt == nil {
		t.Fatalf("row = %+v, want active+completed", got)
	}
}

func TestCompleteSignup_IdempotentReplay_FastPath(t *testing.T) {
	repo := newFakeSignupRepo()
	prov := newFakeProvisioner()
	mailer := &fakeMailer{}
	svc := newTestService(repo, prov, &fakeStripe{}, func(c *SignupConfig) {
		c.Mailer = mailer
	})

	sess := seedPending(t, svc, repo, "founder@acme.com", "Acme Inc", "core")
	first, err := svc.CompleteSignup(context.Background(), sess)
	if err != nil {
		t.Fatalf("first CompleteSignup: %v", err)
	}
	// Replay the same checkout session: must return the same tenant and
	// must NOT provision a second tenant or user.
	second, err := svc.CompleteSignup(context.Background(), sess)
	if err != nil {
		t.Fatalf("replay CompleteSignup: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay tenant id = %q, want %q", second.ID, first.ID)
	}
	if prov.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1 (replay must not re-provision)", prov.createCalls)
	}
	if prov.userCalls != 1 {
		t.Fatalf("userCalls = %d, want 1 (replay must not re-create admin)", prov.userCalls)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("welcome emails = %d, want 1 (no second email on replay)", len(mailer.sent))
	}
}

func TestCompleteSignup_ConcurrentCreate_ErrTenantExists(t *testing.T) {
	repo := newFakeSignupRepo()
	prov := newFakeProvisioner()
	// Pre-create the tenant under the deterministic slug so the first
	// CreateTenant in CompleteSignup collides, exercising the
	// ErrTenantExists -> GetTenantBySlug resolution branch.
	prov.forceExistsOnce = true
	svc := newTestService(repo, prov, &fakeStripe{})

	sess := seedPending(t, svc, repo, "founder@acme.com", "Acme Inc", "core")
	req, _ := repo.GetByCheckoutSession(context.Background(), sess)
	slug := deterministicSlug(req.OrgName, req.ID)
	// Seed the "already provisioned" tenant the resolution will find.
	prov.bySlug[slug] = &Tenant{ID: "tenant-existing", Name: "Acme Inc", Slug: slug, Plan: "core", Status: "active"}

	tn, err := svc.CompleteSignup(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompleteSignup: %v", err)
	}
	if tn.ID != "tenant-existing" {
		t.Fatalf("tenant id = %q, want tenant-existing (resolved by slug)", tn.ID)
	}
}

// TestCompleteSignup_PartialProvision_HealsSameAttempt covers the case
// where Service.CreateTenant inserts the tenant row but a post-insert
// provisioning hook fails (CreateTenant returns a non-nil tenant with
// ErrTenantProvisionIncomplete). CompleteSignup must NOT discard the
// tenant; it must re-drive the idempotent hooks via EnsureProvisioned
// and, on success, complete the signup.
func TestCompleteSignup_PartialProvision_HealsSameAttempt(t *testing.T) {
	repo := newFakeSignupRepo()
	prov := newFakeProvisioner()
	prov.partialOnce = true // first CreateTenant returns tenant + incomplete
	mailer := &fakeMailer{}
	svc := newTestService(repo, prov, &fakeStripe{}, func(c *SignupConfig) {
		c.Mailer = mailer
	})

	sess := seedPending(t, svc, repo, "founder@acme.com", "Acme Inc", "core")
	tn, err := svc.CompleteSignup(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompleteSignup: %v", err)
	}
	if tn == nil || tn.ID == "" {
		t.Fatal("expected a provisioned tenant, got nil/empty")
	}
	if prov.ensureCalls != 1 {
		t.Fatalf("ensureCalls = %d, want 1 (must heal the partial provisioning)", prov.ensureCalls)
	}
	if prov.userCalls != 1 {
		t.Fatalf("userCalls = %d, want 1 (admin user created after heal)", prov.userCalls)
	}
	req, _ := repo.GetByCheckoutSession(context.Background(), sess)
	if req.Status != "active" {
		t.Fatalf("signup status = %q, want active", req.Status)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("welcome emails = %d, want 1", len(mailer.sent))
	}
}

// TestCompleteSignup_PartialProvision_RetryHeals covers the webhook
// redelivery path: the first attempt inserts the tenant row but the
// provisioning heal still fails, so the signup is left retryable. The
// second delivery hits the slug collision (ErrTenantExists), resolves
// the tenant, re-drives the now-succeeding hooks, and completes —
// without ever creating a duplicate tenant.
func TestCompleteSignup_PartialProvision_RetryHeals(t *testing.T) {
	repo := newFakeSignupRepo()
	prov := newFakeProvisioner()
	prov.partialOnce = true
	prov.ensureErrs = []error{errors.New("zk-fabric still unavailable")}
	mailer := &fakeMailer{}
	svc := newTestService(repo, prov, &fakeStripe{}, func(c *SignupConfig) {
		c.Mailer = mailer
	})

	sess := seedPending(t, svc, repo, "founder@acme.com", "Acme Inc", "core")

	// First delivery: row inserted, heal fails -> signup not completed.
	if _, err := svc.CompleteSignup(context.Background(), sess); err == nil {
		t.Fatal("first CompleteSignup: expected error from failed provisioning heal")
	}
	req, _ := repo.GetByCheckoutSession(context.Background(), sess)
	if req.Status == "active" {
		t.Fatal("signup marked active despite incomplete provisioning")
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("welcome emails = %d, want 0 (signup not completed yet)", len(mailer.sent))
	}

	// Second delivery: slug collision -> resolve -> heal now succeeds.
	tn, err := svc.CompleteSignup(context.Background(), sess)
	if err != nil {
		t.Fatalf("retry CompleteSignup: %v", err)
	}
	if prov.createCalls != 2 {
		t.Fatalf("createCalls = %d, want 2", prov.createCalls)
	}
	if prov.ensureCalls != 2 {
		t.Fatalf("ensureCalls = %d, want 2 (heal attempted on both deliveries)", prov.ensureCalls)
	}
	// Exactly one tenant ever inserted under the deterministic slug.
	if len(prov.bySlug) != 1 {
		t.Fatalf("provisioned tenants = %d, want 1 (no duplicate)", len(prov.bySlug))
	}
	if tn.Slug != deterministicSlug(req.OrgName, req.ID) {
		t.Fatalf("tenant slug = %q, want deterministic slug", tn.Slug)
	}
	req, _ = repo.GetByCheckoutSession(context.Background(), sess)
	if req.Status != "active" {
		t.Fatalf("signup status = %q, want active after retry", req.Status)
	}
}

func TestCompleteSignup_UnknownSession(t *testing.T) {
	svc := newTestService(newFakeSignupRepo(), newFakeProvisioner(), &fakeStripe{})
	_, err := svc.CompleteSignup(context.Background(), "cs_missing")
	if !errors.Is(err, ErrSignupNotFound) {
		t.Fatalf("err = %v, want ErrSignupNotFound", err)
	}
}

// TestCompleteCheckoutSignup_UnknownSessionIsNoop guards the webhook
// adapter: a checkout.session.completed for a session this funnel never
// minted must NOT surface an error (which the billing webhook would turn
// into a 500, triggering Stripe to retry the delivery indefinitely). It
// is a no-op, while genuine session ids still provision normally.
func TestCompleteCheckoutSignup_UnknownSessionIsNoop(t *testing.T) {
	repo := newFakeSignupRepo()
	svc := newTestService(repo, newFakeProvisioner(), &fakeStripe{})

	if err := svc.CompleteCheckoutSignup(context.Background(), "cs_not_ours"); err != nil {
		t.Fatalf("unknown session: got err %v, want nil (no-op)", err)
	}

	// A real signup session still completes through the same adapter.
	sess := seedPending(t, svc, repo, "founder@acme.com", "Acme Inc", "pro")
	if err := svc.CompleteCheckoutSignup(context.Background(), sess); err != nil {
		t.Fatalf("known session: CompleteCheckoutSignup: %v", err)
	}
	got, _ := repo.GetByCheckoutSession(context.Background(), sess)
	if got.Status != "active" {
		t.Fatalf("status = %q, want active", got.Status)
	}
}

func TestDeterministicSlug_StableAndUnique(t *testing.T) {
	a1 := deterministicSlug("Acme Inc", "req-1")
	a2 := deterministicSlug("Acme Inc", "req-1")
	if a1 != a2 {
		t.Fatalf("slug not stable: %q vs %q", a1, a2)
	}
	// Same org name, different request -> different slug.
	b := deterministicSlug("Acme Inc", "req-2")
	if a1 == b {
		t.Fatalf("slug collision across requests: %q", a1)
	}
	// Empty org name still yields a valid slug.
	if got := deterministicSlug("", "req-1"); got == "" {
		t.Fatal("empty org name produced empty slug")
	}
}

func TestDisplayNameFromEmail(t *testing.T) {
	cases := map[string]string{
		"jane.doe@acme.com": "Jane Doe",
		"john@acme.com":     "John",
		"a_b-c@acme.com":    "A B C",
	}
	for in, want := range cases {
		if got := displayNameFromEmail(in); got != want {
			t.Errorf("displayNameFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCustomDomainFromEmail(t *testing.T) {
	if got := customDomainFromEmail("a@acme.com"); got != "acme.com" {
		t.Errorf("custom domain = %q, want acme.com", got)
	}
	if got := customDomainFromEmail("a@gmail.com"); got != "" {
		t.Errorf("freemail domain = %q, want empty", got)
	}
}
