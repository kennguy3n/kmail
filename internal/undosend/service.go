// Package undosend implements the "Undo Send" hold queue.
//
// When a user submits an `EmailSubmission/set` create call against
// the JMAP proxy with the `X-KMail-Undo-Send: true` header, the
// proxy hook in this package:
//
//  1. Forwards a stripped JMAP body (without the EmailSubmission/set
//     entry) to Stalwart so the draft `Email/set` portion still
//     mints the underlying message.
//  2. Stores the original `EmailSubmission/set` payload in Valkey
//     keyed by a UUIDv7 (`PendingSendID`), together with the
//     resolved Email id, tenant, kchat-user, and Stalwart account.
//  3. Synthesises an `EmailSubmission/set` response so the JMAP
//     client believes the submission succeeded, and stamps two
//     custom response headers:
//
//     X-KMail-Pending-Send-Id   <uuid>
//     X-KMail-Undo-Deadline     <unix-seconds>
//
// The React Compose view reads those headers and shows an "Undo
// Send" banner with a countdown. If the user clicks Undo, the
// `POST /api/v1/send/{id}/cancel` handler deletes the Valkey key
// before the worker has a chance to dispatch.
//
// The `DispatchWorker` (see worker.go) polls the sorted-set tail
// every second; when an entry's deadline passes the worker pops
// it, resolves the user back to a Stalwart account, and invokes
// the real `EmailSubmission/set` via the JMAP `InternalClient`.
// Stalwart sees a normal authenticated BFF submission at that
// point.
//
// Three storage shapes back this package:
//
//	kmail:pending_send:<id>     STRING  JSON-encoded *PendingSend
//	kmail:pending_sends         ZSET    score=deadline-unix, member=<id>
//	kmail:failed_sends          LIST    operator inspection only
//
// The companion STRING key carries the full submission payload
// (so a node restart doesn't lose work) AND has a TTL set to
// (deadline+slack), which guarantees that even a crashed worker
// can't leak forever.
package undosend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Errors surfaced to the HTTP layer.
var (
	// ErrNotFound is returned when the pending send key is missing
	// (either it already expired and was dispatched, or the
	// caller passed a bad id).
	ErrNotFound = errors.New("undosend: pending send not found")

	// ErrAlreadySent is returned by Cancel when the key has
	// expired (the worker has either already submitted to
	// Stalwart, or is in the process of doing so). 410 Gone.
	ErrAlreadySent = errors.New("undosend: send already dispatched")

	// ErrTenantMismatch is returned when an authenticated tenant
	// tries to cancel a pending send that belongs to a different
	// tenant — surface as 404 to avoid leaking the existence of
	// other tenants' ids.
	ErrTenantMismatch = errors.New("undosend: pending send tenant mismatch")
)

