// Package priority implements KMail's Priority Inbox: a per-user
// ranking of recent mail by how likely the user is to care about
// it. Scoring is rule-based and deterministic — every signal is
// derived from message metadata and the user's own send history
// (see internal/smartfeatures.ContactTracker), so there is no ML
// dependency and the scorer is a pure function that unit-tests
// without any I/O.
//
// Scores are cached per-user in Valkey (store.go) as an ephemeral
// sorted set so the GET /api/v1/priority-inbox handler can answer
// from cache and a recompute can refresh it in the background.
package priority

import "github.com/kennguy3n/kmail/internal/smartfeatures"

// Signals are the per-message inputs to the score. The service
// populates them best-effort from the JMAP message, the user's
// send history, and the tenant's domain set; any signal the
// service can't determine is simply left false/zero, which the
// scorer treats as "no contribution" rather than a penalty.
type Signals struct {
	// SenderInContacts is true when the user has previously emailed
	// this sender (a strong "I know and correspond with them"
	// signal, sourced from the frequent-contacts tracker).
	SenderInContacts bool
	// SenderSendCount is how many times the user has emailed this
	// sender — a proxy for reply history / relationship strength.
	SenderSendCount float64
	// InWindowCount is how many messages from this sender are in the
	// current inbox window. Frequent recent senders rank higher.
	InWindowCount int
	// SameTenant is true when the sender's domain belongs to the
	// user's own tenant (internal mail is usually higher priority).
	SameTenant bool
	// MentionsUser is true when the user is @mentioned or named in
	// the subject/preview.
	MentionsUser bool
	// ThreadAnswered is true when the message carries the $answered
	// keyword — an ongoing conversation the user is part of.
	ThreadAnswered bool
	// Unread weights unread mail slightly above already-read mail.
	Unread bool
}

// Score weights. Exported so tests pin the exact contribution of
// each signal and a future tuning PR changes one named constant
// rather than a magic number buried in the sum.
const (
	WeightInContacts   = 30
	WeightSameTenant   = 20
	WeightMentionsUser = 25
	WeightAnswered     = 15
	WeightUnread       = 5

	// SenderSendCount and InWindowCount are logarithmic-ish: the
	// first few interactions matter most, with a hard cap so a
	// runaway counter can't dominate the score.
	MaxSendCountPoints = 15
	MaxWindowPoints    = 10

	// MaxScore is the clamp ceiling so the score is always a clean
	// 0..100 the UI can render as a percentage / heat bar.
	MaxScore = 100
)

// Score computes a 0..100 priority score from the signals. The
// function is pure and monotonic in every signal (adding a signal
// never lowers the score), which keeps the ranking explainable.
func Score(s Signals) int {
	score := 0
	if s.SenderInContacts {
		score += WeightInContacts
	}
	if s.SameTenant {
		score += WeightSameTenant
	}
	if s.MentionsUser {
		score += WeightMentionsUser
	}
	if s.ThreadAnswered {
		score += WeightAnswered
	}
	if s.Unread {
		score += WeightUnread
	}
	score += sendCountPoints(s.SenderSendCount)
	score += windowPoints(s.InWindowCount)

	if score > MaxScore {
		score = MaxScore
	}
	if score < 0 {
		score = 0
	}
	return score
}

// sendCountPoints maps a raw send count to a capped contribution
// with diminishing returns: 1→~5, 3→~10, 7+→15.
func sendCountPoints(count float64) int {
	switch {
	case count <= 0:
		return 0
	case count < 2:
		return 5
	case count < 5:
		return 10
	default:
		return MaxSendCountPoints
	}
}

// windowPoints rewards senders who appear repeatedly in the recent
// window, capped so a noisy automated sender can't outrank a real
// correspondent on volume alone.
func windowPoints(count int) int {
	switch {
	case count <= 1:
		return 0
	case count < 4:
		return 5
	default:
		return MaxWindowPoints
	}
}

// Scored pairs a message with its computed score for ranking and
// for the API response.
type Scored struct {
	Message smartfeatures.Message
	Score   int
}
