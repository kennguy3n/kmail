// Package secrets is the pluggable indirection between KMail and
// wherever its secret *values* actually live. Today every secret
// (KMAIL_SECRETS_KEY, the Stripe webhook secret, the KChat API
// token, …) is read straight from an environment variable. That is
// fine for dev and for a Kubernetes Secret mounted as env, but it
// makes two production requirements awkward:
//
//  1. Sourcing secrets from a real secret manager (HashiCorp
//     Vault, AWS Secrets Manager, SOPS-decrypted files) without
//     threading bespoke client code through every call site.
//
//  2. Rotating the kmail-secrets master key (KMAIL_SECRETS_KEY)
//     without downtime — see rotation.go.
//
// This package addresses (1) with a small Provider interface plus
// a backend registry, and (2) with RotatingEnvelope. The Provider
// abstraction is intentionally narrow (resolve a reference to a
// string) so backends stay trivial to add and to audit. The env
// backend is always registered; Vault is registered by vault.go;
// AWS Secrets Manager / SOPS can be registered by the deployment
// via RegisterBackend without modifying this package (so the
// heavier cloud SDKs never become a build dependency of the core
// API binary).
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// ErrSecretNotFound is returned by Provider.Resolve when the named
// secret does not exist in the backing store. It is distinct from a
// transport / auth error so callers can treat "unset" (often fine —
// fall back to a default) differently from "Vault is unreachable"
// (never silently defaulted).
var ErrSecretNotFound = errors.New("secrets: not found")

// Provider resolves a secret reference to its plaintext value.
//
// `ref` is backend-specific: for the env backend it is an
// environment variable name; for Vault it is a "mount/path#field"
// selector (see vault.go). Implementations MUST return
// ErrSecretNotFound (wrapped is fine) when the reference resolves
// to nothing, and a descriptive error for transport/auth failures.
type Provider interface {
	// Resolve returns the secret value for ref.
	Resolve(ctx context.Context, ref string) (string, error)
	// Backend names the provider for logs / health output
	// ("env", "vault", "aws-secrets-manager", …).
	Backend() string
}

// EnvProvider resolves secrets from environment variables using the
// same KMAIL_-prefix-then-bare-name lookup that internal/config
// uses, so a reference works whether the Helm chart set
// KMAIL_FOO or a dev shell exported FOO.
type EnvProvider struct{}

// Backend implements Provider.
func (EnvProvider) Backend() string { return "env" }

// Resolve looks up KMAIL_<ref> first, then <ref>. An empty value is
// treated as unset (ErrSecretNotFound) — an exported-but-empty var
// is almost always a misconfiguration, not an intentional empty
// secret.
func (EnvProvider) Resolve(_ context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("secrets/env: empty reference")
	}
	if v, ok := os.LookupEnv("KMAIL_" + ref); ok && v != "" {
		return v, nil
	}
	if v, ok := os.LookupEnv(ref); ok && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%w: env %q (also tried KMAIL_%s)", ErrSecretNotFound, ref, ref)
}

// BackendFactory builds a Provider from its backend-specific
// configuration string (e.g. a Vault address, an AWS region). The
// meaning of `config` is the backend's own; for env it is ignored.
type BackendFactory func(ctx context.Context, config string) (Provider, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]BackendFactory{
		"env": func(context.Context, string) (Provider, error) { return EnvProvider{}, nil },
	}
)

// RegisterBackend makes a backend available to New. It is the
// extension point for backends whose client libraries we do not
// want to hard-link into the core binary (AWS Secrets Manager,
// SOPS, GCP Secret Manager): the deployment's main package imports
// a small adapter that calls RegisterBackend in its init, and the
// operator selects it via KMAIL_SECRETS_BACKEND. Registering the
// same name twice panics — a duplicate registration is always a
// programming error (two init funcs fighting over one name), and
// failing loudly at startup beats a silent last-writer-wins.
func RegisterBackend(name string, factory BackendFactory) {
	if name == "" || factory == nil {
		panic("secrets: RegisterBackend requires a non-empty name and factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("secrets: backend %q already registered", name))
	}
	registry[name] = factory
}

// RegisteredBackends returns the sorted set of backend names known
// to New. Exported for the /healthz / config-dump surface and for
// the error message New emits on an unknown backend.
func RegisteredBackends() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New builds the Provider selected by `backend` (typically
// KMAIL_SECRETS_BACKEND). An empty backend defaults to "env" so the
// zero-config dev path keeps working. `config` is passed verbatim
// to the backend factory.
func New(ctx context.Context, backend, config string) (Provider, error) {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		backend = "env"
	}
	registryMu.RLock()
	factory, ok := registry[backend]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("secrets: unknown backend %q (registered: %s)",
			backend, strings.Join(RegisteredBackends(), ", "))
	}
	return factory(ctx, config)
}
