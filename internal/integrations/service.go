package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// DefaultClientDispatchPerHour is the per-OAuth2-client sliding-
// window quota the Dispatcher enforces when an integration does
// not have a per-client override in oauth_clients.dispatch_quota_per_hour.
//
// The value (3600/hour, i.e. one delivery per second) is chosen
// to be generous for a well-behaved Zapier-style integration
// (Zapier polling intervals are 1-15 minutes; CRMs streaming
// inbox changes do not exceed ~one event per second per user)
// and restrictive enough that a single rogue integration cannot
// drown out the BFF's outbound webhook capacity. Operators tune
// the global default via Service constructor; per-client
// overrides take precedence (see ResolveClientQuota).
const DefaultClientDispatchPerHour = 3600

// ErrClientQuotaExceeded indicates the calling OAuth2 client
// has consumed its outbound webhook delivery quota for the
// current sliding-window bucket. The Dispatcher returns this
// from Dispatch when the rate limiter declines a delivery; the
// caller decides whether to drop the event (high-volume
// firehose case) or persist it with `next_retry_at = next
// window boundary` (the framework's at-least-once default).
var ErrClientQuotaExceeded = errors.New("integrations: client outbound dispatch quota exceeded")

// ErrInsufficientScope is returned when the caller's OAuth2
// access token does not carry a scope required for the
// requested operation. Surfaced through the HTTP handler as
// 403 + WWW-Authenticate: insufficient_scope (RFC 6750 §3.1).
var ErrInsufficientScope = errors.New("integrations: insufficient_scope")

// ErrWebhookNotFound is returned by ListWebhooksForClient /
// DeleteWebhookForClient when the addressed row does not exist
// OR exists but is owned by a different OAuth2 client (the
// distinction is intentionally hidden — RFC 7807 §3.1 / RFC
// 7235: do not leak existence of resources the caller is not
// authorised to see).
var ErrWebhookNotFound = errors.New("integrations: webhook not found")

// SubscribeResult is returned from RegisterWebhookForClient so
// the caller can distinguish "subscribed to all requested events"
// from "subscribed to some events; these others were filtered
// out". The integration is expected to re-prompt the user for
// consent to add the missing scopes if the denied list is
// non-empty.
type SubscribeResult struct {
	Endpoint *webhooks.Endpoint
	Secret   string   // plaintext signing secret, returned ONCE on register
	Denied   []string // event types the calling client was not scoped for
}

// ServiceConfig wires the integration service. All fields are
// required EXCEPT LimiterStore (nil disables the per-client
// dispatch quota — appropriate for tests / single-tenant dev
// boxes that have no Valkey).
type ServiceConfig struct {
	// Pool is the Postgres connection pool. The integrations
	// service runs custom SQL against `webhook_endpoints` to
	// scope queries by `oauth_client_id`, which the public
	// webhooks.Service API does not surface.
	Pool *pgxpool.Pool

	// Webhooks is the underlying delivery machinery. The
	// integrations service wraps RegisterWebhook / TestFire and
	// delegates HMAC signing / worker queueing to it.
	Webhooks *webhooks.Service

	// OAuth resolves OAuth2 clients (used to look up the
	// per-client dispatch_quota_per_hour override at fire time).
	OAuth *oauth.Service

	// LimiterStore is the Valkey-compatible counter store. When
	// nil, Dispatch never returns ErrClientQuotaExceeded — the
	// per-client quota is effectively unbounded. The same store
	// type is shared with internal/middleware/ratelimit so
	// operators wire one Valkey client for all rate limiters.
	LimiterStore middleware.RateLimiterStore

	// DefaultClientDispatchPerHour is the default per-client
	// quota when oauth_clients.dispatch_quota_per_hour is NULL.
	// Zero defaults to the package constant.
	DefaultClientDispatchPerHour int

	// Logger is used for transient-error diagnostics and the
	// fail-open path on rate limiter outage. Defaults to
	// log.Default().
	Logger *log.Logger

	// Now overrides time.Now for tests. Defaults to time.Now.
	Now func() time.Time
}

