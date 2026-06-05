package smartfeatures

import (
	"regexp"
	"strings"
)

// ReplyKind classifies a smart-reply suggestion so the frontend
// can style / order chips consistently and so tests assert on the
// rule that fired rather than the (tweakable) display text.
type ReplyKind string

const (
	ReplyAffirm  ReplyKind = "affirm"
	ReplyDecline ReplyKind = "decline"
	ReplyDefer   ReplyKind = "defer"
	ReplyAck     ReplyKind = "ack"
	ReplyAttend  ReplyKind = "attend"
)

// Suggestion is a single smart-reply chip.
type Suggestion struct {
	Text string    `json:"text"`
	Kind ReplyKind `json:"kind"`
}

// maxSuggestions caps how many chips a single email yields. Gmail
// shows three; we follow that so the compose toolbar never wraps.
const maxSuggestions = 3

var (
	// A trailing "?" or a leading interrogative is a strong signal
	// the sender is asking something that wants a yes/no/defer.
	questionLeadRe = regexp.MustCompile(`(?i)\b(can|could|would|will|are|is|do|does|did|should|may|have|has)\b`)

	// Meeting / scheduling intent. Kept deliberately broad — false
	// positives here just add an extra chip the user can ignore.
	meetingRe = regexp.MustCompile(`(?i)\b(meeting|meet|call|sync|catch[- ]?up|calendar|invite|schedule|reschedule|appointment|stand[- ]?up|zoom|hangout|google meet|teams call)\b`)

	// Gratitude — the sender is thanking the recipient, so the
	// natural replies are "you're welcome" style acknowledgements.
	thanksRe = regexp.MustCompile(`(?i)\b(thank you|thanks|thx|much appreciated|appreciate it|grateful)\b`)

	// A direct request to do something ("please review", "let me
	// know", "can you send"). Distinct from a yes/no question.
	requestRe = regexp.MustCompile(`(?i)\b(please|could you|can you|would you|let me know|lmk|kindly|need you to)\b`)
)

// SuggestReplies returns up to three rule-based reply chips for a
// message, ordered most-relevant first. It never returns nil — a
// message that matches no specific intent still yields generic
// acknowledgements so the UI always has something to show.
//
// The rules are intentionally additive and de-duplicated: a
// message that both asks a question and proposes a meeting gets
// meeting-flavoured chips first (the more specific intent) then
// falls back to yes/no, capped at maxSuggestions.
func SuggestReplies(m Message) []Suggestion {
	text := m.searchText()

	var out []Suggestion
	seen := map[string]bool{}
	add := func(s Suggestion) {
		key := strings.ToLower(s.Text)
		if seen[key] || len(out) >= maxSuggestions {
			return
		}
		seen[key] = true
		out = append(out, s)
	}

	isQuestion := strings.Contains(text, "?") || questionLeadRe.MatchString(text)

	// Order matters: the most specific intent contributes its chips
	// first so they survive the maxSuggestions cap.
	switch {
	case meetingRe.MatchString(text):
		add(Suggestion{Text: "That works for me.", Kind: ReplyAttend})
		add(Suggestion{Text: "I'll be there.", Kind: ReplyAttend})
		add(Suggestion{Text: "Can we reschedule?", Kind: ReplyDefer})
	case thanksRe.MatchString(text):
		add(Suggestion{Text: "You're welcome!", Kind: ReplyAck})
		add(Suggestion{Text: "Happy to help!", Kind: ReplyAck})
		add(Suggestion{Text: "Anytime!", Kind: ReplyAck})
	case isQuestion:
		add(Suggestion{Text: "Yes, I can do that.", Kind: ReplyAffirm})
		add(Suggestion{Text: "No, unfortunately I can't.", Kind: ReplyDecline})
		add(Suggestion{Text: "Let me check and get back to you.", Kind: ReplyDefer})
	case requestRe.MatchString(text):
		add(Suggestion{Text: "Sure, I'll take care of it.", Kind: ReplyAffirm})
		add(Suggestion{Text: "Let me check and get back to you.", Kind: ReplyDefer})
		add(Suggestion{Text: "Thanks for the heads up!", Kind: ReplyAck})
	}

	// Generic fallback acknowledgements so the chip row is never
	// empty. These fill any remaining slots after a specific intent
	// fired, and carry the whole result when nothing matched.
	add(Suggestion{Text: "Thanks for your email!", Kind: ReplyAck})
	add(Suggestion{Text: "Got it, thanks!", Kind: ReplyAck})
	add(Suggestion{Text: "Sounds good.", Kind: ReplyAffirm})

	return out
}
