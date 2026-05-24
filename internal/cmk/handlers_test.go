package cmk

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestStatusForHSMRegisterErr pins the contract between
// RegisterHSMKey error classes and the HTTP status code the
// admin handler returns. The mapping is operator-facing — a wrong
// status sends a client (and on-call) chasing the wrong fix.
func TestStatusForHSMRegisterErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "plan ineligible is 403 Forbidden",
			err:  ErrPlanNotEligible,
			want: http.StatusForbidden,
		},
		{
			name: "envelope not configured is 503 Service Unavailable",
			err:  ErrEnvelopeNotConfigured,
			want: http.StatusServiceUnavailable,
		},
		{
			name: "wrapped envelope error still resolves to 503",
			err:  fmt.Errorf("register: %w", ErrEnvelopeNotConfigured),
			want: http.StatusServiceUnavailable,
		},
		{
			name: "wrapped plan error still resolves to 403",
			err:  fmt.Errorf("register: %w", ErrPlanNotEligible),
			want: http.StatusForbidden,
		},
		{
			name: "unknown error falls back to 400 Bad Request",
			err:  errors.New("invalid endpoint"),
			want: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := statusForHSMRegisterErr(tc.err)
			if got != tc.want {
				t.Errorf("statusForHSMRegisterErr(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