// Service is the public API of the integration framework. It
// composes the underlying webhooks.Service with OAuth2-client
// scope filtering and rate-limited dispatch.
type Service struct {
	cfg ServiceConfig
}

// NewService constructs an integration framework Service. The
// returned Service is safe for concurrent use across all
// methods.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("integrations.NewService: Pool is required")
	}
	if cfg.Webhooks == nil {
		return nil, errors.New("integrations.NewService: Webhooks is required")
	}
	if cfg.OAuth == nil {
		return nil, errors.New("integrations.NewService: OAuth is required")
	}
	if cfg.DefaultClientDispatchPerHour <= 0 {
		cfg.DefaultClientDispatchPerHour = DefaultClientDispatchPerHour
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}, nil
}

// RegisterWebhookForClient is the subscribe-time entry point.
// It:
//
//  1. Validates the input (non-empty URL, tenant + client + user
//     IDs).
//  2. Filters the requested event list through
//     FilterEventsForClient. If every event was denied, returns
//     ErrInsufficientScope so the handler can answer 422 with
//     the denied list (so the integration knows which scope to
//     ask the user for).
//  3. Delegates to webhooks.Service.RegisterWebhook to create
//     the row and mint the plaintext secret.
//  4. INSERTS the row via webhooks.Service.RegisterWebhook with
//     the optional Owner argument carrying both
//     `oauth_client_id` AND `user_id`, so the owner columns
//     land in the SAME SQL statement as the rest of the row.
//     There is no transient "row exists with oauth_client_id =
//     NULL" window — that was the round-1 PR #36 finding the
//     atomicity-on-INSERT fix here closes.
//
// `userID` is the consenting user from the OAuth2 access token
// presented at subscribe time (oauth.AccessTokenContext.UserID).
// Tracking it here is what lets the dispatcher source
// granted-scopes per-user (instead of from the static client
// `allowed_scopes`): when the user later revokes via
// /oauth/revoke, every access token they held flips
// `revoked_at`, the dispatcher's per-event join produces no
// surviving scope set, and delivery stops — without the
// integration having to call us to unsubscribe. This closes the
// PR #36 round-1 architectural finding.
//
// Returns SubscribeResult with the final endpoint, the
// plaintext secret (only available here once), and the list of
// denied event types so the caller can surface them.
func (s *Service) RegisterWebhookForClient(
	ctx context.Context,
	tenantID, oauthClientID, userID string,
	grantedScopes []string,
	url string,
	requestedEvents []string,
	signingVersion string,
) (*SubscribeResult, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("integrations: tenantID required")
	}
	if strings.TrimSpace(oauthClientID) == "" {
		return nil, errors.New("integrations: oauthClientID required")
	}
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("integrations: userID required (the consenting user)")
	}
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("integrations: url required")
	}
	if len(requestedEvents) == 0 {
		// Defense-in-depth: the boundary handler at
		// internal/integrations/handlers.go already rejects
		// `len(req.Events) == 0` with 400 invalid_request (the
		// round-6 fix), but the service method MUST refuse it
		// independently so a future caller that bypasses the
		// HTTP handler (e.g. an internal admin path, a
		// migration backfill, a programmatic client built on
		// top of *Service directly) cannot persist a row with
		// `events = []`. The underlying webhooks layer
		// interprets `events = []` as "deliver every event"
		// (see internal/webhooks/service.go DeliverEvent
		// query — `jsonb_array_length(events) = 0 OR events ?
		// $2`), so an empty array on an integration-owned row
		// would otherwise become a wildcard subscription.
		// Dispatch-time `EventAllowedForClient` still gates on
		// actual scopes (no privilege escalation possible),
		// but persisting a row that's broader than the
		// caller's intent is a contract violation. Fail fast.
		return nil, errors.New("integrations: requestedEvents required (at least one event)")
	}

	allowed, denied := FilterEventsForClient(grantedScopes, requestedEvents)
	if len(allowed) == 0 {
		// Every requested event was denied — the integration
		// has no scope to receive any of them. Fail fast
		// rather than register a webhook that will never fire.
		// `len(requestedEvents) > 0` is now guaranteed by the
		// guard above, so the previous compound check
		// (`len(requestedEvents) > 0 && len(allowed) == 0`)
		// collapses to just `len(allowed) == 0`.
		return &SubscribeResult{Denied: denied}, ErrInsufficientScope
	}

	// Single INSERT with the owner columns stamped in the same
	// SQL statement — webhooks.Service.RegisterWebhook's
	// optional Owner argument carries (oauth_client_id, user_id)
	// into the row's INSERT, so there is NO transient window
	// where the row exists with oauth_client_id IS NULL. The
	// previous two-phase INSERT + UPDATE was the round-1 PR #36
	// finding: a concurrent dispatch firing between the two
	// transactions would observe a row that looked admin-owned,
	// skip the per-client scope check, and deliver events the
	// integration hadn't been scoped for. The single INSERT
	// makes the (insert-then-visible) state atomic.
	ep, secret, err := s.cfg.Webhooks.RegisterWebhook(
		ctx, tenantID, url, allowed, signingVersion,
		webhooks.Owner{OAuthClientID: oauthClientID, UserID: userID},
	)
	if err != nil {
		return nil, fmt.Errorf("integrations: register webhook: %w", err)
	}

	return &SubscribeResult{Endpoint: ep, Secret: secret, Denied: denied}, nil
}

