// Package tenant hosts the Tenant Service business logic: the
// tenant lifecycle (create / suspend / delete / rename / rotate),
// user lifecycle, aliases, shared inboxes, and quotas.
//
// Authoritative for the control-plane Postgres schema defined in
// `docs/SCHEMA.md`. See `docs/ARCHITECTURE.md` §7 for the service
// topology.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// SeatAccounter is the narrow slice of the Billing Service the
// Tenant Service consumes to enforce seat limits and keep the
// `quotas.seat_count` counter in sync with user-row lifecycle
// events. Defined here to avoid a circular import back to
// `internal/billing`.
type SeatAccounter interface {
	CheckSeatAvailable(ctx context.Context, tenantID string) error
	IncrementSeatCount(ctx context.Context, tenantID string, delta int) error
}

// Service holds the Tenant Service dependencies and exposes the
// tenant / user / alias / shared-inbox lifecycle methods consumed by
// the HTTP handlers in this package.
type Service struct {
	pool          *pgxpool.Pool
	logger        *log.Logger
	seats         SeatAccounter
	provisioner   StorageProvisioner
	billing       BillingLifecycleHook
	sharedInboxFn SharedInboxMembershipHook
	aliasSync     StalwartAliasSync
}

// WithLogger returns a copy of the Service wired to the provided
// logger. Used by the alias Stalwart-sync inline attempt to record
// when sync deferred to the background worker. Falls back to the
// standard logger when not set.
func (s *Service) WithLogger(logger *log.Logger) *Service {
	cp := *s
	cp.logger = logger
	return &cp
}

// SharedInboxMembershipHook is invoked after a successful
// `AddSharedInboxMember` / `RemoveSharedInboxMember` write so the
// sharedinbox.WorkflowService can rotate the inbox's MLS group.
// Defined as a closure (not an interface) to keep the call site
// trivially stub-able in tests.
type SharedInboxMembershipHook func(ctx context.Context, tenantID, inboxID string, members []string, reason string)

// StorageProvisioner is the narrow slice of `ZKFabricProvisioner`
// the Tenant Service consumes on CreateTenant. Defined here so
// tests can substitute a stub without standing up a fake
// zk-object-fabric console.
type StorageProvisioner interface {
	Provision(ctx context.Context, tenantID, plan string) (*StorageCredential, error)
}

// BillingLifecycleHook is the narrow slice of the Billing Service's
// `Lifecycle` helper the Tenant Service calls on tenant create /
// delete. Defined here to break the circular import back to
// `internal/billing`.
type BillingLifecycleHook interface {
	OnTenantCreated(ctx context.Context, tenantID, plan string) error
	OnTenantDeleted(ctx context.Context, tenantID string) error
}

// NewService returns a Service wired to the provided pgx pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// WithSeatAccounter returns a copy of the Service wired to the
// provided accounter. The Tenant Service otherwise treats seats as
// unlimited.
func (s *Service) WithSeatAccounter(a SeatAccounter) *Service {
	cp := *s
	cp.seats = a
	return &cp
}

// WithStorageProvisioner returns a copy of the Service wired to a
// per-tenant zk-object-fabric provisioner. CreateTenant calls
// Provision after the DB insert; failures surface to the caller so
// half-provisioned tenants do not slip through silently.
func (s *Service) WithStorageProvisioner(p StorageProvisioner) *Service {
	cp := *s
	cp.provisioner = p
	return &cp
}

// WithBillingLifecycle returns a copy of the Service wired to the
// provided billing-lifecycle hook. CreateTenant / DeleteTenant call
// the hook after the DB mutation succeeds.
func (s *Service) WithBillingLifecycle(b BillingLifecycleHook) *Service {
	cp := *s
	cp.billing = b
	return &cp
}

// WithSharedInboxMembershipHook wires the post-mutation MLS-group
// rotation callback. Returning a copy keeps Service immutable.
func (s *Service) WithSharedInboxMembershipHook(fn SharedInboxMembershipHook) *Service {
	cp := *s
	cp.sharedInboxFn = fn
	return &cp
}

// Tenant is the API representation of a row in `tenants`.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Plan      string    `json:"plan"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User is the API representation of a row in `users`.
type User struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	KChatUserID       string    `json:"kchat_user_id"`
	StalwartAccountID string    `json:"stalwart_account_id"`
	Email             string    `json:"email"`
	DisplayName       string    `json:"display_name"`
	Role              string    `json:"role"`
	Status            string    `json:"status"`
	AccountType       string    `json:"account_type"`
	QuotaBytes        int64     `json:"quota_bytes"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Domain is the API representation of a row in `domains`.
type Domain struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	Domain         string    `json:"domain"`
	Verified       bool      `json:"verified"`
	MXVerified     bool      `json:"mx_verified"`
	SPFVerified    bool      `json:"spf_verified"`
	DKIMVerified   bool      `json:"dkim_verified"`
	DMARCVerified  bool      `json:"dmarc_verified"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SharedInbox is the API representation of a row in `shared_inboxes`.
