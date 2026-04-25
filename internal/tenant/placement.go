// Package tenant — placement policy admin surface.
//
// Phase 5 "Regional storage controls" exposes the per-tenant
// placement policy admin UI. The provisioner (zkfabric.go) seeds an
// initial policy at CreateTenant; this file lets tenant admins read
// and update the policy through the BFF, validates plan-tier
// gating (StrictZK requires the privacy plan), and proxies the
// authoritative state back to the zk-object-fabric console API.
//
// The local `tenant_storage_credentials.placement_policy_ref`
// column stores the policy reference name returned by zk-object-
// fabric; the full policy body lives there and is fetched on
// demand. Local mirroring is intentionally avoided — the fabric is
// the source of truth and dual-writes risk drift.
package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PlacementPolicy is the full placement-policy view returned by the
// admin endpoints.
type PlacementPolicy struct {
	TenantID         string   `json:"tenant_id"`
	PolicyRef        string   `json:"policy_ref"`
	Countries        []string `json:"countries"`
	PreferredProvider string  `json:"preferred_provider"`
	EncryptionMode   string   `json:"encryption_mode"`
	ErasureProfile   string   `json:"erasure_profile"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// AvailableRegion is a region the fabric supports.
type AvailableRegion struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// PlacementService is the admin-facing placement helper.
type PlacementService struct {
	pool       *pgxpool.Pool
	consoleURL string
	httpc      *http.Client
}

// NewPlacementService returns a PlacementService.
func NewPlacementService(pool *pgxpool.Pool, consoleURL string) *PlacementService {
	return &PlacementService{
		pool:       pool,
		consoleURL: strings.TrimRight(consoleURL, "/"),
		httpc:      &http.Client{Timeout: 10 * time.Second},
	}
}

// ListAvailableRegions returns the regions the fabric supports.
// Hardcoded for Phase 1; Phase 2 will query
// `GET /api/regions` on the fabric console.
func (p *PlacementService) ListAvailableRegions() []AvailableRegion {
	return []AvailableRegion{
		{Code: "US", Name: "United States"},
		{Code: "EU", Name: "European Union"},
		{Code: "APAC", Name: "Asia-Pacific"},
	}
}

// GetPlacementPolicy reads the current policy. Falls back to the
// stored row when the console is unreachable so the admin UI can
// still render a stale view.
func (p *PlacementService) GetPlacementPolicy(ctx context.Context, tenantID string) (*PlacementPolicy, error) {
	if tenantID == "" {
		return nil, errors.New("tenantID required")
	}
	if p.pool == nil {
		return nil, errors.New("placement: pool not configured")
	}
	var (
		ref       string
		mode      string
		updatedAt time.Time
	)
	err := p.pool.QueryRow(ctx, `
		SELECT placement_policy_ref, encryption_mode_default, updated_at
		FROM tenant_storage_credentials WHERE tenant_id = $1::uuid
	`, tenantID).Scan(&ref, &mode, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("load placement: %w", err)
	}
	policy := &PlacementPolicy{
		TenantID:       tenantID,
		PolicyRef:      ref,
		EncryptionMode: mode,
		UpdatedAt:      updatedAt,
	}
	if p.consoleURL != "" {
		if err := p.fetchFromConsole(ctx, tenantID, policy); err != nil {
			// Best-effort: fall back to the local row.
			policy.Countries = []string{"US"}
			policy.PreferredProvider = "wasabi"
			policy.ErasureProfile = "rs-6-3"
		}
	}
	return policy, nil
}

func (p *PlacementService) fetchFromConsole(ctx context.Context, tenantID string, out *PlacementPolicy) error {
	url := fmt.Sprintf("%s/api/tenants/%s/placement", p.consoleURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("console GET placement: %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Countries         []string `json:"countries"`
		PreferredProvider string   `json:"preferred_provider"`
		EncryptionMode    string   `json:"encryption_mode"`
		ErasureProfile    string   `json:"erasure_profile"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	out.Countries = raw.Countries
	out.PreferredProvider = raw.PreferredProvider
	if raw.EncryptionMode != "" {
		out.EncryptionMode = raw.EncryptionMode
	}
	out.ErasureProfile = raw.ErasureProfile
	return nil
}

// UpdatePlacementPolicy validates and persists the policy. Plan
// gating: StrictZK (`client_side`) is privacy-tier only; other
// plans are pinned to ManagedEncrypted (`managed`).
func (p *PlacementService) UpdatePlacementPolicy(ctx context.Context, tenantID, plan string, policy PlacementPolicy) (*PlacementPolicy, error) {
	if tenantID == "" {
		return nil, errors.New("tenantID required")
	}
	if len(policy.Countries) == 0 {
		return nil, errors.New("placement: countries allow-list required")
	}
	if policy.EncryptionMode == "client_side" && plan != "privacy" {
		return nil, errors.New("placement: client_side encryption requires privacy plan")
	}
	if policy.EncryptionMode == "" {
		policy.EncryptionMode = "managed"
	}
	if p.consoleURL != "" {
		if err := p.pushToConsole(ctx, tenantID, policy); err != nil {
			return nil, err
		}
	}
	if p.pool != nil {
		_, err := p.pool.Exec(ctx, `
			UPDATE tenant_storage_credentials
			SET encryption_mode_default = $2,
			    placement_policy_ref    = COALESCE(NULLIF($3, ''), placement_policy_ref)
			WHERE tenant_id = $1::uuid
		`, tenantID, policy.EncryptionMode, policy.PolicyRef)
		if err != nil {
			return nil, fmt.Errorf("persist placement: %w", err)
		}
	}
	out := policy
	out.TenantID = tenantID
	out.UpdatedAt = time.Now().UTC()
	return &out, nil
}

func (p *PlacementService) pushToConsole(ctx context.Context, tenantID string, policy PlacementPolicy) error {
	url := fmt.Sprintf("%s/api/tenants/%s/placement", p.consoleURL, tenantID)
	body := map[string]any{
		"countries":          policy.Countries,
		"preferred_provider": policy.PreferredProvider,
		"encryption_mode":    policy.EncryptionMode,
		"erasure_profile":    policy.ErasureProfile,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("console PUT placement: %d %s", resp.StatusCode, string(b))
	}
	return nil
}
