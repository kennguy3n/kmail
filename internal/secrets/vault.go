package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// vaultBackendName is the KMAIL_SECRETS_BACKEND value that selects
// the Vault provider.
const vaultBackendName = "vault"

func init() {
	// Vault uses only net/http, so unlike AWS/SOPS it is safe to
	// hard-link into the core binary and register unconditionally.
	RegisterBackend(vaultBackendName, newVaultFromConfig)
}

// VaultProvider reads secrets from a HashiCorp Vault KV v2 mount
// over its HTTP API. It deliberately avoids the official Vault Go
// client (a large dependency tree) — the read path we need is a
// single authenticated GET.
//
// A reference has the form "mount/path#field", e.g.
// "secret/kmail/prod#stripe_webhook". When "#field" is omitted the
// field defaults to "value" — the convention for single-value
// secrets. Results are cached for CacheTTL so a burst of resolves
// (every consumer asks at startup) is one round-trip, not N.
type VaultProvider struct {
	Addr     string // e.g. https://vault.internal:8200
	Token    string // X-Vault-Token
	CacheTTL time.Duration
	HTTP     *http.Client

	mu    sync.Mutex
	cache map[string]cachedSecret
}

type cachedSecret struct {
	value     string
	expiresAt time.Time
}

// newVaultFromConfig builds a VaultProvider. `config` is the Vault
// address; the token is read from the VAULT_TOKEN secret via the
// env backend so it never has to be passed on a command line. An
// empty address is a configuration error (selecting the vault
// backend without an address is never intentional).
func newVaultFromConfig(ctx context.Context, config string) (Provider, error) {
	addr := strings.TrimRight(strings.TrimSpace(config), "/")
	if addr == "" {
		return nil, fmt.Errorf("secrets/vault: KMAIL_SECRETS_BACKEND=vault requires an address (KMAIL_SECRETS_BACKEND_CONFIG / VAULT_ADDR)")
	}
	token, err := EnvProvider{}.Resolve(ctx, "VAULT_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("secrets/vault: VAULT_TOKEN required: %w", err)
	}
	return &VaultProvider{
		Addr:     addr,
		Token:    token,
		CacheTTL: 5 * time.Minute,
		HTTP:     &http.Client{Timeout: 10 * time.Second},
		cache:    map[string]cachedSecret{},
	}, nil
}

// Backend implements Provider.
func (v *VaultProvider) Backend() string { return vaultBackendName }

// Resolve fetches "mount/path#field" from the KV v2 mount.
func (v *VaultProvider) Resolve(ctx context.Context, ref string) (string, error) {
	if val, ok := v.fromCache(ref); ok {
		return val, nil
	}
	path, field := splitRef(ref)
	if path == "" {
		return "", fmt.Errorf("secrets/vault: empty reference")
	}
	// KV v2 inserts "data" between the mount and the secret path:
	//   <mount>/<rest>  ->  <mount>/data/<rest>
	mount, rest, ok := strings.Cut(path, "/")
	if !ok || rest == "" {
		return "", fmt.Errorf("secrets/vault: reference %q must be mount/path[#field]", ref)
	}
	url := fmt.Sprintf("%s/v1/%s/data/%s", v.Addr, mount, rest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Vault-Token", v.Token)

	resp, err := v.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("secrets/vault: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: vault %q", ErrSecretNotFound, ref)
	default:
		return "", fmt.Errorf("secrets/vault: GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("secrets/vault: decode response: %w", err)
	}
	raw, ok := parsed.Data.Data[field]
	if !ok {
		return "", fmt.Errorf("%w: vault %q field %q", ErrSecretNotFound, ref, field)
	}
	val, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("secrets/vault: %q field %q is not a string", ref, field)
	}
	v.store(ref, val)
	return val, nil
}

func (v *VaultProvider) fromCache(ref string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.cache[ref]
	if !ok || time.Now().After(c.expiresAt) {
		return "", false
	}
	return c.value, true
}

func (v *VaultProvider) store(ref, val string) {
	if v.CacheTTL <= 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cache[ref] = cachedSecret{value: val, expiresAt: time.Now().Add(v.CacheTTL)}
}

// splitRef splits "mount/path#field" into ("mount/path", "field").
// A missing "#field" defaults to "value".
func splitRef(ref string) (path, field string) {
	path, field, ok := strings.Cut(ref, "#")
	if !ok || field == "" {
		field = "value"
	}
	return path, field
}