// ListWebhooksForClient returns the webhooks the calling
// OAuth2 client owns. Cross-client visibility is impossible by
// construction: the SQL filters on `oauth_client_id`.
func (s *Service) ListWebhooksForClient(ctx context.Context, tenantID, oauthClientID string) ([]webhooks.Endpoint, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(oauthClientID) == "" {
		return nil, errors.New("integrations: tenantID + oauthClientID required")
	}
	var out []webhooks.Endpoint
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, url, events, active, signing_version, created_at, updated_at
			FROM webhook_endpoints
			WHERE tenant_id = $1::uuid AND oauth_client_id = $2::uuid
			ORDER BY created_at DESC
		`, tenantID, oauthClientID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ep webhooks.Endpoint
			var rawEvents []byte
			if err := rows.Scan(&ep.ID, &ep.TenantID, &ep.URL, &rawEvents, &ep.Active, &ep.SigningVersion, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(rawEvents, &ep.Events)
			out = append(out, ep)
		}
		return rows.Err()
	})
	return out, err
}

// DeleteWebhookForClient removes a webhook the calling OAuth2
// client owns. Returns ErrWebhookNotFound if the row does not
// exist OR is owned by a different client — the same error in
// either case so the response does not leak existence of rows
// the caller is not authorised to see.
func (s *Service) DeleteWebhookForClient(ctx context.Context, tenantID, oauthClientID, webhookID string) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(oauthClientID) == "" || strings.TrimSpace(webhookID) == "" {
		return errors.New("integrations: tenantID + oauthClientID + webhookID required")
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		cmdTag, err := tx.Exec(ctx, `
			DELETE FROM webhook_endpoints
			WHERE id = $1::uuid AND tenant_id = $2::uuid AND oauth_client_id = $3::uuid
		`, webhookID, tenantID, oauthClientID)
		if err != nil {
			return err
		}
		if cmdTag.RowsAffected() == 0 {
			return ErrWebhookNotFound
		}
		return nil
	})
}

// TestFireForClient enqueues a synthetic `webhook.ping`
// delivery for a webhook the calling OAuth2 client owns. The
// existence check is layered on top of webhooks.Service.TestFire
// so we can return ErrWebhookNotFound for cross-client targets.
//
// The ownership pre-check folds `active = true` into the
// existence predicate (mirroring webhooks.Service.TestFire's
// own active-guard at internal/webhooks/service.go:293). Three
// reasons for the fold:
//
//  1. webhooks.Service.TestFire returns a plain `errors.New`
//     for the "endpoint not found or inactive" case that does
//     NOT match ErrWebhookNotFound. Without the fold, an
//     inactive-but-owned webhook would pass the ownership
//     check (exists=true), then fail inside TestFire with an
//     unmatched error, and the handler would surface it as
//     500 server_error — wrong status code for a known
//     resource state. The fold makes the 404 path catch it.
//  2. The integration framework doesn't expose any
//     reactivation API, so a 409 "webhook inactive" response
//     would give the integration no actionable signal it
//     can't already get from GET /webhooks (which lists the
//     active flag). Better to treat "can't test what you
//     can't deliver to" as a clean 404.
//  3. From the caller's perspective an inactive webhook is
//     indistinguishable from a soft-deleted one: the BFF
//     doesn't deliver to it, so test-firing against it would
//     mislead the integration about wire health. Refusing to
//     test-fire inactive endpoints preserves the "if test
//     succeeds, real deliveries will too" invariant the
//     endpoint is designed around.
func (s *Service) TestFireForClient(ctx context.Context, tenantID, oauthClientID, webhookID string) (int, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(oauthClientID) == "" || strings.TrimSpace(webhookID) == "" {
		return 0, errors.New("integrations: tenantID + oauthClientID + webhookID required")
	}
	// Verify ownership BEFORE delegating, so the underlying
	// "endpoint not found" error from webhooks.Service.TestFire
	// (which doesn't know about OAuth2 clients) does not leak
	// the existence of an admin-owned endpoint. `active = true`
	// is folded in to keep status semantics aligned with the
	// downstream call (see the function-level comment block).
	var exists bool
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM webhook_endpoints
				WHERE id = $1::uuid AND tenant_id = $2::uuid
				  AND oauth_client_id = $3::uuid AND active = true)
		`, webhookID, tenantID, oauthClientID).Scan(&exists)
	})
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrWebhookNotFound
	}
	return s.cfg.Webhooks.TestFire(ctx, tenantID, webhookID)
}