type SharedInbox struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Address     string    `json:"address"`
	DisplayName string    `json:"display_name"`
	MLSGroupID  string    `json:"mls_group_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateTenantInput carries the fields accepted by POST /api/v1/tenants.
type CreateTenantInput struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
	Plan string `json:"plan"`
}

// EnsureTenantInput carries the fields for an idempotent,
// id-explicit tenant provision. Unlike CreateTenantInput (which
// lets Postgres generate the id), the caller supplies the tenant
// UUID so the KMail control-plane row shares its primary key with
// the identity iam-core puts in the `tenant_id` claim and webhook
// payloads. This 1:1 mapping is what lets webhook-driven and lazy
// provisioning converge on the same row and stay idempotent under
// iam-core's at-least-once delivery.
type EnsureTenantInput struct {
	// ID is the tenant UUID (iam-core's tenant identifier). Required
	// and must be a valid UUID — it is inserted into the `uuid`
	// primary key.
	ID string `json:"id"`
	// Name / Slug / Plan seed a freshly-provisioned row. Empty
	// fields are defaulted (Name/Slug from ID, Plan to "core") so
	// lazy provisioning — which only knows the id — can still
	// satisfy the NOT NULL columns.
	Name string `json:"name"`
	Slug string `json:"slug"`
	Plan string `json:"plan"`
	// EnsureProvisioned, when true, makes EnsureTenant guarantee the
	// idempotent post-insert hooks (zk-fabric bucket + billing
	// lifecycle) have run even when the row already exists — not just
	// on the insert that creates it. This closes the
	// partial-provisioning gap where a prior call inserted the row
	// but then failed a hook: a plain idempotent retry would hit the
	// existing-row fast path and return success without ever
	// re-running the hooks, leaving the tenant permanently without
	// storage / billing. The authoritative tenant.create webhook sets
	// this so its at-least-once redelivery converges on a fully
	// provisioned tenant; the lazy-provisioning hot path leaves it
	// false to stay off the zk-fabric/billing network path on every
	// authenticated request.
	EnsureProvisioned bool `json:"-"`
}

// CreateUserInput carries the fields accepted by
// POST /api/v1/tenants/:id/users.
type CreateUserInput struct {
	KChatUserID       string `json:"kchat_user_id"`
	StalwartAccountID string `json:"stalwart_account_id"`
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	Role              string `json:"role"`
	QuotaBytes        int64  `json:"quota_bytes"`

	// AccountType defaults to "user" when empty. Set to
	// "shared_inbox" or "service" to create a non-seat account
	// (shared inboxes, automation endpoints) that the billing
	// Service excludes from CountSeats.
	AccountType string `json:"account_type,omitempty"`
}

// CreateDomainInput carries the fields accepted by
// POST /api/v1/tenants/:id/domains.
type CreateDomainInput struct {
	Domain string `json:"domain"`
}

// UpdateTenantInput carries the fields accepted by
// PUT /api/v1/tenants/:id. Fields are pointers so callers can
// distinguish "unset" from "set to zero value" and only the provided
// fields are updated.
type UpdateTenantInput struct {
	Name   *string `json:"name,omitempty"`
	Plan   *string `json:"plan,omitempty"`
	Status *string `json:"status,omitempty"`
}

// UpdateUserInput carries the fields accepted by
// PUT /api/v1/tenants/:id/users/:userId. Fields are pointers so
// callers can omit fields they do not want to change.
type UpdateUserInput struct {
	DisplayName *string `json:"display_name,omitempty"`
	Role        *string `json:"role,omitempty"`
	Status      *string `json:"status,omitempty"`
	QuotaBytes  *int64  `json:"quota_bytes,omitempty"`
}

// CreateSharedInboxInput carries the fields accepted by
// POST /api/v1/tenants/:id/shared-inboxes.
type CreateSharedInboxInput struct {
	Address     string `json:"address"`
	DisplayName string `json:"display_name"`
	MLSGroupID  string `json:"mls_group_id"`
}

// AddSharedInboxMemberInput carries the fields accepted by
// POST /api/v1/tenants/:id/shared-inboxes/:inboxId/members.
type AddSharedInboxMemberInput struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// SharedInboxMember is the API representation of a row in
// `shared_inbox_members`.
type SharedInboxMember struct {
	TenantID      string    `json:"tenant_id"`
	SharedInboxID string    `json:"shared_inbox_id"`
	UserID        string    `json:"user_id"`
	Role          string    `json:"role"`
	AddedAt       time.Time `json:"added_at"`
}

// ErrNotFound is returned when a lookup resolves no rows.
var ErrNotFound = errors.New("not found")

// ErrInvalidInput wraps caller-visible validation failures (missing
// or malformed fields on a Create* input). Handlers surface this as
// HTTP 400; every other Service error is treated as 500.
var ErrInvalidInput = errors.New("invalid input")

// CreateTenant inserts a new tenant row and returns the persisted
// representation. Tenants are the only entity not subject to RLS —
// creating one is a control-plane-admin operation, not a
// tenant-scoped one.
func (s *Service) CreateTenant(ctx context.Context, in CreateTenantInput) (*Tenant, error) {
	if in.Name == "" || in.Slug == "" || in.Plan == "" {
		return nil, fmt.Errorf("%w: name, slug, and plan are required", ErrInvalidInput)
	}
	var t Tenant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan)
		VALUES ($1, $2, $3)
		RETURNING id::text, name, slug, plan, status, created_at, updated_at
	`, in.Name, in.Slug, in.Plan).Scan(
		&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert tenant: %w", err)
	}
	// Best-effort post-insert hooks. We surface errors to the caller
	// so a half-provisioned tenant doesn't slip into the control
	// plane silently — operators can re-run provisioning idempotently
	// (CreateBucket / placement PUT both no-op when the resource
	// already exists for the same tenant).
	if s.provisioner != nil {
		if _, err := s.provisioner.Provision(ctx, t.ID, t.Plan); err != nil {
			return &t, fmt.Errorf("zk-fabric provision: %w", err)
		}
	}
	if s.billing != nil {
		if err := s.billing.OnTenantCreated(ctx, t.ID, t.Plan); err != nil {
			return &t, fmt.Errorf("billing.OnTenantCreated: %w", err)
		}
	}
	return &t, nil
}

