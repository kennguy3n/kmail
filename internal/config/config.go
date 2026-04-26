// Package config hosts shared configuration loading for the Go
// control plane — environment variables, config files, and
// secrets-manager references consumed by every cmd/* binary.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): provide a single strongly-typed
// surface for Postgres / Valkey / Stalwart / zk-object-fabric /
// KChat endpoint wiring so individual services do not roll their
// own configuration schema.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the strongly-typed surface every cmd/* binary consumes.
//
// All fields are populated from environment variables; defaults are
// picked to match the local `docker-compose.yml` stack so `go run
// ./cmd/kmail-api` against a bare `docker compose up` works without
// additional configuration.
type Config struct {
	// HTTP controls the BFF HTTP listener.
	HTTP HTTPConfig

	// Database is the control-plane Postgres connection string. The
	// schema is defined in `docs/SCHEMA.md` and created by
	// `migrations/001_initial_schema.sql`.
	DatabaseURL string

	// StalwartURL is the internal Stalwart JMAP endpoint the BFF
	// proxies to. In compose this is `http://stalwart:8080`; in
	// production it is an internal service URL behind the mesh.
	StalwartURL string

	// ValkeyURL is the Redis-compatible Valkey connection string
	// used for session caches, rate-limit buckets, and the
	// `(tenant_id, kchat_user_id) → stalwart_account_id` lookup
	// cache documented in `docs/JMAP-CONTRACT.md` §3.3.
	ValkeyURL string

	// KChatOIDCIssuer is the KChat OIDC issuer URL. JWKS discovery
	// and token validation hang off this per `docs/JMAP-CONTRACT.md`
	// §3.1. Empty in pure local dev; combined with `DevBypassToken`
	// the auth middleware accepts a static token.
	KChatOIDCIssuer string

	// KChatOIDCAudience is the expected `aud` claim on OIDC tokens
	// minted for the KMail BFF. When non-empty the auth middleware
	// rejects tokens whose `aud` list does not include this value.
	// Empty disables audience checking (dev-bypass / trust-all
	// posture).
	KChatOIDCAudience string

	// DevBypassToken is a static bearer token that the auth
	// middleware accepts when running in dev mode. Never set this in
	// production; the value is a convenience for local development
	// only. Empty disables the bypass.
	DevBypassToken string

	// RateLimit controls the Valkey-backed rate limiter mounted in
	// front of the JMAP proxy and tenant handlers (per
	// docs/PROPOSAL.md §9.4 and docs/JMAP-CONTRACT.md §3.5).
	RateLimit RateLimitConfig

	// ZKFabric holds the zk-object-fabric S3 gateway wiring Stalwart
	// talks to for blob storage (see docs/ARCHITECTURE.md §4). The
	// BFF does NOT proxy blob reads/writes — Stalwart talks S3
	// directly over its own configuration — but the BFF needs the
	// endpoints and credentials for tenant bucket provisioning,
	// presigned-URL minting (attachments), and usage/quota lookups.
	ZKFabric ZKFabricConfig

	// DNS controls the DNS Onboarding Service. The BFF mounts a
	// sub-handler for /api/v1/tenants/{id}/domains/{domainId}/...
	// routes; the standalone `cmd/kmail-dns` binary exposes the
	// same surface on its own port for operators that want to
	// split the service out.
	DNS DNSConfig

	// KChatAPIURL is the base URL for the KChat channel-message
	// API; the Email-to-Chat bridge POSTs rich-card messages to
	// channels under this host.
	KChatAPIURL string

	// KChatAPIToken is the service-account bearer token the
	// bridge presents to the KChat API.
	KChatAPIToken string

	// KChatMLSEndpoint is the URL of the KChat MLS credential
	// service. Confidential Send uses it to derive per-recipient
	// wrapping keys when the recipient is a KChat user. Empty
	// values disable MLS and fall back to the link-based portal
	// flow.
	KChatMLSEndpoint string

	// ChatBridge controls the standalone `cmd/kmail-chat-bridge`
	// listener. The in-process BFF integration ignores this.
	ChatBridge ChatBridgeConfig

	// Audit controls the standalone `cmd/kmail-audit` CLI /
	// listener.
	Audit AuditConfig

	// Billing holds per-seat pricing and per-seat default storage
	// allocations for the Billing / Quota Service.
	Billing BillingConfig

	// Deliverability holds per-plan daily send caps, warmup ramp
	// parameters, and the suppression / bounce escalation knobs.
	Deliverability DeliverabilityConfig

	// Observability wires the Prometheus /metrics exposition and
	// the OpenTelemetry OTLP exporter endpoint.
	Observability ObservabilityConfig

	// Attachments controls attachment-to-link conversion — size
	// threshold, default link expiry, and the bucket name on
	// zk-object-fabric.
	Attachments AttachmentsConfig
}