// DispatchEvent is the integration-framework dispatcher. For a
// given (tenantID, eventType, payload) it:
//
//  1. Enqueues deliveries for legacy / admin-owned webhooks
//     (oauth_client_id IS NULL) via the existing
//     webhooks.Service.DeliverEvent fan-out — these are
//     unaffected by the integration framework.
//  2. Enumerates integration-owned subscribers (oauth_client_id
//     IS NOT NULL) and, for each one:
//     a. Re-checks the client's granted scopes against
//        EventAllowedForClient (defence-in-depth — see doc.go
//        Scope enforcement #3).
//     b. Consults the per-client rate limiter. On overflow,
//        the delivery is enqueued with `next_retry_at = next
//        window boundary` so it ships on the next bucket.
//     c. Otherwise, INSERTs a delivery row immediately.
//
// Returns the total number of delivery rows that were enqueued
// (admin-owned + integration-owned + quota-deferred).
func (s *Service) DispatchEvent(ctx context.Context, tenantID, eventType string, payload map[string]any) (int, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(eventType) == "" {
		return 0, errors.New("integrations: tenantID + eventType required")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("integrations: marshal payload: %w", err)
	}

	enqueued := 0

	// Step 1: admin-owned webhook fan-out (legacy path,
	// unaffected by scope filtering).
	admin, err := s.dispatchAdminOwned(ctx, tenantID, eventType, body)
	if err != nil {
		return enqueued, fmt.Errorf("integrations: admin-owned dispatch: %w", err)
	}
	enqueued += admin

	// Step 2: integration-owned subscribers, with per-client
	// scope and quota enforcement.
	integ, err := s.dispatchIntegrationOwned(ctx, tenantID, eventType, body)
	if err != nil {
		return enqueued, fmt.Errorf("integrations: integration-owned dispatch: %w", err)
	}
	enqueued += integ

	return enqueued, nil
}

