package priority

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"

	"github.com/kennguy3n/kmail/internal/smartfeatures"
)

// SendHistory supplies the user's per-recipient send count, used
// as the "in contacts" / reply-history signal. Optional — when
// nil, those signals simply don't fire.
type SendHistory interface {
	SendCount(ctx context.Context, tenantID, userID, sender string) (float64, error)
}

// Service computes and caches the priority ranking for a user.
type Service struct {
	source  Source
	history SendHistory
	store   *Store
	logger  *log.Logger
}

// Config wires the service. Source is required; history and store
// are optional (a nil store disables caching, a nil history
// disables the contact/reply signals).
type Config struct {
	Source  Source
	History SendHistory
	Store   *Store
	Logger  *log.Logger
}

// NewService constructs the service. A nil Source is a wiring bug
// and errors.
func NewService(cfg Config) (*Service, error) {
	if cfg.Source == nil {
		return nil, errors.New("priority.NewService: Source is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{source: cfg.Source, history: cfg.History, store: cfg.Store, logger: logger}, nil
}

// defaultWindow is how many recent messages are scored per call.
const defaultWindow = 100

// Compute ranks the user's recent inbox window and returns the top
// `limit` messages by score (highest first). It also refreshes the
// Valkey cache as a side effect so the next read can be served from
// cache. A best-effort cache write failure is logged, not fatal.
func (s *Service) Compute(ctx context.Context, tenantID, userID string, limit int) ([]Scored, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" {
		return nil, errors.New("priority.Compute: tenantID and userID are required")
	}
	if limit <= 0 {
		limit = 20
	}

	msgs, err := s.source.ListInbox(ctx, tenantID, userID, defaultWindow)
	if err != nil {
		return nil, err
	}

	userAddr, err := s.source.UserAddress(ctx, tenantID, userID)
	if err != nil {
		// Best-effort: proceed without same-tenant / @mention signals.
		s.logger.Printf("priority: resolve user address: %v", err)
		userAddr = ""
	}
	userAddr = strings.ToLower(strings.TrimSpace(userAddr))
	userDomain := domainOf(userAddr)
	userLocal := localPartOf(userAddr)

	windowCount := senderWindowCounts(msgs)

	scored := make([]Scored, 0, len(msgs))
	for _, m := range msgs {
		sig := s.signalsFor(ctx, tenantID, userID, m, windowCount, userAddr, userLocal, userDomain)
		scored = append(scored, Scored{Message: m, Score: Score(sig)})
	}

	// Stable, deterministic ordering: score desc, then most recent
	// first, then id for total determinism (so equal-score ties
	// don't reshuffle between calls).
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		if !scored[i].Message.ReceivedAt.Equal(scored[j].Message.ReceivedAt) {
			return scored[i].Message.ReceivedAt.After(scored[j].Message.ReceivedAt)
		}
		return scored[i].Message.ID < scored[j].Message.ID
	})

	if s.store != nil {
		if err := s.store.Save(ctx, tenantID, userID, scored); err != nil {
			s.logger.Printf("priority: cache save: %v", err)
		}
	}

	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

func (s *Service) signalsFor(ctx context.Context, tenantID, userID string, m smartfeatures.Message, windowCount map[string]int, userAddr, userLocal, userDomain string) Signals {
	sig := Signals{
		Unread:         !m.Keywords["$seen"],
		ThreadAnswered: m.Keywords["$answered"],
	}

	from, ok := m.FirstFrom()
	if ok {
		sender := from.Normalized()
		sig.InWindowCount = windowCount[sender]
		if s.history != nil {
			count, err := s.history.SendCount(ctx, tenantID, userID, sender)
			if err != nil {
				s.logger.Printf("priority: send count for %s: %v", sender, err)
			} else {
				sig.SenderSendCount = count
				sig.SenderInContacts = count > 0
			}
		}
		if userDomain != "" && from.Domain() == userDomain {
			sig.SameTenant = true
		}
	}

	if userAddr != "" {
		sig.MentionsUser = mentionsUser(m, userAddr, userLocal)
	}
	return sig
}

// mentionsUser reports whether the user is named in the subject or
// preview — either their full address or, as a softer signal, the
// "@local-part" mention form common in chat-style notifications.
func mentionsUser(m smartfeatures.Message, userAddr, userLocal string) bool {
	hay := strings.ToLower(m.Subject + " " + m.Preview)
	if userAddr != "" && strings.Contains(hay, userAddr) {
		return true
	}
	if userLocal != "" && strings.Contains(hay, "@"+userLocal) {
		return true
	}
	return false
}

// senderWindowCounts tallies how many messages each sender
// contributes to the window.
func senderWindowCounts(msgs []smartfeatures.Message) map[string]int {
	counts := make(map[string]int, len(msgs))
	for _, m := range msgs {
		if from, ok := m.FirstFrom(); ok {
			counts[from.Normalized()]++
		}
	}
	return counts
}

func domainOf(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return addr[at+1:]
}

func localPartOf(addr string) string {
	at := strings.Index(addr, "@")
	if at <= 0 {
		return ""
	}
	return addr[:at]
}
