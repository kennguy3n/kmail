package priority

import "testing"

func TestScore_EmptyIsZero(t *testing.T) {
	if got := Score(Signals{}); got != 0 {
		t.Fatalf("Score(empty) = %d, want 0", got)
	}
}

func TestScore_Monotonic(t *testing.T) {
	// Adding any single signal must never lower the score.
	base := Signals{}
	baseScore := Score(base)
	mutations := []func(*Signals){
		func(s *Signals) { s.SenderInContacts = true },
		func(s *Signals) { s.SameTenant = true },
		func(s *Signals) { s.MentionsUser = true },
		func(s *Signals) { s.ThreadAnswered = true },
		func(s *Signals) { s.Unread = true },
		func(s *Signals) { s.SenderSendCount = 10 },
		func(s *Signals) { s.InWindowCount = 10 },
	}
	for i, mut := range mutations {
		s := base
		mut(&s)
		if Score(s) < baseScore {
			t.Fatalf("mutation %d lowered score below base", i)
		}
	}
}

func TestScore_Clamp(t *testing.T) {
	all := Signals{
		SenderInContacts: true,
		SenderSendCount:  100,
		InWindowCount:    100,
		SameTenant:       true,
		MentionsUser:     true,
		ThreadAnswered:   true,
		Unread:           true,
	}
	if got := Score(all); got != MaxScore {
		t.Fatalf("Score(all) = %d, want clamp %d", got, MaxScore)
	}
}

func TestScore_KnownContributions(t *testing.T) {
	// In contacts + mentions user, nothing else.
	s := Signals{SenderInContacts: true, MentionsUser: true}
	want := WeightInContacts + WeightMentionsUser
	if got := Score(s); got != want {
		t.Fatalf("Score = %d, want %d", got, want)
	}
}

func TestSendCountPoints(t *testing.T) {
	cases := []struct {
		count float64
		want  int
	}{{0, 0}, {1, 5}, {3, 10}, {7, MaxSendCountPoints}}
	for _, c := range cases {
		if got := sendCountPoints(c.count); got != c.want {
			t.Fatalf("sendCountPoints(%v) = %d, want %d", c.count, got, c.want)
		}
	}
}

func TestWindowPoints(t *testing.T) {
	cases := []struct {
		count int
		want  int
	}{{0, 0}, {1, 0}, {2, 5}, {3, 5}, {4, MaxWindowPoints}, {10, MaxWindowPoints}}
	for _, c := range cases {
		if got := windowPoints(c.count); got != c.want {
			t.Fatalf("windowPoints(%d) = %d, want %d", c.count, got, c.want)
		}
	}
}