// BillingConfig wires the Billing / Quota Service.
type BillingConfig struct {
	// Per-seat price in cents for each published plan. Defaults:
	// core=300 ($3), pro=600 ($6), privacy=900 ($9).
	CoreSeatCents    int
	ProSeatCents     int
	PrivacySeatCents int

	// Per-seat default storage allocation, in bytes. Defaults:
	// core=5 GB, pro=15 GB, privacy=50 GB.
	CorePerSeatBytes    int64
	ProPerSeatBytes     int64
	PrivacyPerSeatBytes int64

	// QuotaWorkerInterval controls how often the quota worker
	// polls zk-object-fabric. Defaults to 5m.
	QuotaWorkerInterval time.Duration

	// QuotaWorkerEnabled gates the worker. When false the worker
	// goroutine is not started (e.g. tests, CI).
	QuotaWorkerEnabled bool
}

// DeliverabilityConfig wires the Deliverability Control Plane.
type DeliverabilityConfig struct {
	// Plan-based daily send caps.
	CoreDailyLimit    int
	ProDailyLimit     int
	PrivacyDailyLimit int

	// WarmupDays is the length of the warmup ramp. Defaults to 30.
	WarmupDays int

	// BounceSoftEscalationCount is the number of soft bounces
	// within `BounceSoftWindow` that escalate an address to
	// suppression. Defaults to 3 / 72h.
	BounceSoftEscalationCount int
	BounceSoftWindow          time.Duration
}

// ObservabilityConfig wires Prometheus + OpenTelemetry.
type ObservabilityConfig struct {
	// MetricsEnabled gates the `/metrics` endpoint. Defaults to true.
	MetricsEnabled bool

	// TracingEnabled gates the OTEL SDK. Defaults to false so local
	// dev doesn't require an OTLP collector.
	TracingEnabled bool

	// OTLPEndpoint is the OTLP/HTTP exporter endpoint (e.g.
	// `http://tempo:4318`). Empty disables tracing.
	OTLPEndpoint string

	// LogFormat is either "text" (default) or "json" (structured).
	LogFormat string
}

// AttachmentsConfig wires attachment-to-link conversion.
type AttachmentsConfig struct {
	// ThresholdBytes is the size above which attachments are
	// converted to presigned links. Defaults to 10 MB.
	ThresholdBytes int64

	// DefaultExpiry is the lifetime of a presigned URL when the
	// caller does not specify one. Defaults to 7 days.
	DefaultExpiry time.Duration

	// BucketName is the zk-object-fabric S3 bucket attachments
	// are uploaded to.
	BucketName string
}

// ChatBridgeConfig wires the standalone chat-bridge listener.
type ChatBridgeConfig struct {
	// Addr is the listen address for the chat-bridge HTTP surface.
	Addr string
}

// AuditConfig wires the audit-log service.
type AuditConfig struct {
	// Addr is the listen address for the standalone kmail-audit
	// CLI / HTTP surface.
	Addr string
}

// ZKFabricConfig is the connection surface for the zk-object-fabric
// S3 gateway. In local compose the gateway is reachable at the
// `zk-fabric` service name; host ports `9080` (S3) and `9081`
// (console) avoid colliding with the BFF on `:8080`.
type ZKFabricConfig struct {
	// S3URL is the zk-object-fabric S3 endpoint for direct blob
	// operations (presigned URLs, attachment links).
	S3URL string

	// ConsoleURL is the zk-object-fabric console API for tenant
	// bucket provisioning and usage/quota reads.
	ConsoleURL string

	// AccessKey and SecretKey are the HMAC credentials for the
	// kmail service tenant in zk-object-fabric. They match the
	// `kmail-dev` binding in zk-object-fabric's demo/tenants.json
	// template.
	AccessKey string
	SecretKey string
}

