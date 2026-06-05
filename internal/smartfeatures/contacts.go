package smartfeatures

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ContactTracker records, per user, how often each recipient is
// emailed and which recipients tend to be addressed together. Both
// are stored in Valkey sorted sets so the "frequent contacts" and
// "you usually CC X when emailing Y" features are O(log n) reads
// with no Postgres traffic on the hot compose path.
//
// Keyspace (all per-user, all TTL-refreshed on write):
//
//	kmail:freq_contacts:<tenant>:<user>          ZSET  score=send count, member=email
//	kmail:co_recipients:<tenant>:<user>:<email>  ZSET  score=co-send count, member=co-recipient email
//
// A TTL is applied so a user who stops sending mail eventually
// ages out of the cache — these are convenience signals, not a
// system of record, so losing them on expiry is acceptable (they
// rebuild on the next send).
type ContactTracker struct {
	client redis.UniversalClient
	ttl    time.Duration
}

// DefaultContactTTL bounds how long contact-frequency data lives
// without a refreshing send. 180 days keeps a year-round
// correspondent warm while letting one-off recipients lapse.
const DefaultContactTTL = 180 * 24 * time.Hour

// Contact is a frequent-contact entry returned to the UI.
type Contact struct {
	Email string  `json:"email"`
	Count float64 `json:"count"`
}

// NewContactTracker constructs a tracker. The Valkey client is
// required; ttl <= 0 falls back to DefaultContactTTL.
func NewContactTracker(client redis.UniversalClient, ttl time.Duration) (*ContactTracker, error) {
	if client == nil {
		return nil, errors.New("smartfeatures.NewContactTracker: client is required")
	}
	if ttl <= 0 {
		ttl = DefaultContactTTL
	}
	return &ContactTracker{client: client, ttl: ttl}, nil
}

func freqKey(tenantID, userID string) string {
	return fmt.Sprintf("kmail:freq_contacts:%s:%s", tenantID, userID)
}

func coKey(tenantID, userID, email string) string {
	return fmt.Sprintf("kmail:co_recipients:%s:%s:%s", tenantID, userID, email)
}

// RecordSend increments the send counters for every recipient of a
// sent message and, for each ordered pair, the co-recipient
// counter so co-send suggestions can be surfaced later. Addresses
// are normalized (lower-cased) and de-duplicated within the call
// so a message that lists the same address in To and Cc is counted
// once. A blank tenant/user is an error (the keys would collide
// across the fleet); an empty recipient list is a no-op.
func (t *ContactTracker) RecordSend(ctx context.Context, tenantID, userID string, recipients []string) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return errors.New("smartfeatures.RecordSend: tenantID and userID are required")
	}
	uniq := normalizeRecipients(recipients)
	if len(uniq) == 0 {
		return nil
	}

	pipe := t.client.Pipeline()
	fk := freqKey(tenantID, userID)
	for _, addr := range uniq {
		pipe.ZIncrBy(ctx, fk, 1, addr)
	}
	pipe.Expire(ctx, fk, t.ttl)

	// Co-recipient graph: for each recipient, bump every *other*
	// recipient on the same message. The relation is symmetric, so
	// both directions are recorded.
	for _, a := range uniq {
		ck := coKey(tenantID, userID, a)
		for _, b := range uniq {
			if a == b {
				continue
			}
			pipe.ZIncrBy(ctx, ck, 1, b)
		}
		pipe.Expire(ctx, ck, t.ttl)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("smartfeatures.RecordSend: pipeline exec: %w", err)
	}
	return nil
}

// TopContacts returns the user's most-emailed recipients, highest
// count first. n <= 0 returns an empty slice.
func (t *ContactTracker) TopContacts(ctx context.Context, tenantID, userID string, n int) ([]Contact, error) {
	if n <= 0 {
		return []Contact{}, nil
	}
	res, err := t.client.ZRevRangeWithScores(ctx, freqKey(tenantID, userID), 0, int64(n-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("smartfeatures.TopContacts: %w", err)
	}
	out := make([]Contact, 0, len(res))
	for _, z := range res {
		member, _ := z.Member.(string)
		out = append(out, Contact{Email: member, Count: z.Score})
	}
	return out, nil
}

// SendCount returns how many times the user has emailed the given
// address (0 when never). Used by the Priority Inbox scorer as a
// relationship-strength / reply-history proxy. A missing member
// surfaces as redis.Nil, which is reported as a 0 count rather
// than an error.
func (t *ContactTracker) SendCount(ctx context.Context, tenantID, userID, email string) (float64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return 0, nil
	}
	score, err := t.client.ZScore(ctx, freqKey(tenantID, userID), email).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("smartfeatures.SendCount: %w", err)
	}
	return score, nil
}

// SuggestCoRecipients returns the recipients most often emailed
// together with the given anchor address — the data behind the
// "you usually CC X when emailing Y" hint. Results exclude any
// address already on the draft (in alreadyAdded) and the anchor
// itself.
func (t *ContactTracker) SuggestCoRecipients(ctx context.Context, tenantID, userID, anchor string, alreadyAdded []string, n int) ([]Contact, error) {
	if n <= 0 {
		return []Contact{}, nil
	}
	anchor = strings.ToLower(strings.TrimSpace(anchor))
	if anchor == "" {
		return []Contact{}, nil
	}
	exclude := map[string]bool{anchor: true}
	for _, a := range alreadyAdded {
		exclude[strings.ToLower(strings.TrimSpace(a))] = true
	}

	// Over-fetch so we can drop excluded members and still return n.
	fetch := n + len(exclude)
	res, err := t.client.ZRevRangeWithScores(ctx, coKey(tenantID, userID, anchor), 0, int64(fetch-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("smartfeatures.SuggestCoRecipients: %w", err)
	}
	out := make([]Contact, 0, n)
	for _, z := range res {
		member, _ := z.Member.(string)
		if exclude[member] {
			continue
		}
		out = append(out, Contact{Email: member, Count: z.Score})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// normalizeRecipients lower-cases, trims, drops blanks/non-address
// values, and de-duplicates while preserving first-seen order so
// the co-recipient pairing is deterministic.
func normalizeRecipients(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || !strings.Contains(r, "@") {
			continue
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out) // stable pairing independent of header order
	return out
}