// EnsureTenant idempotently provisions the control-plane tenant
// identified by in.ID. It is the id-explicit, redelivery-safe
// sibling of CreateTenant used by the iam-core integration
// (webhook tenant.create and the lazy-provisioning middleware),
// where the tenant UUID is dictated by iam-core rather than
// generated by KMail.
//
// Semantics:
//   - when the row already exists it reconciles any authoritative
//     metadata the caller supplied (see reconcileTenantMetadata) and
//     returns (row, false, nil);
//   - inserts and returns (new, true, nil) when it did not, running
//     the same zk-fabric + billing post-insert hooks as CreateTenant;
//   - is safe under concurrent callers (lazy provisioning racing a
//     webhook): the insert uses ON CONFLICT (id) DO NOTHING and a
//     lost race re-reads the winner's row.
//
// Callers that only know the tenant id (the lazy-provisioning
// middleware) pass empty Name/Slug/Plan and never mutate an
// existing row; the tenant.create webhook passes the authoritative
// iam-core values, so a placeholder row created by an earlier lazy
// provision is reconciled to the real slug/name/plan rather than
// keeping its UUID-derived placeholders forever.
//
// Post-insert hooks (zk-fabric bucket + billing) always run on the
// insert that creates the row. They are NOT re-run on the
// existing-row fast path unless in.EnsureProvisioned is set — the
// webhook sets it so a retry after a hook failed the first time
// re-drives the (idempotent) hooks and converges on a fully
// provisioned tenant, while the lazy hot path leaves it false to
// avoid a zk-fabric/billing round-trip on every authenticated
// request.
//
// Like CreateTenant this is a control-plane-admin operation and is
// not subject to RLS (the `tenants` table has no RLS policy).
func (s *Service) EnsureTenant(ctx context.Context, in EnsureTenantInput) (*Tenant, bool, error) {
	if _, err := uuid.Parse(in.ID); err != nil {
		return nil, false, fmt.Errorf("%w: tenant id must be a valid UUID", ErrInvalidInput)
	}
	// Fast path: most authenticated requests hit an already
	// provisioned tenant, so a single SELECT avoids the write-path
	// entirely (and avoids burning a sequence / WAL on a no-op
	// insert under the lazy-provisioning hot path).
	if existing, err := s.GetTenant(ctx, in.ID); err == nil {
		return s.reconcileExisting(ctx, existing, in)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	name := firstNonEmptyTenantField(in.Name, in.Slug, in.ID)
	slug := firstNonEmptyTenantField(in.Slug, in.ID)
	plan := in.Plan
	if plan == "" {
		plan = "core"
	}

	var t Tenant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tenants (id, name, slug, plan)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING
		RETURNING id::text, name, slug, plan, status, created_at, updated_at
	`, in.ID, name, slug, plan).Scan(
		&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another caller won the race between our GetTenant and this
		// INSERT. Re-read so we return the persisted row rather than
		// reporting a spurious failure, then reconcile in case the
		// winner was a placeholder lazy provision and we carry the
		// authoritative webhook metadata.
		existing, getErr := s.GetTenant(ctx, in.ID)
		if getErr != nil {
			return nil, false, fmt.Errorf("ensure tenant: lost insert race and re-read failed: %w", getErr)
		}
		return s.reconcileExisting(ctx, existing, in)
	}
	if err != nil {
		return nil, false, fmt.Errorf("ensure tenant: %w", err)
	}

	// Post-insert hooks mirror CreateTenant exactly. They always run
	// on the insert that creates the row; on failure the
	// partially-provisioned tenant is returned so the caller can
	// retry (the webhook sets EnsureProvisioned so that retry
	// re-drives the idempotent hooks via reconcileExisting).
	if err := s.runProvisioningHooks(ctx, t.ID, t.Plan); err != nil {
		return &t, true, err
	}
	return &t, true, nil
}

// reconcileExisting handles the existing-row outcome shared by the
// fast path and the lost-insert-race re-read: it reconciles any
// authoritative metadata the caller supplied and, when
// in.EnsureProvisioned is set, re-runs the idempotent post-insert
// hooks so a tenant whose original insert failed a hook converges on
// full provisioning. It always reports created=false.
func (s *Service) reconcileExisting(ctx context.Context, existing *Tenant, in EnsureTenantInput) (*Tenant, bool, error) {
	reconciled, err := s.reconcileTenantMetadata(ctx, existing, in)
	if err != nil {
		return reconciled, false, err
	}
	if in.EnsureProvisioned {
		if err := s.runProvisioningHooks(ctx, reconciled.ID, reconciled.Plan); err != nil {
			return reconciled, false, err
		}
	}
	return reconciled, false, nil
}

// runProvisioningHooks executes the idempotent post-insert
// provisioning hooks (zk-object-fabric bucket/placement and billing
// lifecycle) for a tenant. It mirrors the hook sequence in
// CreateTenant / EnsureProvisioned and is safe to call repeatedly:
// CreateBucket / placement PUT and the billing OnTenantCreated upsert
// all no-op when the resource already exists.
func (s *Service) runProvisioningHooks(ctx context.Context, tenantID, plan string) error {
	if s.provisioner != nil {
		if _, err := s.provisioner.Provision(ctx, tenantID, plan); err != nil {
			return fmt.Errorf("zk-fabric provision: %w", err)
		}
	}
	if s.billing != nil {
		if err := s.billing.OnTenantCreated(ctx, tenantID, plan); err != nil {
			return fmt.Errorf("billing.OnTenantCreated: %w", err)
		}
	}
	return nil
}

// LazyProvisioner adapts the Service onto the narrow
// middleware.TenantProvisioner interface the iam-core lazy
// provisioning middleware consumes. That middleware only knows the
// tenant id (from the validated token), so the adapter calls
// EnsureTenant with id-only input and discards the (tenant,
// created) detail it does not need. Defined here — rather than in
// cmd/kmail-api — so the adapter lives next to the method it wraps
// and is covered by the tenant package's tests.
func (s *Service) LazyProvisioner() middleware.TenantProvisioner {
	return lazyProvisioner{svc: s}
}

// reconcileTenantMetadata brings an already-provisioned control-plane
// row in line with authoritative metadata the caller supplied. Only
// non-empty input fields that actually differ from the persisted row
// are written, so:
//   - lazy provisioning (id-only input) is always a no-op — no UPDATE
//     is issued, keeping it off the request hot path's write budget;
//   - a redelivered tenant.create webhook carrying unchanged values
//     is also a no-op;
//   - a tenant.create webhook arriving after a lazily-provisioned
//     placeholder row updates the placeholder slug/name/plan to the
//     real iam-core values.
//
// A slug update can still collide with another tenant's slug
// (slug is UNIQUE); that surfaces as an error the webhook turns into
// a retryable 500, matching the insert path's behaviour.
func (s *Service) reconcileTenantMetadata(ctx context.Context, existing *Tenant, in EnsureTenantInput) (*Tenant, error) {
	var sets []string
	var args []any
	appendSet := func(col, val string) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if name := strings.TrimSpace(in.Name); name != "" && name != existing.Name {
		appendSet("name", name)
	}
	if slug := strings.TrimSpace(in.Slug); slug != "" && slug != existing.Slug {
		appendSet("slug", slug)
	}
	if plan := strings.TrimSpace(in.Plan); plan != "" && plan != existing.Plan {
		appendSet("plan", plan)
	}
	if len(sets) == 0 {
		return existing, nil
	}
	args = append(args, existing.ID)
	query := fmt.Sprintf(`
		UPDATE tenants
		SET %s, updated_at = now()
		WHERE id = $%d::uuid
		RETURNING id::text, name, slug, plan, status, created_at, updated_at
	`, strings.Join(sets, ", "), len(args))
	var t Tenant
	if err := s.pool.QueryRow(ctx, query, args...).Scan(
		&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("reconcile tenant metadata: %w", err)
	}
	return &t, nil
}

type lazyProvisioner struct{ svc *Service }

func (p lazyProvisioner) EnsureTenant(ctx context.Context, tenantID string) error {
	_, _, err := p.svc.EnsureTenant(ctx, EnsureTenantInput{ID: tenantID})
	return err
}

// firstNonEmptyTenantField returns the first non-empty (trimmed)
// argument, or "" if all are empty. Used by EnsureTenant to derive
// the NOT NULL name/slug columns from whichever fields the caller
// supplied.
func firstNonEmptyTenantField(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ListTenants returns every tenant in the control plane, ordered by
// creation time. This is an admin-only operation that intentionally
// bypasses RLS — callers are expected to gate it behind the
// control-plane admin role in the BFF.
func (s *Service) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, slug, plan, status, created_at, updated_at
		FROM tenants
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(
			&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tenants: %w", err)
	}
	return out, nil
}

// GetTenant fetches a tenant by ID.
func (s *Service) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	var t Tenant
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, slug, plan, status, created_at, updated_at
		FROM tenants
		WHERE id = $1::uuid
	`, id).Scan(&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select tenant: %w", err)
	}
	return &t, nil
}

