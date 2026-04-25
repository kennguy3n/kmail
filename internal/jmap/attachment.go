package jmap

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// AttachmentLink is the API representation of a row in
// `attachment_links`.
type AttachmentLink struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	ObjectKey   string    `json:"object_key"`
	Filename    string    `json:"filename"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	Expiry      time.Time `json:"expiry"`
	Revoked     bool      `json:"revoked"`
	CreatedAt   time.Time `json:"created_at"`
}

// Presigned is the small response struct returned to the frontend
// after an upload.
type Presigned struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Expiry    time.Time `json:"expiry"`
	Filename  string    `json:"filename"`
	SizeBytes int64     `json:"size_bytes"`
}

// AttachmentConfig wires the AttachmentService.
type AttachmentConfig struct {
	Pool      *pgxpool.Pool
	S3URL     string // e.g. http://zk-fabric:8080
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string // zk-object-fabric uses "us-east-1" by default
	Threshold int64
	Expiry    time.Duration
	Logger    *log.Logger
	HTTP      *http.Client
}

// AttachmentService implements attachment-to-link conversion. Blobs
// larger than the configured threshold are uploaded to
// zk-object-fabric via S3 PUT and replaced in the composed email
// with a presigned GET URL.
type AttachmentService struct {
	cfg AttachmentConfig
}

// NewAttachmentService builds an AttachmentService and applies
// sensible defaults.
func NewAttachmentService(cfg AttachmentConfig) *AttachmentService {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 10 * 1024 * 1024
	}
	if cfg.Expiry <= 0 {
		cfg.Expiry = 7 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 5 * time.Minute}
	}
	return &AttachmentService{cfg: cfg}
}

// UploadLargeAttachment streams an attachment to zk-object-fabric
// and records the metadata in `attachment_links`. The presigned URL
// returned in the response has the configured expiry (default 7d).
//
// The Phase 4 zk-object-fabric integration adds a per-tenant bucket
// lookup (`tenant_storage_credentials`) — when a tenant has a
// dedicated bucket provisioned the upload targets it instead of
// the global `cfg.Bucket`. Tenants created before per-tenant
// provisioning landed continue to upload to the global bucket so
// the attachment flow stays backward-compatible.
func (s *AttachmentService) UploadLargeAttachment(
	ctx context.Context,
	tenantID, filename, contentType string,
	body io.Reader,
	size int64,
) (*Presigned, error) {
	if tenantID == "" || filename == "" {
		return nil, fmt.Errorf("tenantID and filename required")
	}
	bucket := s.resolveTenantBucket(ctx, tenantID)
	objectKey := fmt.Sprintf("%s/%d-%s", tenantID, time.Now().UnixNano(), sanitizeFilename(filename))
	if s.cfg.S3URL != "" && bucket != "" {
		if err := s.s3PutToBucket(ctx, bucket, objectKey, contentType, body, size); err != nil {
			return nil, fmt.Errorf("s3 put: %w", err)
		}
	}
	expiry := time.Now().Add(s.cfg.Expiry)
	var row AttachmentLink
	if s.cfg.Pool != nil {
		err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
			if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `
				INSERT INTO attachment_links (
					tenant_id, object_key, filename, size_bytes,
					content_type, expiry
				) VALUES ($1::uuid, $2, $3, $4, $5, $6)
				RETURNING id::text, tenant_id::text, object_key, filename,
				          size_bytes, content_type, expiry, revoked, created_at
			`, tenantID, objectKey, filename, size, contentType, expiry,
			).Scan(
				&row.ID, &row.TenantID, &row.ObjectKey, &row.Filename,
				&row.SizeBytes, &row.ContentType, &row.Expiry, &row.Revoked,
				&row.CreatedAt,
			)
		})
		if err != nil {
			return nil, fmt.Errorf("insert attachment_link: %w", err)
		}
	}
	signed, err := s.presignForBucket(bucket, objectKey, s.cfg.Expiry)
	if err != nil {
		return nil, err
	}
	return &Presigned{
		ID:        row.ID,
		URL:       signed,
		Expiry:    expiry,
		Filename:  filename,
		SizeBytes: size,
	}, nil
}

// GetAttachmentLink returns a fresh presigned URL for an existing
// `attachment_links` row. Revoked rows return ErrAttachmentRevoked.
func (s *AttachmentService) GetAttachmentLink(ctx context.Context, tenantID, linkID string) (*Presigned, error) {
	if tenantID == "" || linkID == "" {
		return nil, fmt.Errorf("tenantID and linkID required")
	}
	if s.cfg.Pool == nil {
		return nil, fmt.Errorf("no pool configured")
	}
	var row AttachmentLink
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, object_key, filename,
			       size_bytes, content_type, expiry, revoked, created_at
			FROM attachment_links
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, linkID).Scan(
			&row.ID, &row.TenantID, &row.ObjectKey, &row.Filename,
			&row.SizeBytes, &row.ContentType, &row.Expiry, &row.Revoked,
			&row.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("attachment not found")
	}
	if err != nil {
		return nil, fmt.Errorf("select attachment_link: %w", err)
	}
	if row.Revoked {
		return nil, fmt.Errorf("attachment revoked")
	}
	ttl := time.Until(row.Expiry)
	if ttl <= 0 {
		return nil, fmt.Errorf("attachment expired")
	}
	bucket := s.resolveTenantBucket(ctx, tenantID)
	signed, err := s.presignForBucket(bucket, row.ObjectKey, ttl)
	if err != nil {
		return nil, err
	}
	return &Presigned{
		ID:        row.ID,
		URL:       signed,
		Expiry:    row.Expiry,
		Filename:  row.Filename,
		SizeBytes: row.SizeBytes,
	}, nil
}

// RevokeAttachment flips the revoked flag on an attachment link.
func (s *AttachmentService) RevokeAttachment(ctx context.Context, tenantID, linkID string) error {
	if tenantID == "" || linkID == "" {
		return fmt.Errorf("tenantID and linkID required")
	}
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE attachment_links SET revoked = true
			WHERE tenant_id = $1::uuid AND id = $2::uuid
		`, tenantID, linkID)
		return err
	})
}

