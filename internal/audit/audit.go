// Package audit hosts the admin audit-log service (Phase 3 —
// Admin audit logs).
//
// Every administrative action writes a row into the `audit_log`
// table. The rows are linked by a `prev_hash` chain so operators
// can detect tampering: `entry_hash = SHA256(prev_hash + payload)`.
// The chain is tenant-scoped so one tenant's clock does not force
// another to re-verify.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ActorType enumerates the distinct principal types the audit log
// records. Admin-console writes use `admin`, BFF requests on
// behalf of an authenticated user use `user`, background job
// writes use `system`.
type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorAdmin  ActorType = "admin"
	ActorSystem ActorType = "system"
)

// Entry is the payload the service logs.
type Entry struct {
	TenantID     string            `json:"tenantId"`
	ActorID      string            `json:"actorId"`
	ActorType    ActorType         `json:"actorType"`
	Action       string            `json:"action"`
	ResourceType string            `json:"resourceType"`
	ResourceID   string            `json:"resourceId,omitempty"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
	IPAddress    string            `json:"ipAddress,omitempty"`
	UserAgent    string            `json:"userAgent,omitempty"`
	// populated after Log
	ID        string    `json:"id,omitempty"`
	PrevHash  string    `json:"prevHash,omitempty"`
	EntryHash string    `json:"entryHash,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// QueryFilters narrow a paginated audit-log query.
type QueryFilters struct {
	Action       string
	ActorID      string
	ResourceType string
	Since        time.Time
	Until        time.Time
	Limit        int
	Offset       int
}

// ErrInvalidInput wraps validation errors so handlers surface 400.
var ErrInvalidInput = errors.New("invalid input")

// ErrChainBroken is returned when VerifyChain detects a mismatch
// between a stored `entry_hash` and the recomputed hash.
var ErrChainBroken = errors.New("audit chain broken")

// Service persists and queries audit entries.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewService builds an audit Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// Log appends an entry to the tenant's audit chain. The
// `entry_hash` is computed from the canonical JSON serialisation
// of the payload plus the previous row's hash.
func (s *Service) Log(ctx context.Context, e Entry) (*Entry, error) {
	if e.TenantID == "" || e.Action == "" || e.ActorType == "" {
		return nil, fmt.Errorf("%w: tenantID, action, and actorType required", ErrInvalidInput)
	}
	if s.pool == nil {
		e.EntryHash = computeHash("", e)
		e.CreatedAt = s.now().UTC()
		return &e, nil
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, e.TenantID); err != nil {
			return err
		}
		var prevHash string
		err := tx.QueryRow(ctx, `
			SELECT entry_hash FROM audit_log
			WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC, id DESC
			LIMIT 1
		`, e.TenantID).Scan(&prevHash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		e.PrevHash = prevHash
		e.EntryHash = computeHash(prevHash, e)
		meta := e.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		metaJSON, _ := json.Marshal(meta)
		var ip any
		if e.IPAddress != "" {
			if parsed := net.ParseIP(e.IPAddress); parsed != nil {
				ip = parsed.String()
			}
		}
		return tx.QueryRow(ctx, `
			INSERT INTO audit_log
				(tenant_id, actor_id, actor_type, action, resource_type, resource_id,
				 metadata, ip_address, user_agent, prev_hash, entry_hash)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11)
			RETURNING id::text, created_at
		`, e.TenantID, e.ActorID, string(e.ActorType), e.Action, e.ResourceType, nullString(e.ResourceID),
			string(metaJSON), ip, nullString(e.UserAgent), prevHash, e.EntryHash).Scan(&e.ID, &e.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Query returns a paginated audit-log slice.
func (s *Service) Query(ctx context.Context, tenantID string, f QueryFilters) ([]Entry, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []Entry
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
			where []string
			args  []any
		)
		idx := 1
		add := func(clause string, val any) {
			where = append(where, fmt.Sprintf(clause, idx))
			args = append(args, val)
			idx++
		}
		if f.Action != "" {
			add("action = $%d", f.Action)
		}
		if f.ActorID != "" {
			add("actor_id = $%d", f.ActorID)
		}
		if f.ResourceType != "" {
			add("resource_type = $%d", f.ResourceType)
		}
		if !f.Since.IsZero() {
			add("created_at >= $%d", f.Since)
		}
		if !f.Until.IsZero() {
			add("created_at <= $%d", f.Until)
		}
		clause := ""
		if len(where) > 0 {
			clause = "WHERE " + strings.Join(where, " AND ")
		}
		args = append(args, f.Limit, f.Offset)
		query := fmt.Sprintf(`
			SELECT id::text, tenant_id::text, actor_id, actor_type, action, resource_type,
			       COALESCE(resource_id, ''), metadata, COALESCE(host(ip_address), ''),
			       COALESCE(user_agent, ''), prev_hash, entry_hash, created_at
			FROM audit_log
			%s
			ORDER BY created_at DESC, id DESC
			LIMIT $%d OFFSET $%d
		`, clause, idx, idx+1)
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Entry
			var actorType string
			var metaJSON []byte
			if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorID, &actorType, &e.Action,
				&e.ResourceType, &e.ResourceID, &metaJSON, &e.IPAddress, &e.UserAgent,
				&e.PrevHash, &e.EntryHash, &e.CreatedAt); err != nil {
				return err
			}
			e.ActorType = ActorType(actorType)
			if len(metaJSON) > 0 {
				_ = json.Unmarshal(metaJSON, &e.Metadata)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Export streams the audit log as JSON (array) or CSV (RFC 4180).
// `format` must be "json" or "csv". Time range is optional.
func (s *Service) Export(ctx context.Context, tenantID, format string, since, until time.Time) ([]byte, error) {
	entries, err := s.Query(ctx, tenantID, QueryFilters{Since: since, Until: until, Limit: 1000})
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(format) {
	case "json", "":
		return json.MarshalIndent(entries, "", "  ")
	case "csv":
		var b strings.Builder
		b.WriteString("id,created_at,actor_id,actor_type,action,resource_type,resource_id,entry_hash\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%s,%s\n",
				e.ID, e.CreatedAt.Format(time.RFC3339), e.ActorID, e.ActorType,
				e.Action, e.ResourceType, e.ResourceID, e.EntryHash)
		}
		return []byte(b.String()), nil
	default:
		return nil, fmt.Errorf("%w: unknown format %q", ErrInvalidInput, format)
	}
}

// VerifyChain walks every row for the tenant in creation order and
// recomputes each `entry_hash` from the previous row's hash. Any
// mismatch surfaces `ErrChainBroken` pointing at the offending ID.
func (s *Service) VerifyChain(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, actor_id, actor_type, action,
			       resource_type, COALESCE(resource_id, ''), metadata,
			       COALESCE(host(ip_address), ''), COALESCE(user_agent, ''),
			       prev_hash, entry_hash, created_at
			FROM audit_log
			ORDER BY created_at ASC, id ASC
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		prev := ""
		for rows.Next() {
			var e Entry
			var actorType string
			var metaJSON []byte
			if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorID, &actorType, &e.Action,
				&e.ResourceType, &e.ResourceID, &metaJSON, &e.IPAddress, &e.UserAgent,
				&e.PrevHash, &e.EntryHash, &e.CreatedAt); err != nil {
				return err
			}
			e.ActorType = ActorType(actorType)
			if len(metaJSON) > 0 {
				_ = json.Unmarshal(metaJSON, &e.Metadata)
			}
			if e.PrevHash != prev {
				return fmt.Errorf("%w: entry %s prev_hash mismatch", ErrChainBroken, e.ID)
			}
			if computeHash(prev, e) != e.EntryHash {
				return fmt.Errorf("%w: entry %s hash mismatch", ErrChainBroken, e.ID)
			}
			prev = e.EntryHash
		}
		return rows.Err()
	})
}

// computeHash returns the deterministic SHA-256 of prev + canonical
// payload. Metadata is serialised with sorted keys so two semantically
// equivalent maps hash identically.
func computeHash(prev string, e Entry) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{'|'})
	h.Write([]byte(e.TenantID))
	h.Write([]byte{'|'})
	h.Write([]byte(e.ActorID))
	h.Write([]byte{'|'})
	h.Write([]byte(e.ActorType))
	h.Write([]byte{'|'})
	h.Write([]byte(e.Action))
	h.Write([]byte{'|'})
	h.Write([]byte(e.ResourceType))
	h.Write([]byte{'|'})
	h.Write([]byte(e.ResourceID))
	h.Write([]byte{'|'})
	h.Write([]byte(canonicalJSON(e.Metadata)))
	h.Write([]byte{'|'})
	h.Write([]byte(e.IPAddress))
	h.Write([]byte{'|'})
	h.Write([]byte(e.UserAgent))
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalJSON(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		out.Write(kb)
		out.WriteByte(':')
		out.Write(vb)
	}
	out.WriteByte('}')
	return out.String()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
