package contactbridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// GALEntry is one row of the merged tenant address list.
type GALEntry struct {
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	Org           string    `json:"org,omitempty"`
	Phone         string    `json:"phone,omitempty"`
	SourceUID     string    `json:"source_uid,omitempty"`
	SourceAccount string    `json:"source_account,omitempty"`
	LastSyncedAt  time.Time `json:"last_synced_at"`
}

// GALService aggregates per-account address books into a tenant-
// wide directory. Reads are served from the `global_address_list`
// cache table. Writes happen only through the underlying CardDAV
// bridge — `Sync` walks each account's address books and upserts
// into the cache.
type GALService struct {
	pool   *pgxpool.Pool
	bridge *Service
}

// NewGALService returns a GALService.
func NewGALService(pool *pgxpool.Pool, bridge *Service) *GALService {
	return &GALService{pool: pool, bridge: bridge}
}

// List returns every cached entry for the tenant, ordered by
// display_name.
func (g *GALService) List(ctx context.Context, tenantID string) ([]GALEntry, error) {
	if g == nil || g.pool == nil || tenantID == "" {
		return nil, nil
	}
	var out []GALEntry
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT email, display_name, org, phone, source_uid, source_account, last_synced_at
			FROM global_address_list
			WHERE tenant_id = $1::uuid
			ORDER BY display_name, email
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e GALEntry
			if err := rows.Scan(&e.Email, &e.DisplayName, &e.Org, &e.Phone, &e.SourceUID, &e.SourceAccount, &e.LastSyncedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Search returns entries matching the prefix `q` against email or
// display_name (case-insensitive). Capped at `limit` rows for
// typeahead use.
func (g *GALService) Search(ctx context.Context, tenantID, q string, limit int) ([]GALEntry, error) {
	if g == nil || g.pool == nil || tenantID == "" {
		return nil, nil
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return g.List(ctx, tenantID)
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	pattern := "%" + strings.ToLower(q) + "%"
	var out []GALEntry
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT email, display_name, org, phone, source_uid, source_account, last_synced_at
			FROM global_address_list
			WHERE tenant_id = $1::uuid
			  AND (lower(email) LIKE $2 OR lower(display_name) LIKE $2)
			ORDER BY display_name, email
			LIMIT $3
		`, tenantID, pattern, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e GALEntry
			if err := rows.Scan(&e.Email, &e.DisplayName, &e.Org, &e.Phone, &e.SourceUID, &e.SourceAccount, &e.LastSyncedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Upsert inserts or updates a single GAL entry. Used by the sync
// path and by tests; admin UI does not write directly.
func (g *GALService) Upsert(ctx context.Context, tenantID string, e GALEntry) error {
	if g == nil || g.pool == nil {
		return errors.New("gal: pool not configured")
	}
	email := strings.ToLower(strings.TrimSpace(e.Email))
	if email == "" {
		return errors.New("gal: email required")
	}
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO global_address_list
			    (tenant_id, email, display_name, org, phone, source_uid, source_account, last_synced_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, now())
			ON CONFLICT (tenant_id, email) DO UPDATE
			SET display_name = EXCLUDED.display_name,
			    org           = EXCLUDED.org,
			    phone         = EXCLUDED.phone,
			    source_uid    = EXCLUDED.source_uid,
			    source_account = EXCLUDED.source_account,
			    last_synced_at = now()
		`, tenantID, email, e.DisplayName, e.Org, e.Phone, e.SourceUID, e.SourceAccount)
		return err
	})
}

// Sync walks every address book on every account in `accounts`
// and folds the merged contact set into the GAL cache for
// `tenantID`. Deduplication is by normalized email; the most-
// recently-seen contact wins. Best-effort: any per-account
// failure is logged by the caller and the rest still upsert.
func (g *GALService) Sync(ctx context.Context, tenantID string, accounts []string) (int, error) {
	if g == nil || g.bridge == nil {
		return 0, errors.New("gal: bridge not configured")
	}
	written := 0
	for _, acct := range accounts {
		books, err := g.bridge.ListAddressBooks(ctx, acct)
		if err != nil {
			continue
		}
		for _, ab := range books {
			contacts, err := g.bridge.GetContacts(ctx, acct, ab.ID)
			if err != nil {
				continue
			}
			for _, c := range contacts {
				for _, em := range c.Emails {
					if strings.TrimSpace(em) == "" {
						continue
					}
					phone := ""
					if len(c.Phones) > 0 {
						phone = c.Phones[0]
					}
					if err := g.Upsert(ctx, tenantID, GALEntry{
						Email:         em,
						DisplayName:   c.FN,
						Org:           c.Org,
						Phone:         phone,
						SourceUID:     c.UID,
						SourceAccount: acct,
					}); err == nil {
						written++
					}
				}
			}
		}
	}
	return written, nil
}
