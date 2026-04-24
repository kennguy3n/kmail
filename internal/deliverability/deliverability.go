// Package deliverability hosts the Deliverability Control Plane
// business logic: IP pool manager, warmup scheduler, suppression
// lists, bounce processor, DMARC report ingester, Gmail
// Postmaster / Yahoo feedback loop consumers, abuse scoring, and
// compromised-account detection.
//
// See docs/ARCHITECTURE.md §7 and docs/PROPOSAL.md §9.
package deliverability

import (
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Config wires the Deliverability Control Plane services.
type Config struct {
	Pool   *pgxpool.Pool
	Valkey *redis.Client
	Logger *log.Logger

	// Plan-based daily send caps. Hourly caps derive as daily/10.
	CoreDailyLimit    int
	ProDailyLimit     int
	PrivacyDailyLimit int

	// WarmupDays is the ramp length — day 1 starts at a small
	// fraction of the cap, day `WarmupDays` reaches the plan cap.
	WarmupDays int

	// BounceSoftEscalationCount / BounceSoftWindow drive the rule
	// that escalates persistent soft-bounce recipients onto the
	// suppression list.
	BounceSoftEscalationCount int
	BounceSoftWindow          time.Duration
}

// Service bundles the Deliverability sub-services so the BFF only
// has to construct one root object.
type Service struct {
	cfg Config

	Suppression *SuppressionService
	Bounce      *BounceProcessor
	IPPool      *IPPoolService
	SendLimit   *SendLimitService
	Warmup      *WarmupScheduler
	DMARC       *DMARCService
}

// NewService builds every sub-service from a Config. Defaults are
// applied up-front so callers cannot forget to set, e.g., the
// warmup ramp length.
func NewService(cfg Config) *Service {
	if cfg.WarmupDays <= 0 {
		cfg.WarmupDays = 30
	}
	if cfg.CoreDailyLimit <= 0 {
		cfg.CoreDailyLimit = 500
	}
	if cfg.ProDailyLimit <= 0 {
		cfg.ProDailyLimit = 2000
	}
	if cfg.PrivacyDailyLimit <= 0 {
		cfg.PrivacyDailyLimit = 5000
	}
	if cfg.BounceSoftEscalationCount <= 0 {
		cfg.BounceSoftEscalationCount = 3
	}
	if cfg.BounceSoftWindow <= 0 {
		cfg.BounceSoftWindow = 72 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	s := &Service{cfg: cfg}
	s.Suppression = &SuppressionService{pool: cfg.Pool}
	s.Bounce = &BounceProcessor{
		pool:              cfg.Pool,
		suppression:       s.Suppression,
		softEscalateCount: cfg.BounceSoftEscalationCount,
		softEscalateWin:   cfg.BounceSoftWindow,
	}
	s.IPPool = &IPPoolService{pool: cfg.Pool}
	s.SendLimit = &SendLimitService{
		pool:              cfg.Pool,
		valkey:            cfg.Valkey,
		coreDaily:         cfg.CoreDailyLimit,
		proDaily:          cfg.ProDailyLimit,
		privacyDaily:      cfg.PrivacyDailyLimit,
	}
	s.Warmup = &WarmupScheduler{
		pool:       cfg.Pool,
		warmupDays: cfg.WarmupDays,
		sendLimit:  s.SendLimit,
	}
	s.DMARC = &DMARCService{pool: cfg.Pool}
	return s
}

// ErrNotFound is returned when a row lookup resolves nothing.
var ErrNotFound = errors.New("not found")

// ErrInvalidInput wraps caller-visible validation failures.
var ErrInvalidInput = errors.New("invalid input")

// ErrSuppressed is returned when a recipient is on the suppression
// list.
var ErrSuppressed = errors.New("recipient suppressed")

// ErrSendLimitExceeded is returned when a tenant is over its daily
// or hourly send cap.
var ErrSendLimitExceeded = errors.New("send limit exceeded")
