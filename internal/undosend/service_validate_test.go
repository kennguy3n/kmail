package undosend

import (
	"context"
	"testing"
)

func TestDelayAccessor(t *testing.T) {
	svc, _, _ := newTestService(t)
	if svc.Delay() <= 0 {
		t.Errorf("Delay() = %v, want positive", svc.Delay())
	}
}

// TestValidateHoldRequiredFields walks every required-field branch.
func TestValidateHoldRequiredFields(t *testing.T) {
	full := HoldInput{
		TenantID:          "t",
		KChatUserID:       "k",
		StalwartAccountID: "a",
		EmailID:           "e",
		CreateID:          "c",
		SubmissionPayload: []byte(`{}`),
	}
	if err := validateHold(full); err != nil {
		t.Fatalf("full input should validate: %v", err)
	}

	cases := map[string]func(*HoldInput){
		"tenant":     func(h *HoldInput) { h.TenantID = " " },
		"kchat":      func(h *HoldInput) { h.KChatUserID = "" },
		"stalwart":   func(h *HoldInput) { h.StalwartAccountID = "" },
		"email":      func(h *HoldInput) { h.EmailID = "" },
		"submission": func(h *HoldInput) { h.SubmissionPayload = nil },
	}
	for name, mutate := range cases {
		in := full
		mutate(&in)
		if err := validateHold(in); err == nil {
			t.Errorf("missing %s should error", name)
		}
	}
}

// TestHoldRejectsInvalidInput ensures Hold surfaces validation errors
// before touching Redis.
func TestHoldRejectsInvalidInput(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Hold(context.Background(), HoldInput{}); err == nil {
		t.Fatal("Hold with empty input should error")
	}
}