// CreateUser inserts a new user inside the given tenant. It runs
// inside a transaction with `app.tenant_id` set to `tenantID` so the
// RLS policy on `users` validates the insert. When a SeatAccounter
// is wired, new `user` account-type rows are rejected if the
// tenant's seat pool is full (ErrQuotaExceeded surfaced by the
// billing service) and the pool counter is incremented on success.
// Shared-inbox / service account types do not consume seats and
// skip the accounter check.
func (s *Service) CreateUser(ctx context.Context, tenantID string, in CreateUserInput) (*User, error) {
	if in.KChatUserID == "" || in.StalwartAccountID == "" || in.Email == "" || in.DisplayName == "" {
		return nil, fmt.Errorf("%w: kchat_user_id, stalwart_account_id, email, and display_name are required", ErrInvalidInput)
	}
	role := in.Role
	if role == "" {
		role = "member"
	}
	accountType := in.AccountType
	if accountType == "" {
		accountType = "user"
	}
	switch accountType {
	case "user", "shared_inbox", "service":
	default:
		return nil, fmt.Errorf("%w: account_type must be one of user, shared_inbox, service", ErrInvalidInput)
	}
	if accountType == "user" && s.seats != nil {
		if err := s.seats.CheckSeatAvailable(ctx, tenantID); err != nil {
			return nil, err
		}
	}
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO users (
				tenant_id, kchat_user_id, stalwart_account_id, email,
				display_name, role, quota_bytes, account_type
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id::text, tenant_id::text, kchat_user_id,
			          stalwart_account_id, email, display_name, role,
			          status, account_type, quota_bytes, created_at, updated_at
		`,
			tenantID, in.KChatUserID, in.StalwartAccountID, in.Email,
			in.DisplayName, role, in.QuotaBytes, accountType,
		).Scan(
			&u.ID, &u.TenantID, &u.KChatUserID, &u.StalwartAccountID,
			&u.Email, &u.DisplayName, &u.Role, &u.Status,
			&u.AccountType, &u.QuotaBytes,
			&u.CreatedAt, &u.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	if u.AccountType == "user" && s.seats != nil {
		if err := s.seats.IncrementSeatCount(ctx, tenantID, 1); err != nil {
			// Log-and-continue: the INSERT succeeded, so surfacing
			// the counter-update failure as an error to the caller
			// would hide the fact that the seat was taken. The
			// quota worker reconciles drift on its next tick.
			return &u, nil
		}
	}
	return &u, nil
}

// CreateDomain adds a domain to a tenant. Verification flags start
// false and are flipped by the DNS Onboarding Service as each DNS
// record is observed — see `docs/ARCHITECTURE.md` §7.
func (s *Service) CreateDomain(ctx context.Context, tenantID string, in CreateDomainInput) (*Domain, error) {
	if in.Domain == "" {
		return nil, fmt.Errorf("%w: domain is required", ErrInvalidInput)
	}
	var d Domain
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO domains (tenant_id, domain)
			VALUES ($1::uuid, $2)
			RETURNING id::text, tenant_id::text, domain, verified,
			          mx_verified, spf_verified, dkim_verified,
			          dmarc_verified, created_at, updated_at
		`, tenantID, in.Domain).Scan(
			&d.ID, &d.TenantID, &d.Domain, &d.Verified,
			&d.MXVerified, &d.SPFVerified, &d.DKIMVerified,
			&d.DMARCVerified, &d.CreatedAt, &d.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert domain: %w", err)
	}
	return &d, nil
}

