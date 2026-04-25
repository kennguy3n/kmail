// Package tenant — zk-object-fabric provisioning helpers.
//
// `ZKFabricProvisioner` is invoked from `Service.CreateTenant` and
// is responsible for the Phase 4 "Production zk-object-fabric
// integration" workflow:
//
//  1. Create a per-tenant S3 bucket (`kmail-{tenant_id}`) on the
//     zk-object-fabric S3 endpoint.
//  2. Mint a dedicated API key via the console
//     (`POST /api/tenants/{id}/keys`).
//  3. Set the tenant's placement policy via
//     (`PUT /api/tenants/{id}/placement`).
//
// The encryption mode for core/pro plans defaults to `managed`
// (zk-object-fabric `ManagedEncrypted`). Privacy-tier confidential
// flows still use `client_side` (`StrictZK`) at the per-folder /
// per-message level — those are gated by the application layer
// (Confidential Send, Zero-Access Vault) and override the bucket
// default at object-write time.
//
// Cross-tenant dedupe is explicitly forbidden — every tenant gets
// its own bucket namespace and placement policy. The provisioner
// surfaces partial failures back to the caller so a half-provisioned
// tenant doesn't slip into the control plane silently.
package tenant

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// signEmptyPayloadSigV4 stamps `req` with an AWS SigV4 signature
// over an empty body. Mirrors the helper in `internal/jmap/attachment.go`
// but kept separate to avoid a cross-package dependency.
func signEmptyPayloadSigV4(req *http.Request, accessKey, secretKey, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	req.Header.Set("X-Amz-Date", amzDate)
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	req.Header.Set("X-Amz-Content-Sha256", emptyHash)
	canonURI := req.URL.EscapedPath()
	if canonURI == "" {
		canonURI = "/"
	}
	canonHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + emptyHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonRequest := strings.Join([]string{
		req.Method,
		canonURI,
		"",
		canonHeaders,
		signedHeaders,
		emptyHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hexHash([]byte(canonRequest)),
	}, "\n")
	kDate := hmacBytes([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacBytes(kDate, []byte(region))
	kService := hmacBytes(kRegion, []byte("s3"))
	signingKey := hmacBytes(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacBytes(signingKey, []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature))
}

func hmacBytes(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hexHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Encryption mode constants mirror zk-object-fabric's
// `metadata/placement_policy/policy.go`. Centralized here so
// callers do not import the fabric module just for the strings.
const (
	EncryptionModeManaged           = "managed"
	EncryptionModeClientSide        = "client_side"
	EncryptionModePublicDistribution = "public_distribution"
)

// StorageCredential is one row in `tenant_storage_credentials`.
type StorageCredential struct {
	TenantID               string    `json:"tenant_id"`
	BucketName             string    `json:"bucket_name"`
	AccessKey              string    `json:"access_key"`
	EncryptedSecretKey     string    `json:"-"`
	PlacementPolicyRef     string    `json:"placement_policy_ref"`
	EncryptionModeDefault  string    `json:"encryption_mode_default"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// ZKFabricProvisioner orchestrates the per-tenant provisioning
// flow against the zk-object-fabric S3 + console APIs.
type ZKFabricProvisioner struct {
	// Pool stores the resulting credential row.
	Pool *pgxpool.Pool
	// S3URL is the zk-object-fabric S3 endpoint
	// (e.g. http://zk-fabric:9080).
	S3URL string
	// ConsoleURL is the zk-object-fabric console API
	// (e.g. http://zk-fabric:8081).
	ConsoleURL string
	// AdminAccessKey / AdminSecretKey are the BFF's admin credentials
	// used to create per-tenant buckets via S3 `CreateBucket`. Distinct
	// from the per-tenant keys returned by `POST /api/tenants/{id}/keys`.
	AdminAccessKey string
	AdminSecretKey string
	// Region is the AWS region used for SigV4 signing.
	Region string
	// HTTP overrides the HTTP client used for both S3 and console
	// calls. Defaults to `http.Client{Timeout: 10s}`.
	HTTP *http.Client
	// Logger overrides the package logger. Defaults to log.Default().
	Logger *log.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// NewZKFabricProvisioner returns a provisioner with sensible
// defaults applied.
func NewZKFabricProvisioner(p ZKFabricProvisioner) *ZKFabricProvisioner {
	if p.HTTP == nil {
		p.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if p.Logger == nil {
		p.Logger = log.Default()
	}
	if p.Region == "" {
		p.Region = "us-east-1"
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return &p
}

// BucketNameFor returns the canonical bucket name for the tenant
// (`kmail-{tenant_id}`). Exposed for tests and for the migration
// wizard that pre-provisions buckets out-of-band.
func BucketNameFor(tenantID string) string {
	return "kmail-" + strings.ToLower(strings.TrimSpace(tenantID))
}

// PlanEncryptionDefault returns the encryption mode the bucket
// should default to for the given plan tier. Privacy-tier flows
// override per-object at write time (Confidential Send / Zero-Access
// Vault), but the bucket default is still `managed` so server-side
// search and spam scanning work for the rest of the mail flow.
func PlanEncryptionDefault(plan string) string {
	// Every plan defaults to managed. Privacy-tier confidential
	// surfaces upgrade to client_side per object/folder via MLS keys.
	switch plan {
	case "core", "pro", "privacy":
		return EncryptionModeManaged
	default:
		return EncryptionModeManaged
	}
}

// Provision walks through the create-bucket → mint-key →
// set-placement sequence and persists the resulting credential
// row. Errors are wrapped with `provision step: ...` so caller
// log lines identify the failing step without parsing the body.
func (p *ZKFabricProvisioner) Provision(ctx context.Context, tenantID, plan string) (*StorageCredential, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenant id required")
	}
	bucket := BucketNameFor(tenantID)

	// 1) Create the per-tenant bucket on the S3 endpoint. Treat
	//    409/BucketAlreadyOwnedByYou as success so re-provisioning
	//    a tenant whose bucket already exists is idempotent.
	if p.S3URL != "" {
		if err := p.createBucket(ctx, bucket); err != nil {
			return nil, fmt.Errorf("provision step=create_bucket: %w", err)
		}
	}

	// 2) Mint a per-tenant API key on the console.
	var accessKey, secretKey string
	if p.ConsoleURL != "" {
		ak, sk, err := p.createKey(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("provision step=create_key: %w", err)
		}
		accessKey, secretKey = ak, sk
	}

	// 3) Set the placement policy. Phase 4 default: managed
	//    encryption, US country allow-list, wasabi provider. Tenant
	//    admins refine via the Phase 5 placement-policy admin UI
	//    (`/admin/storage-placement`).
	policyRef := ""
	encryptionMode := PlanEncryptionDefault(plan)
	if p.ConsoleURL != "" {
		ref, err := p.putPlacement(ctx, tenantID, encryptionMode)
		if err != nil {
			return nil, fmt.Errorf("provision step=put_placement: %w", err)
		}
		policyRef = ref
	}

	// 4) Persist the credentials so future requests can resolve the
	//    tenant's bucket without another round trip.
	cred := &StorageCredential{
		TenantID:              tenantID,
		BucketName:            bucket,
		AccessKey:             accessKey,
		EncryptedSecretKey:    secretKey, // see migrations/018 — KMS wrap is Phase 5.
		PlacementPolicyRef:    policyRef,
		EncryptionModeDefault: encryptionMode,
	}
	if p.Pool != nil {
		if err := p.persist(ctx, cred); err != nil {
			return nil, fmt.Errorf("provision step=persist: %w", err)
		}
	}
	return cred, nil
}

// createBucket issues an S3 `CreateBucket` request against the
// zk-object-fabric S3 endpoint. SigV4 signing is intentionally
// minimal — we sign with the empty-payload hash and a single-shot
// HMAC chain. The fabric's Phase 1 auth accepts the signature as
// long as the canonical request matches.
func (p *ZKFabricProvisioner) createBucket(ctx context.Context, bucket string) error {
	endpoint := strings.TrimRight(p.S3URL, "/") + "/" + bucket
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
	if err != nil {
		return err
	}
	if p.AdminAccessKey != "" && p.AdminSecretKey != "" {
		signEmptyPayloadSigV4(req, p.AdminAccessKey, p.AdminSecretKey, p.Region, p.Now())
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("create bucket %q: %w", bucket, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusConflict:
		// 409 = BucketAlreadyOwnedByYou; treat as success.
		return nil
	default:
		return fmt.Errorf("create bucket %q: HTTP %d: %s", bucket, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// createKey calls the console `POST /api/tenants/{id}/keys`. The
// console returns a one-time `secret_key` field — the caller must
// persist it immediately because the console will not surface it
// again.
func (p *ZKFabricProvisioner) createKey(ctx context.Context, tenantID string) (string, string, error) {
	url := strings.TrimRight(p.ConsoleURL, "/") + "/api/tenants/" + tenantID + "/keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return "", "", fmt.Errorf("create key: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("decode key response: %w", err)
	}
	if out.AccessKey == "" || out.SecretKey == "" {
		return "", "", errors.New("create key: empty access/secret in response")
	}
	return out.AccessKey, out.SecretKey, nil
}

// putPlacement calls the console `PUT /api/tenants/{id}/placement`
// with a managed-encryption Phase 1 default. The returned ref is
// the policy version handle the console echoed back (Phase 1 just
// echoes the tenant ID).
func (p *ZKFabricProvisioner) putPlacement(ctx context.Context, tenantID, mode string) (string, error) {
	url := strings.TrimRight(p.ConsoleURL, "/") + "/api/tenants/" + tenantID + "/placement"
	policy := map[string]any{
		"tenant": tenantID,
		"policy": map[string]any{
			"encryption": map[string]any{
				"mode": mode,
			},
			"placement": map[string]any{
				"provider": []string{"wasabi"},
				"country":  []string{"US"},
			},
		},
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("put placement: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return tenantID, nil
}

// persist upserts the credential row inside the tenant RLS scope.
func (p *ZKFabricProvisioner) persist(ctx context.Context, cred *StorageCredential) error {
	return pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, cred.TenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO tenant_storage_credentials (
				tenant_id, bucket_name, access_key, encrypted_secret_key,
				placement_policy_ref, encryption_mode_default
			) VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id) DO UPDATE
			SET bucket_name = EXCLUDED.bucket_name,
			    access_key = EXCLUDED.access_key,
			    encrypted_secret_key = EXCLUDED.encrypted_secret_key,
			    placement_policy_ref = EXCLUDED.placement_policy_ref,
			    encryption_mode_default = EXCLUDED.encryption_mode_default
			RETURNING created_at, updated_at
		`, cred.TenantID, cred.BucketName, cred.AccessKey, cred.EncryptedSecretKey,
			cred.PlacementPolicyRef, cred.EncryptionModeDefault).Scan(&cred.CreatedAt, &cred.UpdatedAt)
	})
}

// LookupCredential returns the persisted credential row for the
// tenant, or ErrNotFound when no row is present (the tenant was
// created before per-tenant provisioning landed).
func LookupStorageCredential(ctx context.Context, pool *pgxpool.Pool, tenantID string) (*StorageCredential, error) {
	if pool == nil {
		return nil, ErrNotFound
	}
	var cred StorageCredential
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT tenant_id::text, bucket_name, access_key, encrypted_secret_key,
			       placement_policy_ref, encryption_mode_default,
			       created_at, updated_at
			FROM tenant_storage_credentials
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(
			&cred.TenantID, &cred.BucketName, &cred.AccessKey, &cred.EncryptedSecretKey,
			&cred.PlacementPolicyRef, &cred.EncryptionModeDefault,
			&cred.CreatedAt, &cred.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cred, nil
}


