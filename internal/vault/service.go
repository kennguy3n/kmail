// Package vault — Phase 5 Zero-Access Vault service.
//
// Vault folders are server-stored containers for messages
// encrypted client-side under a key the BFF never sees. The
// `VaultService` only persists metadata about the wrapping (a
// wrapped DEK blob, the algorithm name, and a nonce) so the
// server can hand the ciphertext back to the client and the
// client can unwrap it locally with its KChat MLS credential.
//
// See `docs/PROGRESS.md` Phase 5 §Zero-Access Vault and the
// privacy-mode → zk-object-fabric mode mapping
// (StrictZK) in `docs/PROPOSAL.md`. The service does not search,
// scan, or otherwise touch message contents — that is the load-
// bearing privacy guarantee of the vault tier.
package vault

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

// Folder is the public shape of a vault folder.
type Folder struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	UserID         string    `json:"user_id"`
	FolderName     string    `json:"folder_name"`
	EncryptionMode string    `json:"encryption_mode"`
	WrappedDEK     []byte    `json:"wrapped_dek,omitempty"`
	KeyAlgorithm   string    `json:"key_algorithm"`
	Nonce          []byte    `json:"nonce,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// VaultService manages vault folder metadata.
type VaultService struct {
	pool *pgxpool.Pool
}

// NewVaultService returns a service.
func NewVaultService(pool *pgxpool.Pool) *VaultService {
	return &VaultService{pool: pool}
}

// CreateVaultFolder inserts a new vault folder row. Only the
// metadata is stored; plaintext keys never enter the table.
func (s *VaultService) CreateVaultFolder(ctx context.Context, f Folder) (*Folder, error) {
	if err := validateFolder(f); err != nil {
		return nil, err
	}
	if s.pool == nil {
		return nil, errors.New("vault: pool not configured")
	}
	if f.EncryptionMode == "" {
		f.EncryptionMode = "StrictZK"
	}
	if f.KeyAlgorithm == "" {
		f.KeyAlgorithm = "XChaCha20-Poly1305"
	}
	out := f
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, f.TenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO vault_folders (
				tenant_id, user_id, folder_name, encryption_mode,
				wrapped_dek, key_algorithm, nonce
			) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
			RETURNING id::text, created_at, updated_at
		`, f.TenantID, f.UserID, f.FolderName, f.EncryptionMode,
			f.WrappedDEK, f.KeyAlgorithm, f.Nonce,
		).Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("create vault folder: %w", err)
	}
	return &out, nil
}

// ListVaultFolders returns the vault folders for a tenant. When
// userID is non-empty the result is scoped to that user.
func (s *VaultService) ListVaultFolders(ctx context.Context, tenantID, userID string) ([]Folder, error) {
	if tenantID == "" {
		return nil, errors.New("vault: tenantID required")
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []Folder
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
			rows pgx.Rows
			err  error
		)
		if userID == "" {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, user_id, folder_name,
				       encryption_mode, wrapped_dek, key_algorithm, nonce,
				       created_at, updated_at
				FROM vault_folders
				WHERE tenant_id = $1::uuid
				ORDER BY created_at DESC
			`, tenantID)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, user_id, folder_name,
				       encryption_mode, wrapped_dek, key_algorithm, nonce,
				       created_at, updated_at
				FROM vault_folders
				WHERE tenant_id = $1::uuid AND user_id = $2
				ORDER BY created_at DESC
			`, tenantID, userID)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f Folder
			if err := rows.Scan(&f.ID, &f.TenantID, &f.UserID, &f.FolderName,
				&f.EncryptionMode, &f.WrappedDEK, &f.KeyAlgorithm, &f.Nonce,
				&f.CreatedAt, &f.UpdatedAt); err != nil {
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

// GetVaultFolder returns a single folder.
func (s *VaultService) GetVaultFolder(ctx context.Context, tenantID, folderID string) (*Folder, error) {
	if tenantID == "" || folderID == "" {
		return nil, errors.New("vault: tenantID and folderID required")
	}
	if s.pool == nil {
		return nil, errors.New("vault: pool not configured")
	}
	var f Folder
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, user_id, folder_name,
			       encryption_mode, wrapped_dek, key_algorithm, nonce,
			       created_at, updated_at
			FROM vault_folders
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, folderID).Scan(
			&f.ID, &f.TenantID, &f.UserID, &f.FolderName,
			&f.EncryptionMode, &f.WrappedDEK, &f.KeyAlgorithm, &f.Nonce,
			&f.CreatedAt, &f.UpdatedAt,
		)
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// DeleteVaultFolder removes a vault folder. The actual ciphertext
// objects in zk-object-fabric are reaped by the storage GC; the
// BFF only owns the metadata row.
func (s *VaultService) DeleteVaultFolder(ctx context.Context, tenantID, folderID string) error {
	if tenantID == "" || folderID == "" {
		return errors.New("vault: tenantID and folderID required")
	}
	if s.pool == nil {
		return errors.New("vault: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM vault_folders
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, folderID)
		return err
	})
}

// SetFolderEncryptionMeta updates the wrapped-DEK metadata for a
// folder. Used during MLS key rotation when the client re-wraps
// the existing DEK under a fresh leaf key. Plaintext keys never
// enter this call.
func (s *VaultService) SetFolderEncryptionMeta(ctx context.Context, tenantID, folderID string, wrappedDEK []byte, algorithm string, nonce []byte) (*Folder, error) {
	if tenantID == "" || folderID == "" {
		return nil, errors.New("vault: tenantID and folderID required")
	}
	if algorithm == "" {
		algorithm = "XChaCha20-Poly1305"
	}
	if s.pool == nil {
		return nil, errors.New("vault: pool not configured")
	}
	var f Folder
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			UPDATE vault_folders
			SET wrapped_dek = $3, key_algorithm = $4, nonce = $5
			WHERE tenant_id = $1::uuid AND id = $2::uuid
			RETURNING id::text, tenant_id::text, user_id, folder_name,
			          encryption_mode, wrapped_dek, key_algorithm, nonce,
			          created_at, updated_at
		`, tenantID, folderID, wrappedDEK, algorithm, nonce).Scan(
			&f.ID, &f.TenantID, &f.UserID, &f.FolderName,
			&f.EncryptionMode, &f.WrappedDEK, &f.KeyAlgorithm, &f.Nonce,
			&f.CreatedAt, &f.UpdatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("set encryption meta: %w", err)
	}
	return &f, nil
}

func validateFolder(f Folder) error {
	if strings.TrimSpace(f.TenantID) == "" {
		return errors.New("vault: tenant_id required")
	}
	if strings.TrimSpace(f.UserID) == "" {
		return errors.New("vault: user_id required")
	}
	if strings.TrimSpace(f.FolderName) == "" {
		return errors.New("vault: folder_name required")
	}
	if f.EncryptionMode != "" && f.EncryptionMode != "StrictZK" {
		return fmt.Errorf("vault: invalid encryption_mode %q (only StrictZK)", f.EncryptionMode)
	}
	return nil
}