// ListDomains returns every domain owned by a tenant.
func (s *Service) ListDomains(ctx context.Context, tenantID string) ([]Domain, error) {
	var out []Domain
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, domain, verified,
			       mx_verified, spf_verified, dkim_verified,
			       dmarc_verified, created_at, updated_at
			FROM domains
			WHERE tenant_id = $1::uuid
			ORDER BY created_at ASC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Domain
			if err := rows.Scan(
				&d.ID, &d.TenantID, &d.Domain, &d.Verified,
				&d.MXVerified, &d.SPFVerified, &d.DKIMVerified,
				&d.DMARCVerified, &d.CreatedAt, &d.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	return out, nil
}

// UpdateTenant updates mutable tenant fields (name, plan, status)
// and returns the persisted row. Nil fields on the input are left
// unchanged. Like CreateTenant and ListTenants this is an admin
// operation that does not run through tenant-scoped RLS.
func (s *Service) UpdateTenant(ctx context.Context, id string, in UpdateTenantInput) (*Tenant, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	if in.Name == nil && in.Plan == nil && in.Status == nil {
		return nil, fmt.Errorf("%w: no fields to update", ErrInvalidInput)
	}
	if in.Name != nil && *in.Name == "" {
		return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidInput)
	}
	if in.Plan != nil {
		switch *in.Plan {
		case "core", "pro", "privacy":
		default:
			return nil, fmt.Errorf("%w: plan must be one of core, pro, privacy", ErrInvalidInput)
		}
	}
	if in.Status != nil {
		switch *in.Status {
		case "active", "suspended", "deleted":
		default:
			return nil, fmt.Errorf("%w: status must be one of active, suspended, deleted", ErrInvalidInput)
		}
	}
	var t Tenant
	err := s.pool.QueryRow(ctx, `
		UPDATE tenants
		SET name   = COALESCE($2, name),
		    plan   = COALESCE($3, plan),
		    status = COALESCE($4, status)
		WHERE id = $1::uuid
		RETURNING id::text, name, slug, plan, status, created_at, updated_at
	`, id, in.Name, in.Plan, in.Status).Scan(
		&t.ID, &t.Name, &t.Slug, &t.Plan, &t.Status,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update tenant: %w", err)
	}
	return &t, nil
}