// RateLimitConfig wires the Valkey-backed rate limiter middleware.
// Tenant and per-user limits are applied as a sliding window keyed
// on the authenticated identity.
type RateLimitConfig struct {
	// Enabled gates the middleware. When false the limiter is a
	// no-op and neither reads from Valkey nor allocates a client
	// connection.
	Enabled bool
	// TenantRPM is the per-tenant request-per-minute ceiling.
	// Defaults to 1000 rpm.
	TenantRPM int
	// UserRPM is the per-user request-per-minute ceiling (keyed
	// on `tenant_id + user_id`). Defaults to 200 rpm.
	UserRPM int
	// Window sizes the sliding window. Defaults to 60s so RPM
	// values match wall-clock minutes without arithmetic.
	Window time.Duration
}

// DNSConfig wires the DNS Onboarding Service. The defaults target
// KMail's dev infrastructure (`kmail.local`) so `go run
// ./cmd/kmail-api` and `go run ./cmd/kmail-dns` work out of the
// box without additional configuration.
type DNSConfig struct {
	// Addr is the listen address for the standalone kmail-dns
	// binary. The in-process BFF integration ignores this.
	Addr string
	// MailHost is the canonical KMail mail host; tenant MX records
	// must point at this or a subdomain.
	MailHost string
	// SPFInclude is the SPF include tenants must add to their SPF
	// record.
	SPFInclude string
	// DKIMSelector is the default DKIM selector KMail publishes for
	// tenant domains.
	DKIMSelector string
	// DKIMPublicKey is the RSA DKIM public key (base64) KMail
	// publishes. Surfaced through GenerateRecords so the tenant
	// can configure the matching DNS record.
	DKIMPublicKey string
	// DMARCPolicy is the baseline DMARC policy
	// (`none`/`quarantine`/`reject`) surfaced through
	// GenerateRecords.
	DMARCPolicy string
	// ReportingMailbox receives DMARC aggregate and TLS-RPT
	// reports.
	ReportingMailbox string
	// BIMILogoURL is the publicly hosted SVG logo URL surfaced
	// in the `default._bimi.<domain>` TXT record.
	BIMILogoURL string
	// BIMIVMCURL is the publicly hosted Verified Mark Certificate
	// URL paired with the BIMI logo. Optional.
	BIMIVMCURL string
}

// HTTPConfig holds the BFF HTTP listener configuration.
type HTTPConfig struct {
	// Addr is the listen address (`host:port` or `:port`).
	Addr string

	// ReadHeaderTimeout bounds how long the server will wait to read
	// request headers. Prevents slowloris-style denial of service.
	ReadHeaderTimeout time.Duration

	// ShutdownTimeout bounds how long the graceful shutdown waits
	// for in-flight requests to drain before forcing the server to
	// stop.
	ShutdownTimeout time.Duration
}