// Status values for a PendingSend.
const (
	StatusPending   = "pending"
	StatusSent      = "sent"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

// Defaults. Override via env in main.go.
const (
	// DefaultDelaySeconds matches the Gmail/Outlook conventions
	// (5–30s). 10s is the documented platform default but the
	// service accepts any positive value through the constructor.
	DefaultDelaySeconds = 10

	// DefaultKeyTTLSlack is added to the deadline before the
	// companion STRING key is allowed to expire. The slack covers
	// worker latency + a failed-send dead-letter window so a
	// crashed worker has a chance to recover the payload from
	// Valkey instead of losing the message entirely.
	DefaultKeyTTLSlack = 5 * time.Minute

	// DefaultMaxDelaySeconds caps the per-call delay an attacker
	// could otherwise extend to keep a message indefinitely
	// undeliverable. Anything beyond five minutes is a different
	// product (Scheduled Send / WS4).
	DefaultMaxDelaySeconds = 300
)

// Redis key prefixes.
const (
	keyPrefix    = "kmail:pending_send:"
	sortedSetKey = "kmail:pending_sends"
	failedListKey = "kmail:failed_sends"
)

// PendingSend is the persisted shape of a held submission.
//
// `SubmissionPayload` is the raw bytes of the original
// `EmailSubmission/set` *create* invocation arguments (the second
// element of the JSON-RPC array). The worker uses these verbatim
// when forwarding to Stalwart so we keep wire fidelity with
// whatever the JMAP client originally sent.
type PendingSend struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	KChatUserID       string    `json:"kchat_user_id"`
	StalwartAccountID string    `json:"stalwart_account_id"`
	// CreateID is the client-supplied creation key from the
	// original EmailSubmission/set `create` map (e.g. "submission").
	// The worker mirrors it in the dispatched response so any
	// downstream consumer can correlate the synthetic creation
	// against the real submission once it fires.
	CreateID string `json:"create_id"`
	// EmailID is the real (post-Stalwart-create) Email id we
	// captured from the stripped-body response. The submission
	// payload may reference `#draft`; the worker substitutes
	// EmailID for that back-reference before dispatching.
	EmailID           string    `json:"email_id"`
	IdentityID        string    `json:"identity_id"`
	SubmissionPayload []byte    `json:"submission_payload"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	DeadlineAt        time.Time `json:"deadline_at"`
	Attempts          int       `json:"attempts"`
	LastError         string    `json:"last_error,omitempty"`
}

// Service is the Valkey-backed hold queue.
//
// `Client` is mandatory; everything else is filled in with sane
// defaults via NewService.
type Service struct {
	client  *redis.Client
	logger  *log.Logger
	delay   time.Duration
	maxDelay time.Duration
	ttlSlack time.Duration
	now     func() time.Time

	// cancelScriptOnce / cancelScript hold the compiled Lua
	// handle for the atomic verify-and-revoke step Cancel runs
	// against Valkey. `redis.NewScript` caches the script SHA so
	// subsequent EVALSHAs avoid the script-payload round-trip.
	cancelScriptOnce sync.Once
	cancelScript     *redis.Script
}

// cancelLua atomically (a) reads the companion key, (b) verifies
// the persisted tenant_id matches the caller's, and (c) revokes
// ownership by removing the sorted-set entry FIRST and only then
// deleting the companion key. Performing the ZREM before the DEL
// is the load-bearing safety property: it closes the TOCTOU race
// between Cancel and the dispatch worker.
//
// Without atomicity, the Cancel path was: GET companion + verify
// tenant + DEL companion + ZREM sorted_set (as a pipeline). A
// worker could ZREM the sorted-set entry between Cancel's GET
// and Cancel's DEL+ZREM, win the dispatch race, read the still-
// present companion key, and submit the email to Stalwart. Cancel
// would then succeed (its ZREM returned 0 but Exec ignored that
// signal) and return nil to the HTTP layer — so the user saw
// "cancelled" but the email was already in flight.
//
// The script returns one of:
//
//	"missing"  -> companion key absent (already dispatched / TTL'd /
//	              never existed). Cancel surfaces ErrAlreadySent.
//	"mismatch" -> tenant_id check failed. Cancel surfaces
//	              ErrTenantMismatch.
//	"claimed"  -> the sorted-set ZREM returned 0, meaning the
//	              dispatch worker has ALREADY claimed ownership
//	              and is committed to submitting. Cancel surfaces
//	              ErrAlreadySent. Critically the companion key is
//	              NOT deleted on this path — the worker still
//	              needs the payload to dispatch.
//	"ok"       -> Cancel owns the revocation; both keys are deleted.
//
// `decode_error` and `internal_error` are returned only when the
// persisted payload is corrupt; both map to a wrapped error from
// Cancel so the HTTP layer can surface a 5xx and operators can
// follow up via the failed-sends list.
//
// Encoded as a const string so `redis.NewScript` compiles once at
// NewService time and EVALSHA wins on every call.
const cancelLua = `
local raw = redis.call("GET", KEYS[1])
if not raw or raw == false or raw == "" then
  return "missing"
end
local ok, decoded = pcall(cjson.decode, raw)
if not ok or type(decoded) ~= "table" then
  return "decode_error"
end
if decoded.tenant_id ~= ARGV[1] then
  return "mismatch"
end
local removed = redis.call("ZREM", KEYS[2], ARGV[2])
if removed == 0 then
  return "claimed"
end
redis.call("DEL", KEYS[1])
return "ok"
`

// Config configures the Service. The zero value of every field
// triggers a documented default.
type Config struct {
	Client    *redis.Client
	Logger    *log.Logger
	Delay     time.Duration
	MaxDelay  time.Duration
	TTLSlack  time.Duration
	NowFunc   func() time.Time
}

// NewService validates the config and returns a Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Client == nil {
		return nil, errors.New("undosend.NewService: Client is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	delay := cfg.Delay
	if delay <= 0 {
		delay = DefaultDelaySeconds * time.Second
	}
	maxDelay := cfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelaySeconds * time.Second
	}
	if delay > maxDelay {
		return nil, fmt.Errorf("undosend.NewService: Delay %s exceeds MaxDelay %s", delay, maxDelay)
	}
	ttlSlack := cfg.TTLSlack
	if ttlSlack <= 0 {
		ttlSlack = DefaultKeyTTLSlack
	}
	now := cfg.NowFunc
	if now == nil {
		now = time.Now
	}
	s := &Service{
		client:   cfg.Client,
		logger:   logger,
		delay:    delay,
		maxDelay: maxDelay,
		ttlSlack: ttlSlack,
		now:      now,
	}
	s.ensureCancelScript()
	return s, nil
}

// ensureCancelScript compiles `cancelLua` exactly once across the
// lifetime of the Service. sync.Once also gives us happens-before
// for the `cancelScript` write so subsequent concurrent callers
// observe the compiled handle without a data race even when the
// Service was built via the struct literal in tests.
func (s *Service) ensureCancelScript() {
	s.cancelScriptOnce.Do(func() {
		s.cancelScript = redis.NewScript(cancelLua)
	})
}

// Delay returns the configured hold window. Exposed so the proxy
// hook can stamp `X-KMail-Undo-Deadline` consistently.
func (s *Service) Delay() time.Duration { return s.delay }

// HoldInput carries everything the worker needs to dispatch
// later, plus the tenant context the cancel path checks against.
type HoldInput struct {
	TenantID          string
	KChatUserID       string
	StalwartAccountID string
	CreateID          string
	EmailID           string
	IdentityID        string
	SubmissionPayload []byte
}

// Hold persists the submission payload and registers the
// deadline in the sorted set. Returns the freshly minted
// PendingSend (which carries DeadlineAt the caller surfaces to
// the JMAP client).
func (s *Service) Hold(ctx context.Context, in HoldInput) (*PendingSend, error) {
	if err := validateHold(in); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("undosend.Hold: uuidv7: %w", err)
	}
	now := s.now().UTC()
	ps := &PendingSend{
		ID:                id.String(),
		TenantID:          in.TenantID,
		KChatUserID:       in.KChatUserID,
		StalwartAccountID: in.StalwartAccountID,
		CreateID:          in.CreateID,
		EmailID:           in.EmailID,
		IdentityID:        in.IdentityID,
		SubmissionPayload: in.SubmissionPayload,
		Status:            StatusPending,
		CreatedAt:         now,
		DeadlineAt:        now.Add(s.delay),
	}
	if err := s.write(ctx, ps); err != nil {
		return nil, err
	}
	return ps, nil
}

func validateHold(in HoldInput) error {
	if strings.TrimSpace(in.TenantID) == "" {
		return errors.New("undosend.Hold: TenantID is required")
	}
	if strings.TrimSpace(in.KChatUserID) == "" {
		return errors.New("undosend.Hold: KChatUserID is required")
	}
	if strings.TrimSpace(in.StalwartAccountID) == "" {
		return errors.New("undosend.Hold: StalwartAccountID is required")
	}
	if strings.TrimSpace(in.EmailID) == "" {
		return errors.New("undosend.Hold: EmailID is required")
	}
	if len(in.SubmissionPayload) == 0 {
		return errors.New("undosend.Hold: SubmissionPayload is required")
	}
	return nil
}

func (s *Service) write(ctx context.Context, ps *PendingSend) error {
	payload, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("undosend: marshal pending send: %w", err)
	}
	keyTTL := time.Until(ps.DeadlineAt) + s.ttlSlack
	if keyTTL <= 0 {
		keyTTL = s.ttlSlack
	}
	// Pipeline: write the companion key AND register the deadline
	// in the sorted set in one RTT.
	//
	// We deliberately use SETEX (TTL) on the companion key so
	// even a crashed worker eventually releases storage; the
	// worker also DELs explicitly on success so the common-case
	// footprint is bounded by hold-volume * delay, not by TTL.
	pipe := s.client.TxPipeline()
	pipe.Set(ctx, companionKey(ps.ID), payload, keyTTL)
	pipe.ZAdd(ctx, sortedSetKey, redis.Z{
		Score:  float64(ps.DeadlineAt.Unix()),
		Member: ps.ID,
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("undosend: persist pending send: %w", err)
	}
	return nil
}

// Get reads a pending send by id without modifying it. Returns
// ErrNotFound when the key is gone (expired & dispatched, or
// cancelled, or never existed).
func (s *Service) Get(ctx context.Context, id string) (*PendingSend, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrNotFound
	}
	return s.readByID(ctx, id)
}

// Cancel removes the pending send. Verifies tenant ownership
// AND revokes the dispatch claim atomically via a Lua script —
// the verify, ZREM, and companion-DEL all execute as a single
// Valkey command. Without this atomicity the cancel path races
// the dispatch worker: a worker could ZREM-claim ownership
// between Cancel's GET and Cancel's DEL+ZREM pipeline, dispatch
// the email to Stalwart, while Cancel still returned nil — the
// user would see "cancelled" but the message would already be in
// flight. See the cancelLua doc comment for the protocol.
//
// Returns ErrAlreadySent when (a) the companion key is gone
// (worker has already dispatched, TTL fired, or the caller is
// double-clicking), OR (b) the dispatch worker has just claimed
// ownership via its own ZREM and is committed to submitting. In
// both cases Cancel cannot honor the request and the HTTP layer
// surfaces 410 Gone.
func (s *Service) Cancel(ctx context.Context, id, tenantID string) error {
	if strings.TrimSpace(id) == "" {
		return ErrNotFound
	}
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("undosend.Cancel: tenantID is required")
	}
	s.ensureCancelScript()
	raw, err := s.cancelScript.Run(
		ctx,
		s.client,
		[]string{companionKey(id), sortedSetKey},
		tenantID,
		id,
	).Result()
	if err != nil {
		return fmt.Errorf("undosend.Cancel: eval: %w", err)
	}
	result, _ := raw.(string)
	switch result {
	case "ok":
		return nil
	case "missing", "claimed":
		return ErrAlreadySent
	case "mismatch":
		return ErrTenantMismatch
	case "decode_error":
		return fmt.Errorf("undosend.Cancel: corrupt payload for id=%s", id)
	default:
		return fmt.Errorf("undosend.Cancel: unexpected script result %q", result)
	}
}

func (s *Service) readByID(ctx context.Context, id string) (*PendingSend, error) {
	raw, err := s.client.Get(ctx, companionKey(id)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("undosend: read pending send: %w", err)
	}
	var ps PendingSend
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, fmt.Errorf("undosend: decode pending send: %w", err)
	}
	return &ps, nil
}

func companionKey(id string) string {
	return keyPrefix + id
}

// dueIDs returns up to `limit` pending send ids whose deadline
// has passed at the supplied `now`. Used by the dispatch worker;
// not part of the public surface but kept package-private so the
// worker test can exercise it without exposing Redis details to
// other packages.
func (s *Service) dueIDs(ctx context.Context, now time.Time, limit int64) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.client.ZRangeByScore(ctx, sortedSetKey, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    fmt.Sprintf("%d", now.Unix()),
		Offset: 0,
		Count:  limit,
	}).Result()
}

// claim removes id from the sorted set in a way that guarantees
// at most one worker processes it; returns true if this caller
// is the owner. Implemented as ZREM (atomic) — Redis ZREM returns
// the number of removed members, so the worker that observes a
// non-zero return is the unique owner.
func (s *Service) claim(ctx context.Context, id string) (bool, error) {
	removed, err := s.client.ZRem(ctx, sortedSetKey, id).Result()
	if err != nil {
		return false, fmt.Errorf("undosend.claim: %w", err)
	}
	return removed > 0, nil
}

// markDispatched is the success path: clean both keys. The
// companion key's TTL would clear it eventually but we want the
// happy-path footprint to be as small as possible.
func (s *Service) markDispatched(ctx context.Context, id string) error {
	if err := s.client.Del(ctx, companionKey(id)).Err(); err != nil {
		return fmt.Errorf("undosend.markDispatched: %w", err)
	}
	return nil
}

// markFailed pushes the payload onto a parallel list for operator
// inspection and DELs the companion key. The sorted-set entry is
// gone by the time we get here (claim() removed it).
func (s *Service) markFailed(ctx context.Context, ps *PendingSend, lastErr string) error {
	ps.Status = StatusFailed
	ps.LastError = lastErr
	payload, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("undosend.markFailed: marshal: %w", err)
	}
	pipe := s.client.TxPipeline()
	pipe.LPush(ctx, failedListKey, payload)
	pipe.LTrim(ctx, failedListKey, 0, 999) // bound the dead-letter footprint at 1k entries
	pipe.Del(ctx, companionKey(ps.ID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("undosend.markFailed: pipeline: %w", err)
	}
	return nil
}

// requeue puts a still-retriable row back into the sorted set
// with a delayed deadline. Used by the dispatch worker on
// transient Stalwart failures.
func (s *Service) requeue(ctx context.Context, ps *PendingSend, nextDeadline time.Time) error {
	ps.DeadlineAt = nextDeadline.UTC()
	payload, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("undosend.requeue: marshal: %w", err)
	}
	keyTTL := time.Until(ps.DeadlineAt) + s.ttlSlack
	if keyTTL <= 0 {
		keyTTL = s.ttlSlack
	}
	pipe := s.client.TxPipeline()
	pipe.Set(ctx, companionKey(ps.ID), payload, keyTTL)
	pipe.ZAdd(ctx, sortedSetKey, redis.Z{
		Score:  float64(ps.DeadlineAt.Unix()),
		Member: ps.ID,
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("undosend.requeue: pipeline: %w", err)
	}
	return nil
}