// DeleteTenant soft-deletes a tenant by flipping its status to
// "deleted". We never purge the row — downstream RLS-scoped tables
// still reference it and audit retention applies.
func (s *Service) DeleteTenant(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenants
		SET status = 'deleted'
		WHERE id = $1::uuid
	`, id)
	if err != nil {
		return fmt.Errorf("delete tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if s.billing != nil {
		if err := s.billing.OnTenantDeleted(ctx, id); err != nil {
			return fmt.Errorf("billing.OnTenantDeleted: %w", err)
		}
	}
	return nil
}

// ListUsers returns every user in a tenant, scoped by RLS.
func (s *Service) ListUsers(ctx context.Context, tenantID string) ([]User, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	var out []User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email, display_name, role,
			       status, account_type, quota_bytes, created_at, updated_at
			FROM users
			WHERE tenant_id = $1::uuid
			ORDER BY created_at ASC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u User
			if err := rows.Scan(
				&u.ID, &u.TenantID, &u.KChatUserID, &u.StalwartAccountID,
				&u.Email, &u.DisplayName, &u.Role, &u.Status, &u.AccountType,
				&u.QuotaBytes, &u.CreatedAt, &u.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return out, nil
}

// GetUser fetches a single user inside a tenant. RLS scopes the
// lookup so cross-tenant probes return ErrNotFound.
func (s *Service) GetUser(ctx context.Context, tenantID, userID string) (*User, error) {
	if tenantID == "" || userID == "" {
		return nil, fmt.Errorf("%w: tenant id and user id are required", ErrInvalidInput)
	}
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email, display_name, role,
			       status, account_type, quota_bytes, created_at, updated_at
			FROM users
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, userID, tenantID).Scan(
			&u.ID, &u.TenantID, &u.KChatUserID, &u.StalwartAccountID,
			&u.Email, &u.DisplayName, &u.Role, &u.Status, &u.AccountType,
			&u.QuotaBytes, &u.CreatedAt, &u.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select user: %w", err)
	}
	return &u, nil
}

// GetUserByKChatID fetches a user by its external identity key
// (`kchat_user_id`) inside a tenant. KChat OIDC tokens carry this
// as `kchat_user_id`; iam-core tokens carry it as `sub`. The
// iam-core webhook receiver uses this to resolve the KMail-internal
// user UUID from the iam-core user id on user.update / user.delete
// events, since those tenant.Service methods key on the internal
// UUID. Runs inside the tenant-scoped GUC so RLS enforces the
// tenant boundary on the lookup.
func (s *Service) GetUserByKChatID(ctx context.Context, tenantID, kchatUserID string) (*User, error) {
	if tenantID == "" || kchatUserID == "" {
		return nil, fmt.Errorf("%w: tenant id and kchat_user_id are required", ErrInvalidInput)
	}
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, kchat_user_id,
			       stalwart_account_id, email, display_name, role,
			       status, account_type, quota_bytes, created_at, updated_at
			FROM users
			WHERE tenant_id = $1::uuid AND kchat_user_id = $2
		`, tenantID, kchatUserID).Scan(
			&u.ID, &u.TenantID, &u.KChatUserID, &u.StalwartAccountID,
			&u.Email, &u.DisplayName, &u.Role, &u.Status, &u.AccountType,
			&u.QuotaBytes, &u.CreatedAt, &u.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select user by kchat_user_id: %w", err)
	}
	return &u, nil
}