// GeneratePresignedURL signs a GET URL for `objectKey` on the
// configured zk-object-fabric S3 endpoint. The returned URL is
// valid for `expiry` (capped to 7 days by AWS SigV4 semantics).
//
// The implementation is a minimal AWS SigV4 presigner — we sign
// against the zk-object-fabric S3 endpoint using the HMAC-SHA256
// derivation documented at
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
// to avoid adding the full aws-sdk-go-v2 dependency tree.
func (s *AttachmentService) GeneratePresignedURL(objectKey string, expiry time.Duration) (string, error) {
	return s.presignForBucket(s.cfg.Bucket, objectKey, expiry)
}

// presignForBucket is the bucket-parameterized presign helper. The
// public GeneratePresignedURL retains its signature for callers
// that haven't been migrated to per-tenant buckets.
func (s *AttachmentService) presignForBucket(bucket, objectKey string, expiry time.Duration) (string, error) {
	if bucket == "" {
		bucket = s.cfg.Bucket
	}
	if s.cfg.S3URL == "" || bucket == "" || s.cfg.AccessKey == "" || s.cfg.SecretKey == "" {
		return "", fmt.Errorf("s3 endpoint not configured")
	}
	if expiry <= 0 {
		expiry = s.cfg.Expiry
	}
	if expiry > 7*24*time.Hour {
		expiry = 7 * 24 * time.Hour
	}

	endpoint, err := url.Parse(s.cfg.S3URL)
	if err != nil {
		return "", fmt.Errorf("parse s3 url: %w", err)
	}
	host := endpoint.Host
	scheme := endpoint.Scheme
	if scheme == "" {
		scheme = "https"
	}
	canonURI := "/" + bucket + "/" + strings.TrimPrefix(objectKey, "/")

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.cfg.Region)

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", s.cfg.AccessKey+"/"+credentialScope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(expiry.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")

	canonQuery := canonicalQueryString(q)
	canonRequest := strings.Join([]string{
		"GET",
		canonURI,
		canonQuery,
		"host:" + host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	hashedCanon := sha256hex([]byte(canonRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hashedCanon,
	}, "\n")

	signingKey := deriveSigningKey(s.cfg.SecretKey, dateStamp, s.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	q.Set("X-Amz-Signature", signature)
	return fmt.Sprintf("%s://%s%s?%s", scheme, host, canonURI, q.Encode()), nil
}

// resolveTenantBucket reads the tenant's dedicated bucket from
// `tenant_storage_credentials`. Falls back to `cfg.Bucket` when no
// row is present (legacy tenants) or when the lookup fails — the
// global bucket continues to work for read paths even when the
// per-tenant provisioner has not yet run.
func (s *AttachmentService) resolveTenantBucket(ctx context.Context, tenantID string) string {
	if s.cfg.Pool == nil || tenantID == "" {
		return s.cfg.Bucket
	}
	var bucket string
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT bucket_name FROM tenant_storage_credentials
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(&bucket)
	})
	if err != nil || bucket == "" {
		return s.cfg.Bucket
	}
	return bucket
}

// s3Put uploads `body` to the configured S3 endpoint using SigV4
// query-style signing. Content-Length is inferred from `size`.
func (s *AttachmentService) s3Put(ctx context.Context, objectKey, contentType string, body io.Reader, size int64) error {
	return s.s3PutToBucket(ctx, s.cfg.Bucket, objectKey, contentType, body, size)
}

// s3PutToBucket is the bucket-parameterized PUT helper used by the
// per-tenant upload path. Falls back to `cfg.Bucket` when `bucket`
// is empty so the global single-bucket deployment still works.
func (s *AttachmentService) s3PutToBucket(ctx context.Context, bucket, objectKey, contentType string, body io.Reader, size int64) error {
	if bucket == "" {
		bucket = s.cfg.Bucket
	}
	putURL := strings.TrimRight(s.cfg.S3URL, "/") + "/" + bucket + "/" + strings.TrimPrefix(objectKey, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = size

	// SigV4 PUT with unsigned payload: only `host`, `x-amz-date`,
	// and `x-amz-content-sha256=UNSIGNED-PAYLOAD` are signed.
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.cfg.Region)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

	canonURI := "/" + s.cfg.Bucket + "/" + strings.TrimPrefix(objectKey, "/")
	canonHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:UNSIGNED-PAYLOAD\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonRequest := strings.Join([]string{
		"PUT",
		canonURI,
		"", // empty query
		canonHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256hex([]byte(canonRequest)),
	}, "\n")
	signingKey := deriveSigningKey(s.cfg.SecretKey, dateStamp, s.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, credentialScope, signedHeaders, signature))

	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 put status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// -- SigV4 helpers -------------------------------------------------

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape is url.QueryEscape with `+` → `%20` to match AWS SigV4.
func awsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func sanitizeFilename(name string) string {
	// Strip path separators and keep simple alphanumerics plus
	// dots, hyphens, and underscores so the object key is always
	// a safe S3 key. Max 128 chars.
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	if out == "" {
		out = "attachment"
	}
	return out
}
