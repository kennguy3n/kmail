package iamcore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kennguy3n/kmail/internal/tenant"
)

// Event type identifiers iam-core sends in the webhook envelope's
// `type` field. KMail provisions / deprovisions control-plane state
// in response to each.
const (
	EventTenantCreate = "tenant.create"
	EventUserCreate   = "user.create"
	EventUserUpdate   = "user.update"
	EventUserDelete   = "user.delete"
)

// signatureHeader is the header iam-core signs each delivery with.
// KMail reuses the same `t=<unix>,v1=<hex>` HMAC-SHA256 scheme its
// own outbound webhooks use (see internal/webhooks) so operators
// have a single signature format to reason about.
const signatureHeader = "X-KMail-Signature"

// signatureTolerance bounds how far the signed timestamp may drift
// from now before the delivery is rejected as a replay. Matches the
// 5-minute window used elsewhere in KMail's webhook tooling.
const signatureTolerance = 5 * time.Minute

// maxWebhookBodyBytes caps the request body the receiver buffers
// for signature verification + JSON decode.
const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// TenantService is the slice of *tenant.Service the iam-core
// webhook receiver drives. Declaring it as an interface keeps the
// receiver unit-testable with a stub and documents exactly which
// control-plane mutations iam-core events trigger. *tenant.Service
// satisfies it directly.
type TenantService interface {
	EnsureTenant(ctx context.Context, in tenant.EnsureTenantInput) (*tenant.Tenant, bool, error)
	CreateUser(ctx context.Context, tenantID string, in tenant.CreateUserInput) (*tenant.User, error)
	GetUserByKChatID(ctx context.Context, tenantID, kchatUserID string) (*tenant.User, error)
	UpdateUser(ctx context.Context, tenantID, userID string, in tenant.UpdateUserInput) (*tenant.User, error)
	DeleteUser(ctx context.Context, tenantID, userID string) error
}

// Event is the webhook envelope iam-core POSTs to KMail. `Data` is
// the type-specific payload, decoded lazily into the matching
// struct once the `Type` is known.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt int64           `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// TenantEventData is the payload for tenant.create. iam-core's
// tenant identifier is carried as `tenant_id`; Name/Slug/Plan seed
// the KMail control-plane row.
type TenantEventData struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Plan     string `json:"plan"`
}

// UserEventData is the payload for user.create / user.update /
// user.delete. `UserID` is the iam-core user id (the `sub` claim of
// a user token), mapped onto KMail's `kchat_user_id` column.
type UserEventData struct {
	TenantID    string `json:"tenant_id"`
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// WebhookReceiver validates and dispatches iam-core lifecycle
// events to the KMail control plane.
type WebhookReceiver struct {
	secret string
	tenant TenantService
	enrich *Client
	logger *log.Logger
	now    func() time.Time
}

// NewWebhookReceiver builds a receiver. secret is the shared
// HMAC-SHA256 key (KMAIL_IAM_CORE_WEBHOOK_SECRET); tenantSvc is the
// control-plane service the events drive.
func NewWebhookReceiver(secret string, tenantSvc TenantService, logger *log.Logger) *WebhookReceiver {
	if logger == nil {
		logger = log.Default()
	}
	return &WebhookReceiver{
		secret: secret,
		tenant: tenantSvc,
		logger: logger,
		now:    time.Now,
	}
}

// WithClient attaches an iam-core Management API client the receiver
// uses to backfill user fields (email / display name) that a sparse
// event payload omits. Optional: without it the receiver provisions
// strictly from the data carried in the event. Returns the receiver
// for chaining.
func (rec *WebhookReceiver) WithClient(c *Client) *WebhookReceiver {
	rec.enrich = c
	return rec
}

// Register mounts the receiver at POST /api/v1/webhooks/iam-core.
// The route is intentionally NOT wrapped in the OIDC middleware:
// iam-core authenticates itself by signing the body, not by
// presenting a user bearer token. Authenticity is established by
// VerifySignature inside ServeHTTP.
func (rec *WebhookReceiver) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/webhooks/iam-core", rec)
}

