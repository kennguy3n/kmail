package config

import (
	"testing"
	"time"
)

func TestLoadReturnsDefaults(t *testing.T) {
	// Clear BOTH the bare and `KMAIL_`-prefixed names so the
	// defaults kick in regardless of the developer's shell. The
	// getenvKMail helper checks the prefixed form first, so the
	// only way to truly exercise the defaults is to unset both.
	for _, k := range []string{
		"KMAIL_API_ADDR",
		"KMAIL_API_READ_HEADER_TIMEOUT",
		"KMAIL_API_SHUTDOWN_TIMEOUT",
		"DATABASE_URL", "KMAIL_DATABASE_URL",
		"STALWART_URL", "KMAIL_STALWART_URL",
		"VALKEY_URL", "KMAIL_VALKEY_URL",
		"KCHAT_OIDC_ISSUER", "KMAIL_KCHAT_OIDC_ISSUER",
		"KCHAT_OIDC_AUDIENCE", "KMAIL_KCHAT_OIDC_AUDIENCE",
		"KMAIL_DEV_BYPASS_TOKEN",
		"KMAIL_ENV",
		"ZK_FABRIC_S3_URL", "KMAIL_ZK_FABRIC_S3_URL",
		"ZK_FABRIC_CONSOLE_URL", "KMAIL_ZK_FABRIC_CONSOLE_URL",
		"ZK_FABRIC_ACCESS_KEY", "KMAIL_ZK_FABRIC_ACCESS_KEY",
		"ZK_FABRIC_SECRET_KEY", "KMAIL_ZK_FABRIC_SECRET_KEY",
		"KCHAT_API_URL", "KMAIL_KCHAT_API_URL",
		"KCHAT_API_TOKEN", "KMAIL_KCHAT_API_TOKEN",
		"KCHAT_MLS_ENDPOINT", "KMAIL_KCHAT_MLS_ENDPOINT",
	} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("HTTP.ReadHeaderTimeout = %v, want 10s", cfg.HTTP.ReadHeaderTimeout)
	}
	if cfg.HTTP.ShutdownTimeout != 30*time.Second {
		t.Errorf("HTTP.ShutdownTimeout = %v, want 30s", cfg.HTTP.ShutdownTimeout)
	}
	if cfg.DatabaseURL == "" {
		t.Error("DatabaseURL should have a default")
	}
	if cfg.StalwartURL == "" {
		t.Error("StalwartURL should have a default")
	}
	if cfg.ValkeyURL == "" {
		t.Error("ValkeyURL should have a default")
	}
	if cfg.KChatOIDCIssuer != "" {
		t.Errorf("KChatOIDCIssuer default should be empty, got %q", cfg.KChatOIDCIssuer)
	}
	if cfg.DevBypassToken != "" {
		t.Errorf("DevBypassToken default should be empty, got %q", cfg.DevBypassToken)
	}
	if cfg.Env != "production" {
		t.Errorf("Env default should be 'production' (fail-closed), got %q", cfg.Env)
	}
	if cfg.ZKFabric.S3URL != "http://localhost:9080" {
		t.Errorf("ZKFabric.S3URL = %q, want http://localhost:9080", cfg.ZKFabric.S3URL)
	}
	if cfg.ZKFabric.ConsoleURL != "http://localhost:9081" {
		t.Errorf("ZKFabric.ConsoleURL = %q, want http://localhost:9081", cfg.ZKFabric.ConsoleURL)
	}
	if cfg.ZKFabric.AccessKey == "" {
		t.Error("ZKFabric.AccessKey should have a default")
	}
	if cfg.ZKFabric.SecretKey == "" {
		t.Error("ZKFabric.SecretKey should have a default")
	}
}

func TestLoadHonoursEnv(t *testing.T) {
	t.Setenv("KMAIL_API_ADDR", ":9999")
	t.Setenv("DATABASE_URL", "postgresql://u:p@db/kmail")
	t.Setenv("STALWART_URL", "http://stalwart:8080")
	t.Setenv("KMAIL_DEV_BYPASS_TOKEN", "dev-token")
	t.Setenv("ZK_FABRIC_S3_URL", "http://zk-fabric:8080")
	t.Setenv("ZK_FABRIC_CONSOLE_URL", "http://zk-fabric:8081")
	t.Setenv("ZK_FABRIC_ACCESS_KEY", "override-access")
	t.Setenv("ZK_FABRIC_SECRET_KEY", "override-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":9999" {
		t.Errorf("HTTP.Addr = %q, want :9999", cfg.HTTP.Addr)
	}
	if cfg.DatabaseURL != "postgresql://u:p@db/kmail" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.StalwartURL != "http://stalwart:8080" {
		t.Errorf("StalwartURL = %q", cfg.StalwartURL)
	}
	if cfg.DevBypassToken != "dev-token" {
		t.Errorf("DevBypassToken = %q", cfg.DevBypassToken)
	}
	if cfg.ZKFabric.S3URL != "http://zk-fabric:8080" {
		t.Errorf("ZKFabric.S3URL = %q", cfg.ZKFabric.S3URL)
	}
	if cfg.ZKFabric.ConsoleURL != "http://zk-fabric:8081" {
		t.Errorf("ZKFabric.ConsoleURL = %q", cfg.ZKFabric.ConsoleURL)
	}
	if cfg.ZKFabric.AccessKey != "override-access" {
		t.Errorf("ZKFabric.AccessKey = %q", cfg.ZKFabric.AccessKey)
	}
	if cfg.ZKFabric.SecretKey != "override-secret" {
		t.Errorf("ZKFabric.SecretKey = %q", cfg.ZKFabric.SecretKey)
	}
}