// Load reads configuration from the process environment. It never
// returns an error today — all fields fall back to local-dev
// defaults — but returning an error keeps the call site stable for
// when required secrets are added (e.g., production signing keys).
func Load() (*Config, error) {
	return &Config{
		HTTP: HTTPConfig{
			Addr:              getenv("KMAIL_API_ADDR", ":8080"),
			ReadHeaderTimeout: getenvDuration("KMAIL_API_READ_HEADER_TIMEOUT", 10*time.Second),
			ShutdownTimeout:   getenvDuration("KMAIL_API_SHUTDOWN_TIMEOUT", 30*time.Second),
		},
		DatabaseURL:     getenv("DATABASE_URL", "postgresql://kmail:kmail@localhost:5432/kmail?sslmode=disable"),
		// Stalwart's container port 8080 is published to host 18080
		// in `docker-compose.yml` precisely so a host-run BFF
		// (`go run ./cmd/kmail-api`) can reach it without colliding
		// with the BFF's own :8080 listener. Inside compose, override
		// this with `STALWART_URL=http://stalwart:8080`.
		StalwartURL:     getenv("STALWART_URL", "http://localhost:18080"),
		ValkeyURL:       getenv("VALKEY_URL", "valkey:6379"),
		KChatOIDCIssuer:   getenv("KCHAT_OIDC_ISSUER", ""),
		KChatOIDCAudience: getenv("KCHAT_OIDC_AUDIENCE", ""),
		DevBypassToken:    getenv("KMAIL_DEV_BYPASS_TOKEN", ""),
		RateLimit: RateLimitConfig{
			Enabled:   getenvBool("KMAIL_RATELIMIT_ENABLED", false),
			TenantRPM: GetenvInt("KMAIL_RATELIMIT_TENANT_RPM", 1000),
			UserRPM:   GetenvInt("KMAIL_RATELIMIT_USER_RPM", 200),
			Window:    getenvDuration("KMAIL_RATELIMIT_WINDOW", 60*time.Second),
		},
		ZKFabric: ZKFabricConfig{
			// Host-mapped defaults match docker-compose.yml, which
			// publishes zk-fabric on host `:9080` (S3) and `:9081`
			// (console) to avoid collision with the BFF on :8080.
			// Inside compose, override with
			// `ZK_FABRIC_S3_URL=http://zk-fabric:8080`.
			S3URL:      getenv("ZK_FABRIC_S3_URL", "http://localhost:9080"),
			ConsoleURL: getenv("ZK_FABRIC_CONSOLE_URL", "http://localhost:9081"),
			AccessKey:  getenv("ZK_FABRIC_ACCESS_KEY", "kmail-access-key"),
			SecretKey:  getenv("ZK_FABRIC_SECRET_KEY", "kmail-secret-key"),
		},
		DNS: DNSConfig{
			Addr:             getenv("KMAIL_DNS_ADDR", ":8090"),
			MailHost:         getenv("KMAIL_DNS_MAIL_HOST", "mx.kmail.local"),
			SPFInclude:       getenv("KMAIL_DNS_SPF_INCLUDE", "_spf.kmail.local"),
			DKIMSelector:     getenv("KMAIL_DNS_DKIM_SELECTOR", "kmail"),
			DKIMPublicKey:    getenv("KMAIL_DNS_DKIM_PUBLIC_KEY", ""),
			DMARCPolicy:      getenv("KMAIL_DNS_DMARC_POLICY", "none"),
			ReportingMailbox: getenv("KMAIL_DNS_REPORTING_MAILBOX", "dmarc@kmail.local"),
			BIMILogoURL:      getenv("KMAIL_DNS_BIMI_LOGO_URL", ""),
			BIMIVMCURL:       getenv("KMAIL_DNS_BIMI_VMC_URL", ""),
		},
		KChatAPIURL:      getenv("KCHAT_API_URL", ""),
		KChatAPIToken:    getenv("KCHAT_API_TOKEN", ""),
		KChatMLSEndpoint: getenv("KCHAT_MLS_ENDPOINT", ""),
		ChatBridge: ChatBridgeConfig{
			Addr: getenv("KMAIL_CHAT_BRIDGE_ADDR", ":8091"),
		},
		Audit: AuditConfig{
			Addr: getenv("KMAIL_AUDIT_ADDR", ":8092"),
		},
		Billing: BillingConfig{
			CoreSeatCents:       GetenvInt("KMAIL_BILLING_CORE_CENTS", 300),
			ProSeatCents:        GetenvInt("KMAIL_BILLING_PRO_CENTS", 600),
			PrivacySeatCents:    GetenvInt("KMAIL_BILLING_PRIVACY_CENTS", 900),
			CorePerSeatBytes:    getenvInt64("KMAIL_BILLING_CORE_PERSEAT_BYTES", 5*1024*1024*1024),
			ProPerSeatBytes:     getenvInt64("KMAIL_BILLING_PRO_PERSEAT_BYTES", 15*1024*1024*1024),
			PrivacyPerSeatBytes: getenvInt64("KMAIL_BILLING_PRIVACY_PERSEAT_BYTES", 50*1024*1024*1024),
			QuotaWorkerInterval: getenvDuration("KMAIL_QUOTA_WORKER_INTERVAL", 5*time.Minute),
			QuotaWorkerEnabled:  getenvBool("KMAIL_QUOTA_WORKER_ENABLED", false),
		},
		Deliverability: DeliverabilityConfig{
			CoreDailyLimit:            GetenvInt("KMAIL_SEND_CORE_DAILY", 500),
			ProDailyLimit:             GetenvInt("KMAIL_SEND_PRO_DAILY", 2000),
			PrivacyDailyLimit:         GetenvInt("KMAIL_SEND_PRIVACY_DAILY", 5000),
			WarmupDays:                GetenvInt("KMAIL_WARMUP_DAYS", 30),
			BounceSoftEscalationCount: GetenvInt("KMAIL_BOUNCE_SOFT_COUNT", 3),
			BounceSoftWindow:          getenvDuration("KMAIL_BOUNCE_SOFT_WINDOW", 72*time.Hour),
		},
		Observability: ObservabilityConfig{
			MetricsEnabled: getenvBool("KMAIL_METRICS_ENABLED", true),
			TracingEnabled: getenvBool("KMAIL_TRACING_ENABLED", false),
			OTLPEndpoint:   getenv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
			LogFormat:      getenv("KMAIL_LOG_FORMAT", "text"),
		},
		Attachments: AttachmentsConfig{
			ThresholdBytes: getenvInt64("KMAIL_ATTACHMENT_THRESHOLD_BYTES", 10*1024*1024),
			DefaultExpiry:  getenvDuration("KMAIL_ATTACHMENT_EXPIRY", 7*24*time.Hour),
			BucketName:     getenv("KMAIL_ATTACHMENT_BUCKET", "kmail-attachments"),
		},
	}, nil
}

