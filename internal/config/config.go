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

	// DevBypassToken is a static bearer token that the auth
	// middleware accepts when running in dev mode. Never set this in
	// production; the value is a convenience for local development
	// only. Empty disables the bypass.
	DevBypassToken string

	// ZKFabric holds the zk-object-fabric S3 gateway wiring Stalwart
	// talks to for blob storage (see docs/ARCHITECTURE.md §4). The
	// BFF does NOT proxy blob reads/writes — Stalwart talks S3
	// directly over its own configuration — but the BFF needs the
	// endpoints and credentials for tenant bucket provisioning,
	// presigned-URL minting (attachments), and usage/quota lookups.
	ZKFabric ZKFabricConfig
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
		KChatOIDCIssuer: getenv("KCHAT_OIDC_ISSUER", ""),
		DevBypassToken:  getenv("KMAIL_DEV_BYPASS_TOKEN", ""),
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
	}, nil
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
