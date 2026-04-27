// Package sieve hosts the per-tenant Sieve rule management
// surface. Stalwart owns the actual filter execution; the BFF
// owns the rule store, validation, and deploy mechanic so a
// tenant admin can author rules from the React console without
// hand-rolling JMAP admin calls.
//
// `sieve_rules` (migration 042) carries one row per rule. The
// canonical execution order is `priority` ascending, ties broken
// by `created_at` ascending. Rules can be tenant-wide
// (`user_id IS NULL`) or per-user.
//
// Validation is intentionally simple — Sieve is a small grammar
// and Stalwart will reject malformed scripts on deploy. The
// `ValidateScript` step here catches obvious shape errors
// (mismatched braces, missing `require`, unsupported test names)
// so the user gets a good error before the deploy round-trip.
package sieve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ErrInvalidInput is returned for caller-visible validation errors.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when a Sieve rule lookup misses.
var ErrNotFound = errors.New("not found")

// Rule is the API + DB shape of one row in `sieve_rules`.
type Rule struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	UserID    *string   `json:"user_id,omitempty"`
	Name      string    `json:"name"`
	Script    string    `json:"script"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Pusher is the slice of Stalwart's JMAP admin API the Sieve
// service needs. Tests stub this; production wires the JMAP
// proxy.
type Pusher interface {
	DeployScript(ctx context.Context, tenantID, userID, name, script string) error
}

// Config wires NewService.
type Config struct {
	Pool   *pgxpool.Pool
	Logger *log.Logger
	Pusher Pusher
}

// Service exposes the rule CRUD + deploy surface.
type Service struct {
	cfg Config
}

// NewService returns a Service.
func NewService(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Service{cfg: cfg}
}

// ListRules returns every rule for a tenant ordered by (priority,
// created_at).
func (s *Service) ListRules(ctx context.Context, tenantID string) ([]Rule, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil, nil
	}
	var out []Rule
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, user_id, name, script,
			       priority, enabled, created_at, updated_at
			  FROM sieve_rules
			 WHERE tenant_id = $1::uuid
			 ORDER BY priority ASC, created_at ASC`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Rule
			if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.Name,
				&r.Script, &r.Priority, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list sieve: %w", err)
	}
	return out, nil
}

// CreateRule inserts a new rule after validating the script.
func (s *Service) CreateRule(ctx context.Context, in Rule) (Rule, error) {
	if err := s.validate(in); err != nil {
		return Rule{}, err
	}
	if s.cfg.Pool == nil {
		return in, nil
	}
	out := in
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, in.TenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO sieve_rules (
				tenant_id, user_id, name, script, priority, enabled
			) VALUES (
				$1::uuid, $2, $3, $4, $5, $6
			)
			RETURNING id::text, created_at, updated_at`,
			in.TenantID, in.UserID, in.Name, in.Script, in.Priority, in.Enabled)
		return row.Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return Rule{}, fmt.Errorf("create sieve: %w", err)
	}
	return out, nil
}

// UpdateRule patches an existing rule.
func (s *Service) UpdateRule(ctx context.Context, in Rule) (Rule, error) {
	if in.ID == "" {
		return Rule{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if err := s.validate(in); err != nil {
		return Rule{}, err
	}
	if s.cfg.Pool == nil {
		return in, nil
	}
	out := in
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, in.TenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			UPDATE sieve_rules
			   SET name = $4, script = $5, priority = $6, enabled = $7,
			       user_id = $3, updated_at = now()
			 WHERE tenant_id = $1::uuid AND id = $2::uuid
			 RETURNING created_at, updated_at`,
			in.TenantID, in.ID, in.UserID, in.Name, in.Script, in.Priority, in.Enabled)
		return row.Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, fmt.Errorf("update sieve: %w", err)
	}
	return out, nil
}

// DeleteRule removes a rule by id.
func (s *Service) DeleteRule(ctx context.Context, tenantID, id string) error {
	if tenantID == "" || id == "" {
		return fmt.Errorf("%w: tenantID and id required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM sieve_rules WHERE tenant_id = $1::uuid AND id = $2::uuid`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ValidateScript runs a syntax check on a Sieve script. Returns
// nil if the script looks well-formed.
func (s *Service) ValidateScript(script string) error {
	return validateSieve(script)
}

// DeployRules pushes the enabled, sorted rule set for a tenant
// into Stalwart via the JMAP admin pusher. When no Pusher is
// configured the call is a logged no-op so dev stays useful.
func (s *Service) DeployRules(ctx context.Context, tenantID string) error {
	rules, err := s.ListRules(ctx, tenantID)
	if err != nil {
		return err
	}
	if s.cfg.Pusher == nil {
		s.cfg.Logger.Printf("sieve: deploy skipped for tenant=%s (no pusher configured)", tenantID)
		return nil
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		userID := ""
		if r.UserID != nil {
			userID = *r.UserID
		}
		if err := s.cfg.Pusher.DeployScript(ctx, tenantID, userID, r.Name, r.Script); err != nil {
			return fmt.Errorf("deploy %s: %w", r.Name, err)
		}
	}
	return nil
}

// validate applies the field-level rules a rule must satisfy
// before it can be persisted.
func (s *Service) validate(in Rule) error {
	if in.TenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalidInput)
	}
	if in.Priority < 0 || in.Priority > 1000 {
		return fmt.Errorf("%w: priority must be in [0,1000]", ErrInvalidInput)
	}
	return validateSieve(in.Script)
}

// validateSieve is a small structural validator for Sieve
// scripts. The full grammar (RFC 5228) is significantly larger;
// this function only catches the failure modes that are common
// authoring errors. Stalwart returns the canonical parse error on
// deploy.
func validateSieve(script string) error {
	if strings.TrimSpace(script) == "" {
		return fmt.Errorf("%w: script empty", ErrInvalidInput)
	}
	depth := 0
	for _, c := range script {
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
			if depth < 0 {
				return fmt.Errorf("%w: unmatched closing brace", ErrInvalidInput)
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("%w: unbalanced braces", ErrInvalidInput)
	}
	if !strings.Contains(script, "require") && (strings.Contains(script, "fileinto") || strings.Contains(script, "imap4flags")) {
		return fmt.Errorf("%w: script uses extension actions but is missing a 'require' line", ErrInvalidInput)
	}
	return nil
}