// TestGetenvKMail pins the two-step lookup that makes the Helm
// chart functional: `KMAIL_X` wins over `X`, and `X` wins over the
// default. Without this layer the mTLS URL override emitted by
// `deploy/helm/kmail/templates/deployment-api.yaml` would be a
// no-op and the BFF would silently talk plain HTTP to Stalwart.
func TestGetenvKMail(t *testing.T) {
	t.Setenv("KMAIL_TEST_KMAIL", "")
	t.Setenv("TEST_KMAIL", "")
	if got := getenvKMail("TEST_KMAIL", "fallback"); got != "fallback" {
		t.Errorf("both unset: got %q, want fallback", got)
	}

	t.Setenv("TEST_KMAIL", "bare")
	if got := getenvKMail("TEST_KMAIL", "fallback"); got != "bare" {
		t.Errorf("bare set, prefixed unset: got %q, want bare", got)
	}

	t.Setenv("KMAIL_TEST_KMAIL", "prefixed")
	if got := getenvKMail("TEST_KMAIL", "fallback"); got != "prefixed" {
		t.Errorf("both set: got %q, want prefixed (KMAIL_-prefixed wins)", got)
	}

	t.Setenv("TEST_KMAIL", "")
	t.Setenv("KMAIL_TEST_KMAIL", "prefixed-only")
	if got := getenvKMail("TEST_KMAIL", "fallback"); got != "prefixed-only" {
		t.Errorf("prefixed set, bare unset: got %q", got)
	}
}

// TestLoad_PrefersKMailPrefixedStalwartURL is the regression test
// for the Devin Review finding that surfaced this PR's worst bug:
// the Helm chart set `KMAIL_STALWART_URL` but the binary read
// `STALWART_URL`, so the mTLS HTTPS override never reached the
// proxy. Asserting the precedence here pins the fix.
func TestLoad_PrefersKMailPrefixedStalwartURL(t *testing.T) {
	t.Setenv("STALWART_URL", "http://legacy:8080")
	t.Setenv("KMAIL_STALWART_URL", "https://prod:8443")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StalwartURL != "https://prod:8443" {
		t.Errorf("StalwartURL = %q, want the KMAIL_-prefixed override", cfg.StalwartURL)
	}
}

func TestGetenv(t *testing.T) {
	t.Setenv("KMAIL_TEST_VAR", "")
	if got := getenv("KMAIL_TEST_VAR", "fallback"); got != "fallback" {
		t.Errorf("getenv empty = %q, want fallback", got)
	}
	t.Setenv("KMAIL_TEST_VAR", "set-value")
	if got := getenv("KMAIL_TEST_VAR", "fallback"); got != "set-value" {
		t.Errorf("getenv set = %q, want set-value", got)
	}
}

func TestGetenvDuration(t *testing.T) {
	t.Setenv("KMAIL_TEST_DUR", "")
	if got := getenvDuration("KMAIL_TEST_DUR", 5*time.Second); got != 5*time.Second {
		t.Errorf("unset fallback = %v, want 5s", got)
	}

	t.Setenv("KMAIL_TEST_DUR", "250ms")
	if got := getenvDuration("KMAIL_TEST_DUR", 5*time.Second); got != 250*time.Millisecond {
		t.Errorf("parsed = %v, want 250ms", got)
	}

	t.Setenv("KMAIL_TEST_DUR", "not-a-duration")
	if got := getenvDuration("KMAIL_TEST_DUR", 5*time.Second); got != 5*time.Second {
		t.Errorf("bad parse fallback = %v, want 5s", got)
	}
}

func TestGetenvInt(t *testing.T) {
	t.Setenv("KMAIL_TEST_INT", "")
	if got := GetenvInt("KMAIL_TEST_INT", 42); got != 42 {
		t.Errorf("unset fallback = %d, want 42", got)
	}
	t.Setenv("KMAIL_TEST_INT", "7")
	if got := GetenvInt("KMAIL_TEST_INT", 42); got != 7 {
		t.Errorf("parsed = %d, want 7", got)
	}
	t.Setenv("KMAIL_TEST_INT", "not-a-number")
	if got := GetenvInt("KMAIL_TEST_INT", 42); got != 42 {
		t.Errorf("bad parse fallback = %d, want 42", got)
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "standard postgres dsn",
			in:   "postgresql://kmail:supersecret@localhost:5432/kmail?sslmode=disable",
			want: "postgresql://kmail:***@localhost:5432/kmail?sslmode=disable",
		},
		{
			name: "redis dsn",
			in:   "redis://user:pw@valkey:6379/0",
			want: "redis://user:***@valkey:6379/0",
		},
		{
			name: "no credentials",
			in:   "postgresql://localhost:5432/kmail",
			want: "postgresql://localhost:5432/kmail",
		},
		{
			name: "no scheme",
			in:   "localhost:5432",
			want: "localhost:5432",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactDSN(tc.in); got != tc.want {
				t.Errorf("redactDSN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestConfigString_RedactsPassword(t *testing.T) {
	cfg := &Config{
		HTTP:           HTTPConfig{Addr: ":8080"},
		DatabaseURL:    "postgresql://kmail:hunter2@localhost:5432/kmail",
		StalwartURL:    "http://stalwart:8080",
		ValkeyURL:      "valkey:6379",
		DevBypassToken: "dev",
	}
	s := cfg.String()
	if containsSubstring(s, "hunter2") {
		t.Errorf("Config.String leaked password: %q", s)
	}
	if !containsSubstring(s, "***") {
		t.Errorf("Config.String missing redaction marker: %q", s)
	}
	if !containsSubstring(s, "DevBypass=true") {
		t.Errorf("Config.String should report DevBypass=true, got: %q", s)
	}
}

func containsSubstring(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
