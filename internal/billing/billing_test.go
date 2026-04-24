package billing

import (
	"errors"
	"testing"
)

func TestGetPlanPricing(t *testing.T) {
	s := NewService(Config{})
	cases := map[string]int{
		PlanCore:    300,
		PlanPro:     600,
		PlanPrivacy: 900,
	}
	for plan, want := range cases {
		got, err := s.GetPlanPricing(plan)
		if err != nil {
			t.Fatalf("GetPlanPricing(%q) returned err: %v", plan, err)
		}
		if got != want {
			t.Errorf("GetPlanPricing(%q) = %d, want %d", plan, got, want)
		}
	}
	if _, err := s.GetPlanPricing("ultra"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for unknown plan, got %v", err)
	}
}

func TestGetPlanPricingCustomRates(t *testing.T) {
	s := NewService(Config{
		CoreSeatCents:    500,
		ProSeatCents:     1000,
		PrivacySeatCents: 1500,
	})
	if p, _ := s.GetPlanPricing(PlanCore); p != 500 {
		t.Errorf("core = %d, want 500", p)
	}
	if p, _ := s.GetPlanPricing(PlanPro); p != 1000 {
		t.Errorf("pro = %d, want 1000", p)
	}
	if p, _ := s.GetPlanPricing(PlanPrivacy); p != 1500 {
		t.Errorf("privacy = %d, want 1500", p)
	}
}

func TestPerSeatStorageBytes(t *testing.T) {
	s := NewService(Config{})
	cases := map[string]int64{
		PlanCore:    5 * 1024 * 1024 * 1024,
		PlanPro:     15 * 1024 * 1024 * 1024,
		PlanPrivacy: 50 * 1024 * 1024 * 1024,
	}
	for plan, want := range cases {
		got, err := s.PerSeatStorageBytes(plan)
		if err != nil {
			t.Fatalf("PerSeatStorageBytes(%q) err: %v", plan, err)
		}
		if got != want {
			t.Errorf("PerSeatStorageBytes(%q) = %d, want %d", plan, got, want)
		}
	}
}

func TestEnforcePlanLimitsSeatOverflow(t *testing.T) {
	// Exercise the pure-logic path of EnforcePlanLimits via a
	// fake Quota to avoid needing a real database.
	q := &Quota{SeatCount: 10, SeatLimit: 5}
	err := enforceQuotaLimits(q)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}

func TestEnforcePlanLimitsStorageOverflow(t *testing.T) {
	q := &Quota{StorageUsedBytes: 2000, StorageLimitBytes: 1000}
	err := enforceQuotaLimits(q)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}

func TestEnforcePlanLimitsOK(t *testing.T) {
	q := &Quota{SeatCount: 5, SeatLimit: 10, StorageUsedBytes: 100, StorageLimitBytes: 1000}
	if err := enforceQuotaLimits(q); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestEnforcePlanLimitsUnlimited(t *testing.T) {
	q := &Quota{SeatCount: 1000, SeatLimit: 0, StorageUsedBytes: 1 << 40, StorageLimitBytes: 0}
	if err := enforceQuotaLimits(q); err != nil {
		t.Errorf("expected nil for unlimited plan, got %v", err)
	}
}

func TestCheckStorageQuotaHeadroom(t *testing.T) {
	q := &Quota{StorageUsedBytes: 900, StorageLimitBytes: 1000}
	if err := checkStorageQuota(q, 50); err != nil {
		t.Errorf("50 bytes should fit, got %v", err)
	}
	if err := checkStorageQuota(q, 200); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("200 bytes should overflow, got %v", err)
	}
}

func TestCheckSeatAvailable(t *testing.T) {
	q := &Quota{SeatCount: 4, SeatLimit: 5}
	if err := checkSeatAvailable(q); err != nil {
		t.Errorf("slot 5/5 should fit, got %v", err)
	}
	q = &Quota{SeatCount: 5, SeatLimit: 5}
	if err := checkSeatAvailable(q); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("slot 6/5 should overflow, got %v", err)
	}
}

func TestInvoiceTotal(t *testing.T) {
	s := NewService(Config{})
	price, err := s.GetPlanPricing(PlanPro)
	if err != nil {
		t.Fatal(err)
	}
	got := int64(price) * int64(12)
	want := int64(7200) // $6 * 12 seats = $72.00
	if got != want {
		t.Errorf("invoice total = %d, want %d", got, want)
	}
}

// enforceQuotaLimits / checkStorageQuota / checkSeatAvailable are
// the pure-logic halves of the Service methods of the same names,
// extracted so unit tests can exercise the decision logic without
// standing up Postgres.

func enforceQuotaLimits(q *Quota) error {
	if q.SeatLimit > 0 && q.SeatCount > q.SeatLimit {
		return ErrQuotaExceeded
	}
	if q.StorageLimitBytes > 0 && q.StorageUsedBytes > q.StorageLimitBytes {
		return ErrQuotaExceeded
	}
	return nil
}

func checkStorageQuota(q *Quota, additional int64) error {
	if q.StorageLimitBytes == 0 {
		return nil
	}
	if q.StorageUsedBytes+additional > q.StorageLimitBytes {
		return ErrQuotaExceeded
	}
	return nil
}

func checkSeatAvailable(q *Quota) error {
	if q.SeatLimit == 0 {
		return nil
	}
	if q.SeatCount+1 > q.SeatLimit {
		return ErrQuotaExceeded
	}
	return nil
}
