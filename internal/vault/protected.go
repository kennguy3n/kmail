package vault

// Protected folders are server-managed encrypted folders shared
// among teammates within the same tenant. Distinct from the
// Zero-Access Vault: server-side scanning still runs (the contents
// are ManagedEncrypted), but only explicitly granted users can
// open the folder. Every grant / revoke / read operation is
// recorded in `protected_folder_access_log` so the owner can
// audit who opened what.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ProtectedFolder is the public folder shape.
type ProtectedFolder struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	OwnerID    string    `json:"owner_id"`
	FolderName string    `json:"folder_name"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// FolderAccess is one grant of access on a protected folder.
type FolderAccess struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	FolderID   string    `json:"folder_id"`
	GranteeID  string    `json:"grantee_id"`
	Permission string    `json:"permission"`
	GrantedAt  time.Time `json:"granted_at"`
}

// AccessLogEntry is one row of the immutable audit trail for a
// protected folder.
type AccessLogEntry struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	FolderID  string    `json:"folder_id"`
	ActorID   string    `json:"actor_id"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"created_at"`
}

// ProtectedFolderService manages protected folders and their
// access grants.
type ProtectedFolderService struct {
	pool *pgxpool.Pool
}

// NewProtectedFolderService returns a service.
func NewProtectedFolderService(pool *pgxpool.Pool) *ProtectedFolderService {
	return &ProtectedFolderService{pool: pool}
}

// CreateProtectedFolder inserts a new folder owned by the caller.
func (s *ProtectedFolderService) CreateProtectedFolder(ctx context.Context, f ProtectedFolder) (*ProtectedFolder, error) {
	if strings.TrimSpace(f.TenantID) == "" {
		return nil, errors.New("protected_folder: tenant_id required")
	}
	if strings.TrimSpace(f.OwnerID) == "" {
		return nil, errors.New("protected_folder: owner_id required")
	}
	if strings.TrimSpace(f.FolderName) == "" {
		return nil, errors.New("protected_folder: folder_name required")
	}
	if s.pool == nil {
		return nil, errors.New("protected_folder: pool not configured")
	}
	out := f
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, f.TenantID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO protected_folders (tenant_id, owner_id, folder_name)
			VALUES ($1::uuid, $2, $3)
			RETURNING id::text, created_at, updated_at
		`, f.TenantID, f.OwnerID, f.FolderName).Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO protected_folder_access_log (tenant_id, folder_id, actor_id, action)
			VALUES ($1::uuid, $2::uuid, $3, 'create')
		`, f.TenantID, out.ID, f.OwnerID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create protected folder: %w", err)
	}
	return &out, nil
}

// ListProtectedFolders returns folders owned by `ownerID` (when
// non-empty) or every folder in the tenant.
func (s *ProtectedFolderService) ListProtectedFolders(ctx context.Context, tenantID, ownerID string) ([]ProtectedFolder, error) {
	if tenantID == "" {
		return nil, errors.New("protected_folder: tenantID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []ProtectedFolder
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
			rows pgx.Rows
			err  error
		)
		if ownerID == "" {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, owner_id, folder_name, created_at, updated_at
				FROM protected_folders WHERE tenant_id = $1::uuid
				ORDER BY created_at DESC
			`, tenantID)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, owner_id, folder_name, created_at, updated_at
				FROM protected_folders WHERE tenant_id = $1::uuid AND owner_id = $2
				ORDER BY created_at DESC
			`, tenantID, ownerID)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f ProtectedFolder
			if err := rows.Scan(&f.ID, &f.TenantID, &f.OwnerID, &f.FolderName, &f.CreatedAt, &f.UpdatedAt); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ShareFolder grants access to another user within the same tenant.
// `permission` must be `"read"` or `"read_write"`.
func (s *ProtectedFolderService) ShareFolder(ctx context.Context, tenantID, folderID, ownerID, granteeID, permission string) (*FolderAccess, error) {
	if tenantID == "" || folderID == "" || granteeID == "" {
		return nil, errors.New("protected_folder: tenantID, folderID, granteeID required")
	}
	if permission == "" {
		permission = "read"
	}
	if permission != "read" && permission != "read_write" {
		return nil, fmt.Errorf("protected_folder: invalid permission %q", permission)
	}
	if s.pool == nil {
		return nil, errors.New("protected_folder: pool not configured")
	}
	var a FolderAccess
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO protected_folder_access (tenant_id, folder_id, grantee_id, permission)
			VALUES ($1::uuid, $2::uuid, $3, $4)
			ON CONFLICT (folder_id, grantee_id) DO UPDATE
			SET permission = EXCLUDED.permission
			RETURNING id::text, tenant_id::text, folder_id::text, grantee_id, permission, granted_at
		`, tenantID, folderID, granteeID, permission).Scan(
			&a.ID, &a.TenantID, &a.FolderID, &a.GranteeID, &a.Permission, &a.GrantedAt,
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO protected_folder_access_log (tenant_id, folder_id, actor_id, action)
			VALUES ($1::uuid, $2::uuid, $3, $4)
		`, tenantID, folderID, ownerID, "grant:"+granteeID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UnshareFolder revokes a grant.
func (s *ProtectedFolderService) UnshareFolder(ctx context.Context, tenantID, folderID, ownerID, granteeID string) error {
	if tenantID == "" || folderID == "" || granteeID == "" {
		return errors.New("protected_folder: tenantID, folderID, granteeID required")
	}
	if s.pool == nil {
		return errors.New("protected_folder: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM protected_folder_access
			WHERE tenant_id = $1::uuid AND folder_id = $2::uuid AND grantee_id = $3
		`, tenantID, folderID, granteeID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO protected_folder_access_log (tenant_id, folder_id, actor_id, action)
			VALUES ($1::uuid, $2::uuid, $3, $4)
		`, tenantID, folderID, ownerID, "revoke:"+granteeID)
		return err
	})
}

// ListFolderAccess returns all grants for a folder.
func (s *ProtectedFolderService) ListFolderAccess(ctx context.Context, tenantID, folderID string) ([]FolderAccess, error) {
	if tenantID == "" || folderID == "" {
		return nil, errors.New("protected_folder: tenantID and folderID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []FolderAccess
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, folder_id::text, grantee_id, permission, granted_at
			FROM protected_folder_access
			WHERE tenant_id = $1::uuid AND folder_id = $2::uuid
			ORDER BY granted_at DESC
		`, tenantID, folderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a FolderAccess
			if err := rows.Scan(&a.ID, &a.TenantID, &a.FolderID, &a.GranteeID, &a.Permission, &a.GrantedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetFolderAccessLog returns the most recent log entries for a
// folder (capped at 200 rows).
func (s *ProtectedFolderService) GetFolderAccessLog(ctx context.Context, tenantID, folderID string) ([]AccessLogEntry, error) {
	if tenantID == "" || folderID == "" {
		return nil, errors.New("protected_folder: tenantID and folderID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []AccessLogEntry
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, folder_id::text, actor_id, action, created_at
			FROM protected_folder_access_log
			WHERE tenant_id = $1::uuid AND folder_id = $2::uuid
			ORDER BY created_at DESC
			LIMIT 200
		`, tenantID, folderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e AccessLogEntry
			if err := rows.Scan(&e.ID, &e.TenantID, &e.FolderID, &e.ActorID, &e.Action, &e.CreatedAt); err != nil {
				return err
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