// dispatchAdminOwned inserts deliveries for webhook_endpoints
// rows that have NULL oauth_client_id (legacy / admin-owned).
// Behaviour matches the pre-Phase-E webhooks.Service.DeliverEvent
// path; no scope filter, no per-client rate limit.
func (s *Service) dispatchAdminOwned(ctx context.Context, tenantID, eventType string, body []byte) (int, error) {
	var enqueued int
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Buffer IDs in-memory before the INSERT loop so the
		// rows.Close() can run inside the same logical scope as
		// the Query (via defer) without holding the result set
		// open across N tx.Exec calls. pgx v5's pgx.Rows.Next()
		// auto-closes on natural-end-of-iteration, but a Scan
		// error or context cancellation between Query and the
		// loop terminator would otherwise leave the rows open
		// until the GC finalizer fires — fragile against future
		// edits that add code paths between Query and Close.
		// `defer rows.Close()` is idempotent (pgx.Rows.Close
		// guards a closed flag internally) and handles every
		// exit path uniformly.
		ids, err := func() ([]string, error) {
			rows, err := tx.Query(ctx, `
				SELECT id::text FROM webhook_endpoints
				WHERE tenant_id = $1::uuid AND active = true
				  AND oauth_client_id IS NULL
				  AND (jsonb_array_length(events) = 0 OR events ? $2)
			`, tenantID, eventType)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var collected []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				collected = append(collected, id)
			}
			// pgx surfaces driver-level iteration errors
			// (network reset, partial result) on rows.Err()
			// after the loop terminates. Without this check
			// a transport-level failure mid-scan would
			// silently truncate the subscriber list and the
			// dispatcher would under-deliver — a silent
			// at-least-once violation. Treat as fatal: the
			// outer BeginFunc retries the whole dispatch.
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("integrations: scan admin-owned subscribers: %w", err)
			}
			return collected, nil
		}()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `
				INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_type, payload)
				VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
			`, tenantID, id, eventType, string(body)); err != nil {
				return err
			}
			enqueued++
		}
		return nil
	})
	return enqueued, err
}

// integrationSubscriber bundles the row data we need to apply
// scope + quota enforcement before INSERTing a delivery.
type integrationSubscriber struct {
	EndpointID    string
	OAuthClientID string
	UserID        string   // consenting user; used to look up per-user grants
	QuotaPerHour  *int     // nil means use service default
	GrantedScopes []string // UNION of scopes across the user's live, non-revoked access tokens for this client
}

// dispatchIntegrationOwned enumerates integration-owned
// subscribers, runs the defence-in-depth scope check and the
// per-client rate limit, and enqueues a delivery (or a
// quota-deferred delivery with future next_retry_at) for each.
func (s *Service) dispatchIntegrationOwned(ctx context.Context, tenantID, eventType string, body []byte) (int, error) {
	subs, err := s.loadIntegrationSubscribers(ctx, tenantID, eventType)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	var (
		insertFailed int
		lastInsertErr error
	)
	for _, sub := range subs {
		if !EventAllowedForClient(sub.GrantedScopes, eventType) {
			// Subscribed at some past point with broader
			// scopes; the user has since revoked. Skip
			// silently — the next OAuth2 token refresh will
			// reflect the revocation and the integration
			// can re-prompt.
			s.cfg.Logger.Printf("integrations: skip delivery for endpoint=%s client=%s event=%s — scope no longer granted",
				sub.EndpointID, sub.OAuthClientID, eventType)
			continue
		}

		quota := s.cfg.DefaultClientDispatchPerHour
		if sub.QuotaPerHour != nil {
			quota = *sub.QuotaPerHour
		}
		allowed, nextRetryAt := s.checkQuota(ctx, sub.OAuthClientID, quota)
		if err := s.insertIntegrationDelivery(ctx, tenantID, sub.EndpointID, eventType, body, allowed, nextRetryAt); err != nil {
			// One subscriber's failure must not block other
			// subscribers (head-of-line blocking would be
			// strictly worse for at-least-once than the per-
			// subscriber drop), so we log + continue. BUT we
			// also remember the failure and surface it as a
			// non-nil return error after the loop so the
			// caller (the outbox-style upstream that fired
			// this event) knows to schedule a redrive. Without
			// the surfaced error the dispatcher silently
			// degrades to at-MOST-once on transient INSERT
			// failure, violating the at-least-once default
			// promised by the comment block at the top of
			// this file (line 38-41) and in doc.go.
			s.cfg.Logger.Printf("integrations: insert delivery failed for endpoint=%s client=%s: %v", sub.EndpointID, sub.OAuthClientID, err)
			insertFailed++
			lastInsertErr = err
			continue
		}
		enqueued++
	}
	if insertFailed > 0 {
		return enqueued, fmt.Errorf("integrations: %d of %d integration-owned subscribers failed to enqueue (last error: %w)",
			insertFailed, len(subs), lastInsertErr)
	}
	return enqueued, nil
}

