package config

import (
	"testing"
	"time"
)

func TestLoadReturnsDefaults(t *testing.T) {
	// Clear relevant env vars so defaults kick in regardless of
	// the developer's shell.
	for _, k := range []string{
		"KMAIL_API_ADDR",
		"KMAIL_API_READ_HEADER_TIMEOUT",
		"KMAIL_API_SHUTDOWN_TIMEOUT",
		"DATABASE_URL",
		"STALWART_URL",
		"VALKEY_URL",
		"KCHAT_OIDC_ISSUER",
		"KMAIL_DEV_BYPASS_TOKEN",
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
}

func TestLoadHonoursEnv(t *testing.T) {
	t.Setenv("KMAIL_API_ADDR", ":9999")
	t.Setenv("DATABASE_URL", "postgresql://u:p@db/kmail")
	t.Setenv("STALWART_URL", "http://stalwart:8080")
	t.Setenv("KMAIL_DEV_BYPASS_TOKEN", "dev-token")

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