// getenvInt64 parses the named environment variable as an int64.
func getenvInt64(key string, fallback int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// getenv returns the value of the named environment variable or the
// provided default if it is unset.
func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getenvDuration parses the named environment variable as a
// `time.Duration`. If the variable is unset or cannot be parsed, the
// provided default is used.
func getenvDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// getenvBool parses the named environment variable as a boolean.
// Accepted truthy values: 1, t, true, y, yes (case-insensitive);
// everything else (including unset) falls back to the provided
// default.
func getenvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	}
	return fallback
}

// GetenvInt parses the named environment variable as an integer. If
// the variable is unset or cannot be parsed, the provided default is
// used. Exported for use by sibling packages that read their own
// configuration knobs from the environment.
func GetenvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// String renders a human-readable, secret-masked summary of the
// config for startup logging.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{HTTP.Addr=%s DatabaseURL=%s StalwartURL=%s ValkeyURL=%s KChatOIDCIssuer=%s DevBypass=%t ZKFabric.S3URL=%s ZKFabric.ConsoleURL=%s ZKFabric.Keys=%t}",
		c.HTTP.Addr,
		redactDSN(c.DatabaseURL),
		c.StalwartURL,
		c.ValkeyURL,
		c.KChatOIDCIssuer,
		c.DevBypassToken != "",
		c.ZKFabric.S3URL,
		c.ZKFabric.ConsoleURL,
		c.ZKFabric.AccessKey != "" && c.ZKFabric.SecretKey != "",
	)
}

// redactDSN replaces the password component of a `scheme://user:pass@host/...`
// string with `***` so config summaries are safe to log.
func redactDSN(dsn string) string {
	// Find the `:` that separates user from password and the `@`
	// that ends the credential section.
	schemeIdx := -1
	for i := 0; i+2 < len(dsn); i++ {
		if dsn[i] == ':' && dsn[i+1] == '/' && dsn[i+2] == '/' {
			schemeIdx = i + 3
			break
		}
	}
	if schemeIdx < 0 {
		return dsn
	}
	atIdx := -1
	for i := schemeIdx; i < len(dsn); i++ {
		if dsn[i] == '@' {
			atIdx = i
			break
		}
	}
	if atIdx < 0 {
		return dsn
	}
	colonIdx := -1
	for i := schemeIdx; i < atIdx; i++ {
		if dsn[i] == ':' {
			colonIdx = i
			break
		}
	}
	if colonIdx < 0 {
		return dsn
	}
	return dsn[:colonIdx+1] + "***" + dsn[atIdx:]
}