// UpdateUser updates mutable user fields (display_name, role,
// status, quota_bytes). Nil fields on the input are left unchanged.
// Runs inside the tenant-scoped GUC so RLS enforces tenant
// boundaries on the update.
func (s *Service) UpdateUser(ctx context.Context, tenantID, userID string, in UpdateUserInput) (*User, error) {
	if tenantID == "" || userID == "" {
		return nil, fmt.Errorf("%w: tenant id and user id are required", ErrInvalidInput)
	}
	if in.DisplayName == nil && in.Role == nil && in.Status == nil && in.QuotaBytes == nil {
		return nil, fmt.Errorf("%w: no fields to update", ErrInvalidInput)
	}
	if in.DisplayName != nil && *in.DisplayName == "" {
		return nil, fmt.Errorf("%w: display_name cannot be empty", ErrInvalidInput)
	}
	if in.Role != nil {
		switch *in.Role {
		case "owner", "admin", "member", "billing", "deliverability":
		default:
			return nil, fmt.Errorf("%w: role must be one of owner, admin, member, billing, deliverability", ErrInvalidInput)
		}
	}
	if in.Status != nil {
		switch *in.Status {
		case "active", "suspended", "deleted":
		default:
			return nil, fmt.Errorf("%w: status must be one of active, suspended, deleted", ErrInvalidInput)
		}
	}
	if in.QuotaBytes != nil && *in.QuotaBytes < 0 {
		return nil, fmt.Errorf("%w: quota_bytes must be non-negative", ErrInvalidInput)
	}
	var u User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			UPDATE users
			SET display_name = COALESCE($3, display_name),
			    role         = COALESCE($4, role),
			    status       = COALESCE($5, status),
			    quota_bytes  = COALESCE($6, quota_bytes)
			WHERE id = $1::uuid AND tenant_id = $2::uuid
			RETURNING id::text, tenant_id::text, kchat_user_id,
			          stalwart_account_id, email, display_name, role,
			          status, account_type, quota_bytes, created_at, updated_at
		`, userID, tenantID, in.DisplayName, in.Role, in.Status, in.QuotaBytes).Scan(
			&u.ID, &u.TenantID, &u.KChatUserID, &u.StalwartAccountID,
			&u.Email, &u.DisplayName, &u.Role, &u.Status, &u.AccountType,
			&u.QuotaBytes, &u.CreatedAt, &u.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	return &u, nil
}

// DeleteUser soft-deletes a user by flipping status to "deleted".
// The row stays in place so audit trails and RLS references remain
// valid. When the deleted row is a paid seat (account_type=user)
// the seat counter is decremented on the billing service.
func (s *Service) DeleteUser(ctx context.Context, tenantID, userID string) error {
	if tenantID == "" || userID == "" {
		return fmt.Errorf("%w: tenant id and user id are required", ErrInvalidInput)
	}
	var affected int64
	var accountType string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			UPDATE users
			SET status = 'deleted'
			WHERE id = $1::uuid AND tenant_id = $2::uuid
			  AND status <> 'deleted'
			RETURNING account_type
		`, userID, tenantID).Scan(&accountType)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		affected = 1
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if accountType == "user" && s.seats != nil {
		_ = s.seats.IncrementSeatCount(ctx, tenantID, -1)
	}
	return nil
}

// GetDomain fetches a single domain inside a tenant. Used by the
// DNS verification handler to look up records before running checks.
func (s *Service) GetDomain(ctx context.Context, tenantID, domainID string) (*Domain, error) {
	if tenantID == "" || domainID == "" {
		return nil, fmt.Errorf("%w: tenant id and domain id are required", ErrInvalidInput)
	}
	var d Domain
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, domain, verified,
			       mx_verified, spf_verified, dkim_verified,
			       dmarc_verified, created_at, updated_at
			FROM domains
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, domainID, tenantID).Scan(
			&d.ID, &d.TenantID, &d.Domain, &d.Verified,
			&d.MXVerified, &d.SPFVerified, &d.DKIMVerified,
			&d.DMARCVerified, &d.CreatedAt, &d.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select domain: %w", err)
	}
	return &d, nil
}

