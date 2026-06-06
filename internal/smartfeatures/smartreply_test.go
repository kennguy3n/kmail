package smartfeatures

import "testing"

func kinds(sugs []Suggestion) []ReplyKind {
	out := make([]ReplyKind, len(sugs))
	for i, s := range sugs {
		out[i] = s.Kind
	}
	return out
}

func containsKind(sugs []Suggestion, k ReplyKind) bool {
	for _, s := range sugs {
		if s.Kind == k {
			return true
		}
	}
	return false
}

func TestSuggestReplies_NeverEmpty_AndCapped(t *testing.T) {
	cases := []Message{
		{},
		{Subject: "random subject with no intent", Preview: "hello there"},
		{Subject: "Can you review this?", Preview: "thanks so much, see you at the meeting"},
	}
	for _, m := range cases {
		got := SuggestReplies(m)
		if len(got) == 0 {
			t.Fatalf("SuggestReplies(%q) returned no suggestions", m.Subject)
		}
		if len(got) > maxSuggestions {
			t.Fatalf("SuggestReplies(%q) returned %d > cap %d", m.Subject, len(got), maxSuggestions)
		}
		// No duplicate text.
		seen := map[string]bool{}
		for _, s := range got {
			if seen[s.Text] {
				t.Fatalf("duplicate suggestion text %q for %q", s.Text, m.Subject)
			}
			seen[s.Text] = true
		}
	}
}

func TestSuggestReplies_Meeting(t *testing.T) {
	m := Message{Subject: "Quick sync tomorrow?", Preview: "Can we schedule a call?"}
	got := SuggestReplies(m)
	if !containsKind(got, ReplyAttend) {
		t.Fatalf("expected an attend suggestion for a meeting, got %v", kinds(got))
	}
}

func TestSuggestReplies_Thanks(t *testing.T) {
	m := Message{Subject: "Thank you!", Preview: "Thanks for the quick turnaround."}
	got := SuggestReplies(m)
	if !containsKind(got, ReplyAck) {
		t.Fatalf("expected an ack suggestion for a thank-you, got %v", kinds(got))
	}
	if got[0].Text != "You're welcome!" {
		t.Fatalf("expected first thanks reply to be 'You're welcome!', got %q", got[0].Text)
	}
}

func TestSuggestReplies_Question(t *testing.T) {
	m := Message{Subject: "Are you available Friday?", Preview: "Let me know."}
	got := SuggestReplies(m)
	// "available" + "?" → question intent, yields affirm/decline/defer.
	if !containsKind(got, ReplyAffirm) || !containsKind(got, ReplyDecline) {
		t.Fatalf("expected yes/no replies for a question, got %v", kinds(got))
	}
}

func TestSuggestReplies_MeetingBeatsQuestion(t *testing.T) {
	// A message that is both a question and a meeting should lead
	// with the more specific meeting intent.
	m := Message{Subject: "Can we reschedule our meeting?", Preview: ""}
	got := SuggestReplies(m)
	if got[0].Kind != ReplyAttend {
		t.Fatalf("expected meeting intent to win, got first kind %q (%v)", got[0].Kind, kinds(got))
	}
}