// ServeHTTP buffers the body, verifies the signature, decodes the
// envelope, and dispatches. It returns 200 on success, 400 for a
// malformed body, 401 for a bad/missing signature, and 500 when a
// downstream provisioning call fails (so iam-core retries).
func (rec *WebhookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if rec.secret == "" {
		// Fail closed: an unconfigured secret means we cannot
		// authenticate the sender, so we must not act on the body.
		rec.logger.Printf("iamcore webhook: rejected delivery — KMAIL_IAM_CORE_WEBHOOK_SECRET is not configured")
		http.Error(w, "webhook receiver not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	sig := r.Header.Get(signatureHeader)
	if !rec.VerifySignature(sig, body) {
		rec.logger.Printf("iamcore webhook: signature verification failed")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var evt Event
	if err := json.Unmarshal(body, &evt); err != nil {
		http.Error(w, "malformed event", http.StatusBadRequest)
		return
	}
	if err := rec.Dispatch(r.Context(), evt); err != nil {
		if errors.Is(err, errUnhandledEvent) {
			// Unknown event types are acknowledged with 200 so
			// iam-core does not retry an event KMail will never
			// process; the type is logged for visibility.
			rec.logger.Printf("iamcore webhook: ignoring unhandled event type %q", evt.Type)
			w.WriteHeader(http.StatusOK)
			return
		}
		rec.logger.Printf("iamcore webhook: processing %q failed: %v", evt.Type, err)
		http.Error(w, "event processing failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// errUnhandledEvent flags an event type the receiver does not act
// on, so ServeHTTP can ACK rather than ask iam-core to retry.
var errUnhandledEvent = errors.New("iamcore: unhandled event type")

// Dispatch routes a verified event to the matching handler. Exposed
// (separately from ServeHTTP) so tests can drive event handling
// without constructing an HTTP request.
func (rec *WebhookReceiver) Dispatch(ctx context.Context, evt Event) error {
	switch evt.Type {
	case EventTenantCreate:
		return rec.handleTenantCreate(ctx, evt)
	case EventUserCreate:
		return rec.handleUserCreate(ctx, evt)
	case EventUserUpdate:
		return rec.handleUserUpdate(ctx, evt)
	case EventUserDelete:
		return rec.handleUserDelete(ctx, evt)
	default:
		return errUnhandledEvent
	}
}

func (rec *WebhookReceiver) handleTenantCreate(ctx context.Context, evt Event) error {
	var d TenantEventData
	if err := json.Unmarshal(evt.Data, &d); err != nil {
		return fmt.Errorf("decode tenant.create data: %w", err)
	}
	if d.TenantID == "" {
		return fmt.Errorf("tenant.create: tenant_id is required")
	}
	// EnsureTenant is idempotent on the tenant UUID, so redelivered
	// tenant.create events (iam-core delivers at-least-once)
	// converge on the same control-plane row instead of wedging on a
	// duplicate-key 500. The authoritative name/slug/plan are passed
	// through verbatim (not pre-defaulted here): EnsureTenant fills
	// the NOT NULL columns with sensible defaults on first insert and
	// reconciles them onto a row that a prior lazy provision created
	// with placeholder values, so the iam-core slug/name/plan win
	// once the webhook arrives.
	_, created, err := rec.tenant.EnsureTenant(ctx, tenant.EnsureTenantInput{
		ID:   d.TenantID,
		Name: d.Name,
		Slug: d.Slug,
		Plan: d.Plan,
	})
	if err != nil {
		return fmt.Errorf("tenant.create: %w", err)
	}
	if created {
		rec.logger.Printf("iamcore webhook: provisioned tenant %q (slug=%q) from tenant.create", d.TenantID, firstNonEmpty(d.Slug, d.TenantID))
	} else {
		rec.logger.Printf("iamcore webhook: tenant %q already provisioned, tenant.create is a no-op", d.TenantID)
	}
	return nil
}

func (rec *WebhookReceiver) handleUserCreate(ctx context.Context, evt Event) error {
	d, err := decodeUserEvent(evt)
	if err != nil {
		return err
	}
	if d.TenantID == "" || d.UserID == "" {
		return fmt.Errorf("user.create: tenant_id and user_id are required")
	}
	d = rec.enrichUser(ctx, d)
	display := firstNonEmpty(d.DisplayName, d.Name, d.Email, d.UserID)
	in := tenant.CreateUserInput{
		// iam-core user id maps onto KMail's external identity
		// column. The Stalwart mailbox account id is derived
		// deterministically from the iam-core user id so it is
		// stable across redeliveries and unique per user.
		KChatUserID:       d.UserID,
		StalwartAccountID: stalwartAccountID(d.UserID),
		Email:             d.Email,
		DisplayName:       display,
	}
	if _, err := rec.tenant.CreateUser(ctx, d.TenantID, in); err != nil {
		if isDuplicate(err) {
			rec.logger.Printf("iamcore webhook: user %q already provisioned in tenant %q, treating user.create as no-op", d.UserID, d.TenantID)
			return nil
		}
		return fmt.Errorf("user.create: %w", err)
	}
	rec.logger.Printf("iamcore webhook: provisioned mailbox for iam-core user %q in tenant %q", d.UserID, d.TenantID)
	return nil
}

func (rec *WebhookReceiver) handleUserUpdate(ctx context.Context, evt Event) error {
	d, err := decodeUserEvent(evt)
	if err != nil {
		return err
	}
	if d.TenantID == "" || d.UserID == "" {
		return fmt.Errorf("user.update: tenant_id and user_id are required")
	}
	u, err := rec.tenant.GetUserByKChatID(ctx, d.TenantID, d.UserID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			// The user has not been provisioned in KMail yet (e.g.
			// the user.create webhook was lost). Provision now so
			// the update is not silently dropped.
			rec.logger.Printf("iamcore webhook: user.update for unknown user %q in tenant %q — provisioning", d.UserID, d.TenantID)
			return rec.handleUserCreate(ctx, evt)
		}
		return fmt.Errorf("user.update: resolve user: %w", err)
	}
	display := firstNonEmpty(d.DisplayName, d.Name)
	if display == "" {
		// Nothing mutable changed that KMail tracks; ack.
		return nil
	}
	if _, err := rec.tenant.UpdateUser(ctx, d.TenantID, u.ID, tenant.UpdateUserInput{DisplayName: &display}); err != nil {
		return fmt.Errorf("user.update: %w", err)
	}
	rec.logger.Printf("iamcore webhook: updated metadata for iam-core user %q in tenant %q", d.UserID, d.TenantID)
	return nil
}

func (rec *WebhookReceiver) handleUserDelete(ctx context.Context, evt Event) error {
	d, err := decodeUserEvent(evt)
	if err != nil {
		return err
	}
	if d.TenantID == "" || d.UserID == "" {
		return fmt.Errorf("user.delete: tenant_id and user_id are required")
	}
	u, err := rec.tenant.GetUserByKChatID(ctx, d.TenantID, d.UserID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			rec.logger.Printf("iamcore webhook: user.delete for unknown user %q in tenant %q — no-op", d.UserID, d.TenantID)
			return nil
		}
		return fmt.Errorf("user.delete: resolve user: %w", err)
	}
	if err := rec.tenant.DeleteUser(ctx, d.TenantID, u.ID); err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("user.delete: %w", err)
	}
	rec.logger.Printf("iamcore webhook: deprovisioned mailbox for iam-core user %q in tenant %q", d.UserID, d.TenantID)
	return nil
}

// enrichUser backfills email / display fields a sparse event omits
// by fetching the authoritative user record from iam-core's
// Management API. It is a best-effort optimisation: with no client
// configured, or when the lookup fails, it returns the input
// unchanged and provisioning proceeds from the event data (the
// downstream tenant.Service validation still enforces required
// fields). Fields already present in the event win — the event is
// the triggering source of truth; the API only fills gaps.
func (rec *WebhookReceiver) enrichUser(ctx context.Context, d UserEventData) UserEventData {
	if rec.enrich == nil {
		return d
	}
	if d.Email != "" && firstNonEmpty(d.DisplayName, d.Name) != "" {
		return d
	}
	u, err := rec.enrich.GetUser(ctx, d.TenantID, d.UserID)
	if err != nil {
		rec.logger.Printf("iamcore webhook: enrich user %q failed, using event data: %v", d.UserID, err)
		return d
	}
	if d.Email == "" {
		d.Email = u.Email
	}
	if d.Name == "" {
		d.Name = u.Name
	}
	if d.DisplayName == "" {
		d.DisplayName = firstNonEmpty(u.Name, u.GivenName)
	}
	return d
}

func decodeUserEvent(evt Event) (UserEventData, error) {
	var d UserEventData
	if err := json.Unmarshal(evt.Data, &d); err != nil {
		return UserEventData{}, fmt.Errorf("decode %s data: %w", evt.Type, err)
	}
	return d, nil
}

// VerifySignature recomputes the HMAC-SHA256 over `<unix>.<body>`
// and constant-time compares it against the `v1=` component of the
// header. It also enforces the timestamp tolerance to bound replay.
// Header format: `t=<unix>,v1=<hex>`.
func (rec *WebhookReceiver) VerifySignature(header string, body []byte) bool {
	if rec.secret == "" || header == "" {
		return false
	}
	var tsStr, v1 string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			tsStr = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if tsStr == "" || v1 == "" {
		return false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	drift := rec.now().Sub(time.Unix(ts, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > signatureTolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(rec.secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	expected := mac.Sum(nil)
	// Decode the provided hex first so a malformed (odd-length /
	// non-hex) value is rejected cleanly; hmac.Equal then does the
	// constant-time compare over the raw MAC bytes.
	provided, err := hex.DecodeString(v1)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, provided)
}

// SignPayload produces the `t=<unix>,v1=<hex>` header value for a
// body + secret. Exposed so callers (and tests) can generate a
// signature identical to the one iam-core is expected to send.
func SignPayload(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// firstNonEmpty returns the first non-empty (after trim) argument,
// or "" when all are empty. Used to derive provisioning fields from
// whichever of several optional event fields iam-core populated.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// stalwartAccountID derives a stable Stalwart mailbox account id
// from the iam-core user id. Deterministic so redelivered
// user.create events resolve to the same account.
func stalwartAccountID(iamUserID string) string {
	return "iam-" + iamUserID
}

// isDuplicate reports whether err is a Postgres unique-violation
// (SQLSTATE 23505) surfaced through the tenant.Service, which wraps
// the underlying pgconn error with %w. Used to make create handlers
// idempotent against iam-core's at-least-once webhook delivery.
//
// The typed *pgconn.PgError check is authoritative and robust
// against Postgres message-wording changes; the string match is a
// defensive fallback for any path that loses the typed error in its
// wrap chain.
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgerrcodeUniqueViolation
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "SQLSTATE 23505") ||
		strings.Contains(msg, "unique constraint")
}

// pgerrcodeUniqueViolation is the Postgres SQLSTATE for a unique
// constraint violation.
const pgerrcodeUniqueViolation = "23505"