// loadIntegrationSubscribers reads all integration-owned
// webhook_endpoints rows subscribed to the given event_type for
// the given tenant, joined to oauth_clients for the per-client
// quota override AND to oauth_access_tokens for the consenting
// user's CURRENT granted-scopes set.
//
// Per-user grants (the PR #36 round-1 architectural fix):
// granted_scopes is the UNION of `scopes` over all non-revoked,
// non-expired access tokens for (tenant, oauth_client_id,
// user_id). This is the canonical view of "what scopes does the
// user currently grant this integration": when the user revokes
// via /oauth/revoke or the consent UI, all relevant rows flip
// `revoked_at IS NOT NULL`, the LATERAL join below shrinks the
// result accordingly, and the dispatcher's EventAllowedForClient
// check filters the now-unauthorised events out. The static
// `oauth_clients.allowed_scopes` column is NOT consulted here —
// it represents what the client *may request*, not what the
// user actually *granted*, and using it (as round-1 did) leaks
// events after consent revocation.
//
// Rows whose user_id has been deleted (user offboarded) yield
// no token row in the join and are therefore skipped — exactly
// the desired behaviour, because there is no live user to
// re-consent.
//
// Legacy / round-1 rows that were registered before migration
// 048 carry user_id = NULL; the LEFT JOIN preserves them but
// the COALESCE leaves granted_scopes as an empty array, so
// EventAllowedForClient returns false for every event and they
// stop delivering until the integration re-subscribes. This is
// the intentional fail-closed migration semantics — the
// alternative (falling back to `oauth_clients.allowed_scopes`
// when user_id IS NULL) re-introduces the privacy gap this
// schema change was meant to close.
func (s *Service) loadIntegrationSubscribers(ctx context.Context, tenantID, eventType string) ([]integrationSubscriber, error) {
	var subs []integrationSubscriber
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT
			    we.id::text,
			    we.oauth_client_id::text,
			    COALESCE(we.user_id::text, ''),
			    oc.dispatch_quota_per_hour,
			    COALESCE(
			        (
			            SELECT to_jsonb(array_agg(DISTINCT s))
			            FROM oauth_access_tokens t,
			                 LATERAL jsonb_array_elements_text(t.scopes) AS s
			            WHERE t.tenant_id = we.tenant_id
			              AND t.client_id = we.oauth_client_id
			              AND t.user_id   = we.user_id
			              AND t.revoked_at IS NULL
			              AND t.expires_at > now()
			        ),
			        '[]'::jsonb
			    ) AS granted_scopes
			FROM webhook_endpoints we
			JOIN oauth_clients oc ON oc.id = we.oauth_client_id
			WHERE we.tenant_id = $1::uuid
			  AND we.active = true
			  -- Filter on the OWNING OAuth2 client's active flag in
			  -- addition to the endpoint's own. Without this, an
			  -- operator deactivating a client (UPDATE oauth_clients
			  -- SET active = false) revokes the client's INBOUND API
			  -- surface immediately (ValidateAccessToken at
			  -- internal/oauth/service.go has its own
			  -- "AND c.active = true" predicate) but the OUTBOUND
			  -- dispatch path keeps firing webhooks at the client's
			  -- endpoints for up to AccessTokenTTL (1h) -- until
			  -- the LATERAL subquery's "t.expires_at > now()"
			  -- filter drains the pre-deactivation tokens out of
			  -- the granted_scopes union. That's a data-leakage
			  -- window the operator's deactivation gesture clearly
			  -- intends to close: a deactivated client should stop
			  -- receiving events with the same step that stops it
			  -- accepting requests.
			  AND oc.active = true
			  AND we.oauth_client_id IS NOT NULL
			  AND (jsonb_array_length(we.events) = 0 OR we.events ? $2)
		`, tenantID, eventType)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sub integrationSubscriber
			var quota *int
			var rawScopes []byte
			if err := rows.Scan(&sub.EndpointID, &sub.OAuthClientID, &sub.UserID, &quota, &rawScopes); err != nil {
				return err
			}
			sub.QuotaPerHour = quota
			if len(rawScopes) > 0 {
				if err := json.Unmarshal(rawScopes, &sub.GrantedScopes); err != nil {
					// Malformed JSON from a programmatic
					// error — fail closed (no live grant)
					// rather than reading the static
					// client allowed_scopes.
					sub.GrantedScopes = nil
				}
			}
			subs = append(subs, sub)
		}
		return rows.Err()
	})
	return subs, err
}

// checkQuota consults the rate limiter for the given client.
// Returns (allowed=true, nextRetryAt=zero) when the bucket has
// room, (allowed=false, nextRetryAt=window boundary) when it
// is full, and (allowed=true, nextRetryAt=zero) on a transient
// limiter error (fail-open — see doc.go).
func (s *Service) checkQuota(ctx context.Context, clientID string, quotaPerHour int) (bool, time.Time) {
	if s.cfg.LimiterStore == nil || quotaPerHour <= 0 {
		return true, time.Time{}
	}
	now := s.cfg.Now().UTC()
	// Hourly buckets keyed on (client_id, bucket-start). At
	// most one delivery per bucket pays the EXPIRE cost; all
	// subsequent INCRs share the TTL window.
	bucket := now.Truncate(time.Hour).Unix()
	key := fmt.Sprintf("kmail:integ:dispatch:%s:%d", clientID, bucket)
	ttl := time.Hour + 5*time.Minute // small grace so a late delivery still sees the TTL

	count, err := s.cfg.LimiterStore.IncrWithTTL(ctx, key, ttl)
	if err != nil {
		// Fail-open: don't take dispatch offline because the
		// rate limiter is unhealthy.
		s.cfg.Logger.Printf("integrations: rate limiter incr %s: %v (fail-open)", key, err)
		return true, time.Time{}
	}
	if count > int64(quotaPerHour) {
		// Compute the next bucket boundary so the deferred
		// delivery rides in the fresh quota window.
		nextBoundary := now.Truncate(time.Hour).Add(time.Hour)
		return false, nextBoundary
	}
	return true, time.Time{}
}

// insertIntegrationDelivery enqueues either an immediate
// delivery (allowed=true) or a quota-deferred delivery
// (allowed=false, next_retry_at=window boundary). At-least-once
// semantics — the worker will pick up the deferred row when
// next_retry_at passes.
func (s *Service) insertIntegrationDelivery(
	ctx context.Context,
	tenantID, endpointID, eventType string,
	body []byte,
	allowed bool,
	nextRetryAt time.Time,
) error {
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		if allowed {
			_, err := tx.Exec(ctx, `
				INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_type, payload)
				VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
			`, tenantID, endpointID, eventType, string(body))
			return err
		}
		// Quota-deferred path: explicitly stamp
		// next_retry_at so the worker holds the row until the
		// next quota window opens.
		_, err := tx.Exec(ctx, `
			INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_type, payload, next_retry_at)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, $5)
		`, tenantID, endpointID, eventType, string(body), nextRetryAt)
		return err
	})
}