// ListSharedInboxes returns every shared inbox owned by the tenant,
// RLS-scoped via the `app.tenant_id` GUC.
func (s *Service) ListSharedInboxes(ctx context.Context, tenantID string) ([]SharedInbox, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	var out []SharedInbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, address, display_name,
			       mls_group_id, created_at, updated_at
			FROM shared_inboxes
			WHERE tenant_id = $1::uuid
			ORDER BY created_at ASC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var si SharedInbox
			if err := rows.Scan(
				&si.ID, &si.TenantID, &si.Address, &si.DisplayName,
				&si.MLSGroupID, &si.CreatedAt, &si.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, si)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list shared inboxes: %w", err)
	}
	return out, nil
}

// AddSharedInboxMember adds a user to a shared inbox with the given
// role. Both the inbox and the user must already exist inside the
// tenant — RLS scopes the insert so cross-tenant references cannot
// be created even if the IDs collide.
func (s *Service) AddSharedInboxMember(ctx context.Context, tenantID, inboxID, userID, role string) (*SharedInboxMember, error) {
	if tenantID == "" || inboxID == "" || userID == "" {
		return nil, fmt.Errorf("%w: tenant id, inbox id, and user id are required", ErrInvalidInput)
	}
	if role == "" {
		role = "member"
	}
	switch role {
	case "owner", "member", "viewer":
	default:
		return nil, fmt.Errorf("%w: role must be one of owner, member, viewer", ErrInvalidInput)
	}
	m := SharedInboxMember{
		TenantID:      tenantID,
		SharedInboxID: inboxID,
		UserID:        userID,
		Role:          role,
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO shared_inbox_members (
				tenant_id, shared_inbox_id, user_id, role
			) VALUES ($1::uuid, $2::uuid, $3::uuid, $4)
			RETURNING added_at
		`, tenantID, inboxID, userID, role).Scan(&m.AddedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insert shared inbox member: %w", err)
	}
	if s.sharedInboxFn != nil {
		members, _ := s.listSharedInboxMemberIDs(ctx, tenantID, inboxID)
		s.sharedInboxFn(ctx, tenantID, inboxID, members, "member_added")
	}
	return &m, nil
}

// listSharedInboxMemberIDs returns the current member set after a
// mutation, used to feed MLS rotation. Best-effort — failures are
// logged at the call-site.
func (s *Service) listSharedInboxMemberIDs(ctx context.Context, tenantID, inboxID string) ([]string, error) {
	var ids []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT user_id::text FROM shared_inbox_members
			WHERE tenant_id = $1::uuid AND shared_inbox_id = $2::uuid
			ORDER BY user_id
		`, tenantID, inboxID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// RemoveSharedInboxMember removes a user from a shared inbox. It
// returns ErrNotFound if the membership row does not exist or is
// hidden by RLS.
func (s *Service) RemoveSharedInboxMember(ctx context.Context, tenantID, inboxID, userID string) error {
	if tenantID == "" || inboxID == "" || userID == "" {
		return fmt.Errorf("%w: tenant id, inbox id, and user id are required", ErrInvalidInput)
	}
	var affected int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM shared_inbox_members
			WHERE tenant_id = $1::uuid
			  AND shared_inbox_id = $2::uuid
			  AND user_id = $3::uuid
		`, tenantID, inboxID, userID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete shared inbox member: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if s.sharedInboxFn != nil {
		members, _ := s.listSharedInboxMemberIDs(ctx, tenantID, inboxID)
		s.sharedInboxFn(ctx, tenantID, inboxID, members, "member_removed")
	}
	return nil
}

// CreateSharedInbox creates a shared inbox for a tenant. Membership
// is managed separately via `shared_inbox_members` — this method
// only mints the inbox row and the MLS group reference.
func (s *Service) CreateSharedInbox(ctx context.Context, tenantID string, in CreateSharedInboxInput) (*SharedInbox, error) {
	if in.Address == "" || in.DisplayName == "" || in.MLSGroupID == "" {
		return nil, fmt.Errorf("%w: address, display_name, and mls_group_id are required", ErrInvalidInput)
	}
	var s2 SharedInbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO shared_inboxes (tenant_id, address, display_name, mls_group_id)
			VALUES ($1::uuid, $2, $3, $4)
			RETURNING id::text, tenant_id::text, address, display_name,
			          mls_group_id, created_at, updated_at
		`, tenantID, in.Address, in.DisplayName, in.MLSGroupID).Scan(
			&s2.ID, &s2.TenantID, &s2.Address, &s2.DisplayName,
			&s2.MLSGroupID, &s2.CreatedAt, &s2.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert shared inbox: %w", err)
	}
	return &s2, nil
}
