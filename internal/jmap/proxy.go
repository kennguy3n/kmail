// Package jmap hosts the Go BFF's JMAP proxy: speaks JMAP to
// Stalwart on behalf of the React client, translates KChat OIDC
// auth into Stalwart auth, enforces tenant policy, and manages
// capability negotiation.
//
// See `docs/JMAP-CONTRACT.md` for the contract this package
// implements against, and `docs/ARCHITECTURE.md` §7 for the Go
// service topology.
package jmap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ShardResolver is the slice of `tenant.ShardService` the JMAP
// proxy needs. Defining it here lets the proxy depend on a narrow
// interface and lets tests stub the resolver without touching the
// full ShardService surface.
type ShardResolver interface {
	GetTenantShard(ctx context.Context, tenantID string) (string, error)
	GetSecondaryShards(ctx context.Context, tenantID string) ([]string, error)
}

// ProxyConfig wires the JMAP reverse proxy. `StalwartURL` is the
// internal Stalwart JMAP endpoint (e.g., `http://stalwart:8080` in
// the local compose stack, `https://kmail-stalwart-0.kmail-stalwart.svc:8443`
// in production where mTLS is mandatory). `Pool` is used to
// resolve the acting user's Stalwart account ID per
// `docs/JMAP-CONTRACT.md` §3.3. `Logger` is optional; if nil, a
// logger writing to the default output is used.
type ProxyConfig struct {
	StalwartURL string
	Pool        *pgxpool.Pool
	Logger      *log.Logger

	// TLS, when non-nil, configures the BFF→Stalwart transport for
	// mutual TLS authentication. In production the BFF presents a
	// per-pod client certificate issued by cert-manager so
	// Stalwart can authenticate the caller cryptographically
	// rather than relying on a trusted-network posture
	// (`docs/ARCHITECTURE.md` §7). When nil, the transport falls
	// back to whatever scheme `StalwartURL` declares — plain HTTP
	// in compose dev, HTTPS without a client cert in staging.
	TLS *ClientTLSConfig

	// DevStalwartAuthHeader, when non-empty, is the exact value the
	// proxy sets on the outbound `Authorization` header instead of
	// stripping it. It exists ONLY for the dev/CI stack: the
	// official Stalwart image cannot authenticate the BFF over the
	// production mTLS header-trust path (it does not implement the
	// `X-KMail-*` trust feature), so to exercise the real mail data
	// plane the dev BFF authenticates with the recovery-admin Basic
	// credential instead. In production this stays empty and the
	// proxy strips `Authorization`, deferring to the mTLS client
	// certificate for caller authentication (see
	// `docs/JMAP-CONTRACT.md` §3.2). Wired in `cmd/kmail-api/main.go`
	// only when `middleware.IsDevEnv(cfg.Env)` is true.
	DevStalwartAuthHeader string

	// AccountCacheTTL controls how long the `(tenant_id, kchat_user_id)
	// → stalwart_account_id` cache entries live. Defaults to 5
	// minutes per `docs/JMAP-CONTRACT.md` §3.3.
	AccountCacheTTL time.Duration

	// Shards resolves the per-tenant Stalwart URL + failover
	// list. nil = single-shard deployment, every request goes to
	// `StalwartURL`. Wired in `cmd/kmail-api/main.go` for the
	// production multi-shard topology.
	Shards ShardResolver

	// CircuitBreakThreshold is the consecutive 5xx / transport
	// failure count after which the proxy marks a shard URL
	// unhealthy and routes to the next backup. Defaults to 3.
	// Ignored when `Breaker` is non-nil — a custom breaker is
	// expected to own its own threshold configuration.
	CircuitBreakThreshold int

	// CircuitBreakCooldown and CircuitBreakWindow tune the
	// fallback in-process breaker so its sliding-window /
	// cooldown semantics line up with the Redis-backed
	// implementation. Defaults: 30s cooldown, 60s window — the
	// same values the production main wires into
	// `RedisCircuitBreakerConfig`. Ignored when `Breaker` is
	// non-nil.
	//
	// Setting both to zero falls back to the legacy count-only
	// behavior used by older tests that never advance a clock.
	CircuitBreakCooldown time.Duration
	CircuitBreakWindow   time.Duration

	// Breaker is the optional shared circuit breaker. Wire a
	// `*RedisCircuitBreaker` (via `NewRedisCircuitBreaker`) to
	// share trip state across BFF pods so a 5xx storm against
	// shard X opens the breaker once across the fleet instead of
	// once per pod. When nil, the proxy falls back to a
	// per-process counter map that matches the original Phase 4
	// behavior — single-pod deployments are not affected.
	Breaker CircuitBreaker

	// PreDeliverHook (Phase 8) is invoked over the submit body
	// before forwarding it to Stalwart. Returning a non-nil
	// error short-circuits the request with 422 (and a JMAP
	// `urn:ietf:params:jmap:error:rejectedByPolicy` payload). nil
	// means "no pre-delivery checks", which is the default.
	//
	// In production this is wired to `malware.Handlers.PreDeliverHook`
	// from `internal/malware`, behind the `KMAIL_CLAMAV_ADDR` env
	// var. The hook only runs on writes (POST/PUT) — JMAP is a
	// JSON-RPC-style protocol so the body is small and re-readable.
	PreDeliverHook func(ctx context.Context, body []byte) error

	// SendInterceptor (Phase 9 — Undo Send, WS3) is the optional
	// hook that owns `EmailSubmission/set` create traffic when the
	// client opts in via the `X-KMail-Undo-Send: true` header. The
	// interceptor:
	//
	//   • Forwards the `Email/set` portion of the batch to Stalwart
	//     so the underlying draft is still minted.
	//   • Holds the `EmailSubmission/set` portion in Valkey with a
	//     deadline.
	//   • Writes a synthesised JMAP response (plus undo-send headers)
	//     directly to the response writer.
	//
	// Returning `intercepted=true` means the interceptor has fully
	// served the response and the proxy MUST NOT forward upstream.
	// `intercepted=false` means "not my request, continue as usual"
	// and the proxy forwards normally.
	//
	// Wired in `cmd/kmail-api/main.go` from `internal/undosend`.
	SendInterceptor SendInterceptor
}

// SendInterceptor is the optional Undo-Send hook surface. See
// the `ProxyConfig.SendInterceptor` doc for semantics.
type SendInterceptor interface {
	Intercept(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte) (intercepted bool, err error)
}

// ChainSendInterceptors composes multiple SendInterceptors into
// a single one. Each member is offered the request in order. The
// first member that returns `intercepted=true` (or any non-nil
// error) wins; subsequent members are not invoked.
//
// The chain is the architecturally clean way to register N
// independent send hooks (Undo Send + Scheduled Send today;
// future features tomorrow) without coupling them to each other.
// Each hook is header-gated and self-contained, so the order
// inside the chain only matters when a single request could
// match more than one — in which case the first-registered hook
// wins by design.
func ChainSendInterceptors(hooks ...SendInterceptor) SendInterceptor {
	filtered := make([]SendInterceptor, 0, len(hooks))
	for _, h := range hooks {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &chainedSendInterceptor{hooks: filtered}
}

type chainedSendInterceptor struct {
	hooks []SendInterceptor
}

func (c *chainedSendInterceptor) Intercept(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte) (bool, error) {
	for _, h := range c.hooks {
		intercepted, err := h.Intercept(ctx, w, r, body)
		if err != nil {
			return intercepted, err
		}
		if intercepted {
			return true, nil
		}
	}
	return false, nil
}

// ClientTLSConfig configures the BFF→Stalwart mTLS transport.
//
// The expected layout in production is that cert-manager issues a
// short-lived (24h) leaf certificate for each BFF pod from an
// internal Issuer/ClusterIssuer, mounted via a Kubernetes Secret
// onto `/etc/kmail/tls`. The Stalwart server is configured to
// trust the same root and demand a client certificate (TLS
// `verify_client = required`).
//
// `CAFile` is the PEM bundle that pins which CAs Stalwart's
// server certificate must chain to. `ServerName` is the SNI /
// `tls.Config.ServerName` value. Leaving it empty lets Go's
// transport derive SNI per-connection from each upstream URL —
// the correct default for shard failover, where the secondary's
// certificate may not carry the primary's hostname. Set it
// explicitly only when the upstream URL host does not match the
// SAN on Stalwart's server cert (e.g. when going through a
// pod-local sidecar). `MinVersion` raises the floor; the
// transport never speaks below TLS 1.2 even when this is zero.
type ClientTLSConfig struct {
	CertFile   string
	KeyFile    string
	CAFile     string
	ServerName string
	MinVersion uint16
}

// validate returns an error when the config is unusable. Empty
// configs are caught here so callers don't have to repeat the
// guard.
func (c *ClientTLSConfig) validate() error {
	if c == nil {
		return errors.New("jmap.ClientTLSConfig: nil receiver")
	}
	if strings.TrimSpace(c.CertFile) == "" {
		return errors.New("jmap.ClientTLSConfig: CertFile is required")
	}
	if strings.TrimSpace(c.KeyFile) == "" {
		return errors.New("jmap.ClientTLSConfig: KeyFile is required")
	}
	return nil
}

// clientTLSBuild bundles a base *tls.Config with the trust-root
// loader and pinned ServerName so the transport can mint a fresh
// per-connection config from the dial target. The per-connection
// path exists specifically to keep hostname verification correct
// when the upstream URL is an IP literal — Go's TLS stack strips
// SNI for IP literals per RFC 6066, so `state.ServerName` in a
// shared VerifyConnection callback would be empty and `DNSName: ""`
// in `x509.VerifyOptions` would silently skip the hostname check
// entirely. The mitigation: build VerifyConnection per dial, with
// the dial host (which may be an IP literal) closed over so it
// becomes the `DNSName` passed to `x509.Certificate.Verify` — Go's
// verifier then enforces the cert's `IPAddresses` SAN list against
// that value instead of falling through to chain-only verification.
type clientTLSBuild struct {
	base           *tls.Config
	caLoader       *caPoolLoader
	pinnedSrvName  string
	customVerifier bool // true when CAFile was set; otherwise the stdlib verifier is fine
}

// build assembles the *tls.Config that the BFF transport presents
// to Stalwart. The initial keypair and CA bundle loads run at
// startup so a misconfigured deployment fails fast on boot;
// subsequent rotations are picked up on the next handshake via
// the `GetClientCertificate` and `VerifyConnection` callbacks
// (see keypairLoader / caPoolLoader). This means cert-manager
// rotation — of either the BFF leaf certificate OR the trust
// root — continues to work even in clusters where the Reloader
// controller is not installed.
func (c *ClientTLSConfig) build(logger *log.Logger) (*clientTLSBuild, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	kpLoader := &keypairLoader{
		certFile: c.CertFile,
		keyFile:  c.KeyFile,
		logger:   logger,
	}
	// Validate the on-disk keypair once at startup so we surface
	// `bad cert / bad key` to the operator before serving traffic
	// rather than at the first handshake.
	if _, err := kpLoader.load(); err != nil {
		return nil, fmt.Errorf("jmap.ClientTLSConfig: load keypair: %w", err)
	}
	min := c.MinVersion
	if min == 0 {
		min = tls.VersionTLS12
	}
	b := &clientTLSBuild{
		base: &tls.Config{
			GetClientCertificate: kpLoader.get,
			MinVersion:           min,
			ServerName:           c.ServerName,
		},
		pinnedSrvName: c.ServerName,
	}
	if strings.TrimSpace(c.CAFile) != "" {
		b.caLoader = &caPoolLoader{
			caFile: c.CAFile,
			logger: logger,
		}
		// Validate the trust root at startup for the same fail-fast
		// reason as the keypair. The pool is cached and re-loaded on
		// the next handshake whenever the file's mtime changes.
		if _, err := b.caLoader.load(); err != nil {
			return nil, fmt.Errorf("jmap.ClientTLSConfig: load CA bundle: %w", err)
		}
		b.customVerifier = true
	}
	return b, nil
}

// perConnConfig clones the base config and stamps in the dial host
// so VerifyConnection has correct DNSName / IPAddresses verification
// even when SNI is suppressed (IP-literal URLs). The returned config
// is safe for `tls.Client` — every connection gets its own clone.
//
// `dialHost` is the host portion of the dial address (no port). It
// may be a hostname, an IPv4 / IPv6 literal, or — in the unusual
// case of a malformed dial — empty. An empty `dialHost` combined
// with an empty `pinnedSrvName` is the failure case that motivated
// this refactor: previously it slipped through as chain-only
// verification; now it produces a hard handshake error.
func (b *clientTLSBuild) perConnConfig(dialHost string) *tls.Config {
	cfg := b.base.Clone()
	// Prefer the operator's explicit ServerName pin if one is set
	// — this is the documented escape hatch when the upstream URL
	// host doesn't match the SAN on Stalwart's server cert (e.g.
	// going through a pod-local sidecar). Otherwise, fall back to
	// the dial host. Setting `ServerName` here drives BOTH Go's
	// per-connection SNI emission and `state.ServerName` in the
	// VerifyConnection callback below.
	if cfg.ServerName == "" {
		cfg.ServerName = dialHost
	}
	if !b.customVerifier {
		return cfg
	}
	// `InsecureSkipVerify=true` *combined with* `VerifyConnection`
	// is the documented Go stdlib pattern for swapping in a custom
	// verifier — it disables the built-in cert chain check so we
	// can do it ourselves against a freshly-loaded pool. The logic
	// below is otherwise identical to the default behavior: it
	// pins the chain to our CA roots AND enforces the dial host
	// matches a SAN (DNS name OR IPAddresses entry, depending on
	// whether dialHost parses as an IP literal).
	cfg.InsecureSkipVerify = true
	pinned := b.pinnedSrvName
	loader := b.caLoader
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("jmap.ClientTLSConfig: peer presented no certificate")
		}
		pool, err := loader.load()
		if err != nil {
			return fmt.Errorf("jmap.ClientTLSConfig: load CA bundle: %w", err)
		}
		// Resolve the name that must appear in the SAN list, in
		// priority order:
		//
		//   1. The operator's explicit `ServerName` pin (overrides
		//      everything; intentional for sidecar / proxy cases).
		//   2. `state.ServerName` — populated by Go when SNI was
		//      sent (the common hostname case).
		//   3. The dial host — used when SNI was suppressed because
		//      the URL host is an IP literal (RFC 6066). Go's x509
		//      verifier treats `DNSName` that parses as an IP as a
		//      check against the cert's `IPAddresses` SAN, so this
		//      is the correct value to pass even though the field
		//      is named `DNSName`.
		verifyName := pinned
		if verifyName == "" {
			verifyName = state.ServerName
		}
		if verifyName == "" {
			verifyName = dialHost
		}
		if verifyName == "" {
			// Should be impossible — the transport's DialTLSContext
			// always passes a non-empty host. Treat as a hard error
			// rather than silently falling through to chain-only
			// verification, which would let a server impersonating
			// any other Stalwart on the same root authenticate.
			return errors.New("jmap.ClientTLSConfig: no name available for hostname verification (empty SNI and empty dial host); refusing chain-only verification")
		}
		opts := x509.VerifyOptions{
			Roots:         pool,
			Intermediates: x509.NewCertPool(),
			DNSName:       verifyName,
		}
		for _, ic := range state.PeerCertificates[1:] {
			opts.Intermediates.AddCert(ic)
		}
		if _, err := state.PeerCertificates[0].Verify(opts); err != nil {
			return fmt.Errorf("jmap.ClientTLSConfig: verify peer: %w", err)
		}
		return nil
	}
	return cfg
}

// keypairLoader is the `GetClientCertificate` provider used by
// the mTLS transport. It caches the most-recently-loaded keypair
// and re-reads the underlying files whenever either file's mtime
// changes, so cert-manager rotations land on the next handshake.
//
// The cache is keyed on a tuple of (cert mtime, key mtime). A
// shared RWMutex protects the cache; the common path (no
// rotation) takes the read lock and returns immediately.
type keypairLoader struct {
	certFile string
	keyFile  string
	logger   *log.Logger
	// now is the wall-clock source used by the expiry check.
	// Defaults to time.Now; tests inject a fixed clock to make
	// the WARN threshold deterministic.
	now func() time.Time

	mu        sync.RWMutex
	cert      *tls.Certificate
	certMTime time.Time
	keyMTime  time.Time
	// lastExpiryWarn dedup's the WARN log so a single near-
	// expiry cert doesn't spam the log on every reload check.
	// The map key is the leaf NotAfter so a rotated cert with
	// a new horizon emits a fresh WARN if it also lands inside
	// the threshold.
	lastExpiryWarn time.Time
}

// certExpiryWarnThreshold is the maximum remaining lifetime
// below which the keypair loader logs a WARN on every reload
// (deduplicated per leaf NotAfter). The default Helm chart
// configures cert-manager for 24h certs with 8h renewal, so any
// remaining lifetime < 24h means either renewal is broken or
// the Reloader controller never restarted the pod — both are
// production-affecting conditions an operator needs to see in
// the BFF logs.
const certExpiryWarnThreshold = 24 * time.Hour

// get satisfies tls.Config.GetClientCertificate.
func (l *keypairLoader) get(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return l.load()
}

// load returns the cached *tls.Certificate, reloading from disk
// if either underlying file has been replaced since the last read.
func (l *keypairLoader) load() (*tls.Certificate, error) {
	certInfo, err := os.Stat(l.certFile)
	if err != nil {
		return nil, fmt.Errorf("jmap.keypairLoader: stat cert %q: %w", l.certFile, err)
	}
	keyInfo, err := os.Stat(l.keyFile)
	if err != nil {
		return nil, fmt.Errorf("jmap.keypairLoader: stat key %q: %w", l.keyFile, err)
	}

	l.mu.RLock()
	if l.cert != nil && certInfo.ModTime().Equal(l.certMTime) && keyInfo.ModTime().Equal(l.keyMTime) {
		cert := l.cert
		l.mu.RUnlock()
		return cert, nil
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	// Double-check under the write lock — another goroutine may
	// have reloaded while we were upgrading.
	if l.cert != nil && certInfo.ModTime().Equal(l.certMTime) && keyInfo.ModTime().Equal(l.keyMTime) {
		return l.cert, nil
	}

	loaded, err := tls.LoadX509KeyPair(l.certFile, l.keyFile)
	if err != nil {
		return nil, fmt.Errorf("jmap.keypairLoader: reload keypair: %w", err)
	}
	prev := l.cert
	l.cert = &loaded
	l.certMTime = certInfo.ModTime()
	l.keyMTime = keyInfo.ModTime()
	if l.logger != nil {
		leafNotAfter := "unknown"
		leafSubject := "unknown"
		var leaf *x509.Certificate
		if len(loaded.Certificate) > 0 {
			if parsed, perr := x509.ParseCertificate(loaded.Certificate[0]); perr == nil {
				leaf = parsed
				leafNotAfter = parsed.NotAfter.UTC().Format(time.RFC3339)
				leafSubject = parsed.Subject.CommonName
				if leafSubject == "" && len(parsed.DNSNames) > 0 {
					leafSubject = parsed.DNSNames[0]
				}
			}
		}
		if prev == nil {
			l.logger.Printf("jmap proxy: loaded client TLS keypair subject=%q notAfter=%s", leafSubject, leafNotAfter)
		} else {
			l.logger.Printf("jmap proxy: rotated client TLS keypair subject=%q notAfter=%s", leafSubject, leafNotAfter)
		}
		if leaf != nil {
			l.maybeWarnNearExpiryLocked(leaf, leafSubject)
		}
	}
	return l.cert, nil
}

// maybeWarnNearExpiryLocked emits a WARN when the loaded leaf is
// within `certExpiryWarnThreshold` of expiry — or already past
// it. The caller MUST hold l.mu in write mode. The WARN is
// dedup'd per (leaf NotAfter) so a near-expiry cert that gets
// re-checked on every handshake doesn't spam the log; an
// operational rotation that lands a fresh cert resets the
// dedup key naturally.
func (l *keypairLoader) maybeWarnNearExpiryLocked(leaf *x509.Certificate, subject string) {
	now := l.clockLocked()
	remaining := leaf.NotAfter.Sub(now)
	if remaining > certExpiryWarnThreshold {
		return
	}
	if l.lastExpiryWarn.Equal(leaf.NotAfter) {
		return
	}
	l.lastExpiryWarn = leaf.NotAfter
	switch {
	case remaining <= 0:
		l.logger.Printf(
			"WARN: jmap proxy: client TLS keypair subject=%q is EXPIRED (notAfter=%s, expired %s ago); "+
				"cert-manager renewal may be broken or Reloader did not restart the pod.",
			subject, leaf.NotAfter.UTC().Format(time.RFC3339), -remaining.Round(time.Second),
		)
	default:
		l.logger.Printf(
			"WARN: jmap proxy: client TLS keypair subject=%q expires in %s (notAfter=%s); "+
				"verify cert-manager renewal is healthy and Reloader is installed so the BFF picks up the rotation.",
			subject, remaining.Round(time.Second), leaf.NotAfter.UTC().Format(time.RFC3339),
		)
	}
}

// clockLocked returns the keypair loader's wall-clock instant.
// Pulled into a helper so tests can inject a deterministic clock
// via the `now` field.
func (l *keypairLoader) clockLocked() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// caPoolLoader is the dynamic trust-root provider used by the
// mTLS transport. It mirrors keypairLoader for the CA bundle:
// each handshake stats the file, returns the cached pool when
// unchanged, and re-parses the on-disk PEM whenever mtime moves
// forward. CA rotations are far rarer than leaf rotations (the
// internal PKI root typically lasts years) but the same
// "rotation works without Reloader" guarantee applies — when
// cert-manager updates the CA bundle in the mounted Secret we
// pick it up on the next request.
type caPoolLoader struct {
	caFile string
	logger *log.Logger

	mu     sync.RWMutex
	pool   *x509.CertPool
	mtime  time.Time
	digest string
}

// load returns the cached *x509.CertPool, re-parsing the PEM
// bundle from disk whenever the underlying file has been
// replaced since the last successful load.
func (l *caPoolLoader) load() (*x509.CertPool, error) {
	info, err := os.Stat(l.caFile)
	if err != nil {
		return nil, fmt.Errorf("jmap.caPoolLoader: stat CA %q: %w", l.caFile, err)
	}

	l.mu.RLock()
	if l.pool != nil && info.ModTime().Equal(l.mtime) {
		pool := l.pool
		l.mu.RUnlock()
		return pool, nil
	}
	l.mu.RUnlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	// Double-check under the write lock.
	if l.pool != nil && info.ModTime().Equal(l.mtime) {
		return l.pool, nil
	}

	pem, err := os.ReadFile(l.caFile)
	if err != nil {
		return nil, fmt.Errorf("jmap.caPoolLoader: read CA %q: %w", l.caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("jmap.caPoolLoader: CA bundle %q contained no usable certs", l.caFile)
	}
	// SHA-256 digest of the PEM bytes lets us suppress log spam
	// when the file mtime changes but the contents are byte-
	// identical (e.g. a noop reconciliation in cert-manager).
	sum := sha256.Sum256(pem)
	digest := hex.EncodeToString(sum[:])
	prev := l.pool
	l.pool = pool
	l.mtime = info.ModTime()
	prevDigest := l.digest
	l.digest = digest
	if l.logger != nil && digest != prevDigest {
		if prev == nil {
			l.logger.Printf("jmap proxy: loaded CA bundle sha256=%s", digest)
		} else {
			l.logger.Printf("jmap proxy: rotated CA bundle sha256=%s (was %s)", digest, prevDigest)
		}
	}
	return l.pool, nil
}

// isBareSvcHostname reports whether `host` is a Kubernetes
// in-cluster DNS short form ending in `.svc` but not the FQDN
// `.svc.cluster.local`. Cert-manager's Certificate resource
// generates SANs for the FQDN form only (see
// `templates/stalwart-mtls.yaml`), so any `.svc`-only hostname
// will fail TLS hostname verification. The Helm chart's default
// `KMAIL_STALWART_URL` and operator overrides both go through
// this check.
func isBareSvcHostname(host string) bool {
	if !strings.HasSuffix(host, ".svc") {
		return false
	}
	// Already-FQDN forms (`.svc.cluster.local`, `.svc.example.com`)
	// are excluded because they DO match the cert SAN list.
	return !strings.Contains(host, ".svc.")
}

// newClientTLSTransport returns an *http.Transport configured for
// mTLS to Stalwart. The dialer and timeout values match the
// stdlib defaults (`http.DefaultTransport`) so retry / dial
// behaviour is unchanged when the only addition is a client cert.
//
// The transport installs a `DialTLSContext` wrapper rather than
// relying on `TLSClientConfig` + Go's auto-promotion, because the
// auto-promoted config does not give us the dial host before the
// handshake. We need it to keep hostname verification correct for
// IP-literal upstream URLs — see clientTLSBuild.perConnConfig for
// the full rationale. The wrapper:
//
//  1. Dials TCP exactly like the stdlib default.
//  2. Splits the dial address into host:port and uses the host
//     (an FQDN, an IPv4 literal, or `[IPv6]`-stripped) to build a
//     per-connection *tls.Config.
//  3. Performs the TLS handshake explicitly with HandshakeContext
//     so cancellation propagates and the handshake-deadline
//     observance matches `TLSHandshakeTimeout`.
func newClientTLSTransport(b *clientTLSBuild) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Pointer-equal `b.base` is INTENTIONAL and load-bearing
		// for HTTP/2 negotiation; do not "decouple" the transport's
		// TLSClientConfig from the per-connection clone source.
		//
		// When `ForceAttemptHTTP2` is true, the net/http package's
		// `http2configureTransports` registration mutates
		// `TLSClientConfig.NextProtos` to prepend `"h2"` so ALPN
		// announces HTTP/2 on the wire. Because `perConnConfig`
		// clones `b.base` (which IS this same pointer) at dial
		// time, every per-connection *tls.Config inherits the
		// HTTP/2-aware NextProtos and the handshake negotiates
		// HTTP/2 cleanly. A future refactor that points
		// `TLSClientConfig` at a *different* *tls.Config than
		// `perConnConfig` clones from would silently downgrade
		// every BFF→Stalwart request to HTTP/1.1.
		//
		// The actual handshake runs in DialTLSContext below; this
		// field is kept populated so the transport's HTTP/2 setup
		// path has a config to mutate and ALPN works as expected.
		TLSClientConfig: b.base,
	}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			// Fall back to the raw addr; if it's malformed the
			// dial below will surface a clearer error.
			host = addr
		}
		raw, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		perConn := b.perConnConfig(host)
		// Match the transport's TLSHandshakeTimeout so an
		// unresponsive server fails the dial promptly rather than
		// hanging the whole HTTP request.
		hsCtx, cancel := context.WithTimeout(ctx, tr.TLSHandshakeTimeout)
		defer cancel()
		tlsConn := tls.Client(raw, perConn)
		if err := tlsConn.HandshakeContext(hsCtx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return tr
}

// Proxy forwards authenticated JMAP requests from the React client
// to Stalwart, injecting the acting user's Stalwart account ID
// (resolved and cached from Postgres) into the `X-KMail-Stalwart-Account-Id`
// header for the downstream.
//
// Production hardening: the BFF presents a mutual-TLS client
// certificate to Stalwart (see `ClientTLSConfig` and the
// cert-manager Certificate resource in the Helm chart). Stalwart
// is configured to require a client cert (`verify_client = required`)
// and pins the BFF's issuing CA. This replaces the trusted-network
// posture used in early Phase 4 development.
//
// Phase 4 adds shard-aware routing: when `cfg.Shards` is wired, the
// proxy resolves each tenant's primary Stalwart URL on every
// request and falls back to the configured secondary shards on
// 5xx / transport errors. Falls back to `cfg.StalwartURL` for
// tenants without a shard assignment so single-shard dev stays
// working.
type Proxy struct {
	cfg     ProxyConfig
	rp      *httputil.ReverseProxy
	logger  *log.Logger
	cache   *accountCache
	target  *url.URL

	// base is the underlying BFF→Stalwart transport (mTLS-aware
	// when `cfg.TLS != nil`, the default transport otherwise).
	// The reverse proxy wraps this in a `shardFailoverTransport`
	// to drive the proxy retries; BFF-internal callers (see
	// `InternalClient`) reuse the bare base via
	// `Proxy.BaseTransport` so they get the same certificate
	// material + dialer tuning without inheriting the proxy's
	// per-request shard-list context plumbing.
	base http.RoundTripper

	// breaker is the active circuit-breaker implementation. It's
	// either the per-process default (a thin wrapper over a
	// counter map) or a `*RedisCircuitBreaker` wired by the
	// embedder. The `Open / RecordSuccess / RecordFailure`
	// surface is consulted by `shardFailoverTransport.RoundTrip`
	// on every retry decision.
	breaker CircuitBreaker

	// sendInterceptor is the optional Undo-Send hook. Loaded
	// atomically because it is wired by `cmd/kmail-api/main.go`
	// *after* `NewProxy` returns (the interceptor depends on
	// `InternalClient` which depends on the proxy — see
	// `SetSendInterceptor` for the wiring rationale).
	sendInterceptor atomic.Pointer[sendInterceptorRef]
}

// sendInterceptorRef wraps the interface so atomic.Pointer’s
// type-parameter constraint is satisfied (interface values cannot
// be stored in atomic.Pointer directly).
type sendInterceptorRef struct{ v SendInterceptor }

// SetSendInterceptor swaps in (or clears) the Undo-Send hook at
// runtime. Safe for concurrent use with `ServeHTTP`. Passing nil
// disables interception entirely — the atomic.Pointer is the sole
// source of truth for the interceptor, so a Store(nil) call wins
// even if `ProxyConfig.SendInterceptor` was wired at NewProxy
// time (the cfg value is migrated into the atomic on construction
// so there is no orphaned fallback path that could resurrect a
// disabled hook).
//
// Wiring callers should call this exactly once at startup, before
// the HTTP listener starts accepting client traffic, so the
// initial-request cohort observes the wired interceptor.
func (p *Proxy) SetSendInterceptor(s SendInterceptor) {
	if s == nil {
		p.sendInterceptor.Store(nil)
		return
	}
	p.sendInterceptor.Store(&sendInterceptorRef{v: s})
}

// loadSendInterceptor returns the currently-wired interceptor or
// nil if none. The atomic.Pointer is the single source of truth;
// `ProxyConfig.SendInterceptor` is migrated into the atomic at
// NewProxy time so there is no separate cfg fallback for
// `SetSendInterceptor(nil)` to leak past.
func (p *Proxy) loadSendInterceptor() SendInterceptor {
	if ref := p.sendInterceptor.Load(); ref != nil {
		return ref.v
	}
	return nil
}

// BaseTransport returns the underlying BFF→Stalwart RoundTripper
// used by the proxy. The transport is mTLS-aware when the proxy
// was built with a non-nil `ProxyConfig.TLS`, otherwise it is
// `http.DefaultTransport`. `InternalClient` reuses it so
// BFF-initiated JMAP requests (e.g. `/api/v1/sync/bootstrap`)
// observe the same handshake / dialer tuning as proxied traffic.
func (p *Proxy) BaseTransport() http.RoundTripper { return p.base }

// Target returns the configured Stalwart base URL. Used as the
// default upstream for `InternalClient` when no per-tenant shard
// is configured.
func (p *Proxy) Target() *url.URL { return p.target }

// Logger returns the proxy's logger so colocated helpers can emit
// to the same destination.
func (p *Proxy) Logger() *log.Logger { return p.logger }

// ResolveAccountID returns the Stalwart account ID for the given
// `(tenant_id, kchat_user_id)` pair, hitting the in-process cache
// and falling through to Postgres on a miss. Exposes the proxy's
// internal `resolveAccount` to colocated helpers like
// `InternalClient` so BFF-initiated JMAP requests share the same
// `(tenant, user) → account_id` lookup path as proxied requests —
// the cache stays consistent across both call sites and we don't
// double the Postgres query rate on a cold pod.
func (p *Proxy) ResolveAccountID(ctx context.Context, tenantID, kchatUserID string) (string, error) {
	return p.resolveAccount(ctx, tenantID, kchatUserID)
}

// PrimeAccountCache seeds the in-process `(tenant_id, kchat_user_id)
// → stalwart_account_id` cache.
//
// Two legitimate use cases:
//
//   - **Cold-start warm-up.** Operators that already know the
//     account-ID set for an incoming traffic burst can pre-load
//     the cache from a sidecar process so the first request
//     doesn't pay a Postgres round-trip. The TTL still applies;
//     entries seeded here are evicted on the same `cacheTTL`
//     schedule as cache-miss writes.
//
//   - **Integration tests.** The proxy's account-resolution path
//     normally falls back to Postgres on miss; tests that run
//     against an httptest Stalwart but lack a real Postgres
//     fixture seed the cache directly so the `(tenant, user) →
//     account_id` resolution returns deterministic values.
//
// `tenantID` and `kchatUserID` must both be non-empty; calls
// with empty strings are silent no-ops so a buggy seeder cannot
// accidentally cache `("", "") → "acc-1"` and shadow legitimate
// lookups.
func (p *Proxy) PrimeAccountCache(tenantID, kchatUserID, accountID string) {
	if tenantID == "" || kchatUserID == "" || accountID == "" {
		return
	}
	p.cache.set(tenantID, kchatUserID, accountID)
}

// ResolveShardURLs returns the ordered list of Stalwart shard URLs
// for the given tenant (primary first, then secondaries) when a
// `ShardResolver` is wired; nil otherwise. Callers should treat
// the slice as read-only and dispatch the request against the
// head, failing over to subsequent entries on transport errors
// or 5xx responses. Single-shard deployments (no `ShardResolver`)
// fall back to `Target()`.
func (p *Proxy) ResolveShardURLs(ctx context.Context, tenantID string) []string {
	return p.resolveShardURLs(ctx, tenantID)
}

// ShardsAvailable reports whether at least one candidate Stalwart
// shard for the tenant is currently serving — i.e. its circuit
// breaker is not open. It keys the breaker on the same `u.Host`
// the failover transport uses, so the verdict matches the proxy's
// own routing decision: when every candidate is tripped the proxy
// would surface a 502/503, which is exactly when a read should
// fall back to a last-known-good cache instead. Single-shard
// deployments (no resolver) consult the default Target host.
func (p *Proxy) ShardsAvailable(ctx context.Context, tenantID string) bool {
	urls := p.resolveShardURLs(ctx, tenantID)
	if len(urls) == 0 && p.target != nil {
		urls = []string{p.target.String()}
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		if !p.breaker.Open(ctx, u.Host) {
			return true
		}
	}
	return false
}

// shardCtxKey carries the resolved shard URL list (primary first)
// to the custom transport so retries can switch hosts without
// re-querying Postgres on every attempt.
type shardCtxKey struct{}

func withShardURLs(ctx context.Context, urls []string) context.Context {
	return context.WithValue(ctx, shardCtxKey{}, urls)
}
func shardURLsFrom(ctx context.Context) []string {
	v, _ := ctx.Value(shardCtxKey{}).([]string)
	return v
}

// shardResolveMemoKey carries a per-request memo so a tenant's
// shard URLs are resolved from Postgres at most once per request.
type shardResolveMemoKey struct{}

// shardResolveMemo caches a single resolveShardURLs result for the
// lifetime of one request. It is consulted by both the degradation
// health check (ShardsAvailable) and ServeHTTP, which would
// otherwise each issue the same GetTenantShard query.
type shardResolveMemo struct {
	mu     sync.Mutex
	tenant string
	urls   []string
	done   bool
}

// WithShardResolveMemo installs a per-request shard-resolution memo
// on the context. When graceful degradation is enabled, both the
// health check and the proxy's own routing resolve the tenant's
// shards; without this memo that doubles the per-request Postgres
// load on every degradation-eligible read. Install it once, outside
// the degradation middleware, so the seeded context is visible to
// the health check and to ServeHTTP alike.
func WithShardResolveMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, shardResolveMemoKey{}, &shardResolveMemo{})
}

// NewProxy builds a Proxy pointed at the configured Stalwart URL.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.StalwartURL == "" {
		return nil, errors.New("jmap.NewProxy: StalwartURL is required")
	}
	if cfg.Pool == nil {
		return nil, errors.New("jmap.NewProxy: Pool is required")
	}
	target, err := url.Parse(cfg.StalwartURL)
	if err != nil {
		return nil, fmt.Errorf("jmap.NewProxy: parse StalwartURL: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	ttl := cfg.AccountCacheTTL
	if ttl <= 0 {
		// Single source of truth lives next to the cache itself
		// (see `accountCacheDefaultTTL`). `newAccountCache`
		// applies the same default on ttl<=0, so this is
		// belt-and-braces — but referencing the constant keeps
		// the two defaults in lockstep if someone tunes one.
		ttl = accountCacheDefaultTTL
	}
	breaker := cfg.Breaker
	if breaker == nil {
		// Default: per-pod sliding-window breaker. Threshold /
		// cooldown / window flow from the proxy config; zero
		// fields pick up the breaker's own defaults so older
		// callers that only set `CircuitBreakThreshold` keep
		// working unchanged (Cooldown=0 → legacy count-only
		// behavior).
		breaker = newInProcessCircuitBreaker(inProcessBreakerConfig{
			Threshold: cfg.CircuitBreakThreshold,
			Cooldown:  cfg.CircuitBreakCooldown,
			Window:    cfg.CircuitBreakWindow,
			Logger:    logger,
		})
	}
	p := &Proxy{
		cfg:     cfg,
		logger:  logger,
		cache:   newAccountCache(ttl),
		target:  target,
		breaker: breaker,
	}
	// Migrate the static `ProxyConfig.SendInterceptor` (if any) into
	// the atomic so `loadSendInterceptor` has a single source of
	// truth. Without this, `SetSendInterceptor(nil)` would silently
	// fall back to the cfg-wired interceptor at later requests and
	// the "Passing nil disables interception" contract on
	// SetSendInterceptor would be a lie. In production the cfg field
	// is never set (main.go always wires via SetSendInterceptor) so
	// this is also a no-op there; the migration exists so test code
	// and any future cfg-wiring caller share the same semantics.
	if cfg.SendInterceptor != nil {
		p.sendInterceptor.Store(&sendInterceptorRef{v: cfg.SendInterceptor})
	}
	base := http.DefaultTransport
	if cfg.TLS != nil {
		tlsCfg, err := cfg.TLS.build(logger)
		if err != nil {
			return nil, fmt.Errorf("jmap.NewProxy: build TLS client config: %w", err)
		}
		// ServerName intentionally left empty when the operator did
		// not pin it: Go's transport derives SNI per-connection from
		// each upstream URL, which is the correct behaviour for shard
		// failover where the secondary's certificate may not carry
		// the primary's hostname.
		//
		// If both Shards AND a pinned ServerName are wired, the SNI
		// is frozen to that single name on every retry, which means
		// every shard's server certificate MUST also carry that
		// name as a SAN or the failover handshake will fail with
		// `certificate is valid for X, not Y`. Loudly warn the
		// operator at startup so they either widen the SAN list on
		// every shard's server cert or remove the pin from
		// `mtls.serverName` in the Helm values.
		if cfg.Shards != nil && strings.TrimSpace(cfg.TLS.ServerName) != "" {
			logger.Printf("jmap proxy: WARNING shard failover is wired but a pinned TLS ServerName=%q is set; every shard's server certificate MUST list %q as a SAN or failover handshakes will fail. Leave mtls.serverName empty to let the transport derive SNI per-connection from each shard URL.", cfg.TLS.ServerName, cfg.TLS.ServerName)
		}
		// Defensive warning: mTLS is wired but the configured
		// StalwartURL is plain HTTP or uses the bare `.svc` short
		// hostname (which is NOT in the SAN list cert-manager
		// generates in `stalwart-mtls.yaml` — those SANs are the
		// FQDN `.svc.cluster.local` form). Either case will lead
		// to a TLS handshake failure on the first request; flagging
		// at startup lets the operator catch the mismatch before
		// traffic starts flowing instead of after.
		if target.Scheme != "https" {
			logger.Printf("jmap proxy: WARNING mTLS is configured (KMAIL_STALWART_TLS_CERT set) but StalwartURL scheme is %q \u2014 mutual-TLS only fires on https URLs. Set KMAIL_STALWART_URL to an https://...:8443 endpoint or disable mTLS to silence this warning.", target.Scheme)
		} else if isBareSvcHostname(target.Hostname()) {
			logger.Printf("jmap proxy: WARNING mTLS is enabled but StalwartURL hostname %q uses the bare `.svc` short form, which is NOT in the SAN list of the server certificate (the Helm chart generates `.svc.cluster.local` SANs). Switch KMAIL_STALWART_URL to the `.svc.cluster.local` FQDN form or override mtls.serverName to match.", target.Hostname())
		}
		base = newClientTLSTransport(tlsCfg)
	} else if target.Scheme == "https" {
		logger.Printf("jmap proxy: WARNING StalwartURL=%s is HTTPS but no client TLS configured \u2014 falling back to default transport (no mutual auth)", cfg.StalwartURL)
	}
	p.base = base
	p.rp = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		ErrorHandler: p.errorHandler,
		Transport:    &shardFailoverTransport{proxy: p, base: base},
	}
	return p, nil
}

// shardFailoverTransport is the custom RoundTripper that retries a
// request against secondary shards when the primary returns a 5xx
// or fails at the transport layer. The list of candidate URLs is
// stamped onto the request context by ServeHTTP so the transport
// does not re-query Postgres per attempt.
type shardFailoverTransport struct {
	proxy *Proxy
	base  http.RoundTripper
}

func (t *shardFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	urls := shardURLsFrom(req.Context())
	if len(urls) == 0 {
		// No per-tenant shard wiring — a single-target deployment or
		// a tenant without a shard assignment (e.g. mid-provisioning
		// or dev). Forward to the default target as the unmodified
		// proxy would, but still drive that target's breaker: it's
		// the same host ShardsAvailable consults, so without this the
		// breaker would never trip on the non-shard path and graceful
		// degradation could never engage for these tenants.
		if t.proxy.target == nil {
			return t.base.RoundTrip(req)
		}
		host := t.proxy.target.Host
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			t.proxy.breaker.RecordFailure(req.Context(), host)
			return nil, err
		}
		if resp.StatusCode >= 500 {
			t.proxy.breaker.RecordFailure(req.Context(), host)
			return resp, nil
		}
		t.proxy.breaker.RecordSuccess(req.Context(), host)
		return resp, nil
	}
	// Threshold comes from the breaker implementation, not the
	// transport. The transport only consults `Open` per retry,
	// `RecordFailure` on 5xx / transport errors, and
	// `RecordSuccess` on a 2xx — the breaker owns its own
	// trip / cooldown / window policy.
	// Buffer the request body once so each retry can rewind. JMAP
	// payloads are small JSON envelopes, so the in-memory cost is
	// bounded; large attachment uploads go through a separate
	// upload endpoint, not this proxy. `req.GetBody` is preferred
	// when callers set it (e.g. net/http internal redirects), but
	// the BFF does not, so we fall back to draining the body.
	var bodyBuf []byte
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody != nil {
			// GetBody returns a fresh reader each call; cheaper
			// than buffering. Probe once to confirm it works.
			if rc, err := req.GetBody(); err == nil {
				rc.Close()
			} else {
				req.GetBody = nil
			}
		}
		if req.GetBody == nil {
			b, err := io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("jmap proxy: buffer body: %w", err)
			}
			bodyBuf = b
		}
	}
	var lastErr error
	for i, candidate := range urls {
		u, err := url.Parse(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		// Skip hosts that have tripped the breaker. The breaker
		// auto-resets when a healthy probe rolls through (see the
		// shard HealthWorker). Only divert when there's a fallback
		// shard available — with no candidates left, attempting
		// the tripped host is still preferable to a guaranteed 502.
		if t.proxy.breaker.Open(req.Context(), u.Host) && i+1 < len(urls) {
			continue
		}
		clone := req.Clone(req.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		clone.Host = u.Host
		// Re-attach a fresh body for each attempt so the previous
		// retry's consumed reader doesn't leak into the next.
		if req.GetBody != nil {
			rc, err := req.GetBody()
			if err != nil {
				lastErr = fmt.Errorf("jmap proxy: rewind body: %w", err)
				continue
			}
			clone.Body = rc
		} else if bodyBuf != nil {
			clone.Body = io.NopCloser(bytes.NewReader(bodyBuf))
			clone.ContentLength = int64(len(bodyBuf))
		}
		resp, err := t.base.RoundTrip(clone)
		if err != nil {
			t.proxy.breaker.RecordFailure(req.Context(), u.Host)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// Always count a 5xx against the breaker, even on the
			// last candidate. The previous code only incremented
			// when a fallback existed (`i+1 < len(urls)`), so the
			// last shard could fail forever without ever tripping
			// its breaker.
			t.proxy.breaker.RecordFailure(req.Context(), u.Host)
			if i+1 < len(urls) {
				resp.Body.Close()
				lastErr = fmt.Errorf("upstream %s returned %d", u.Host, resp.StatusCode)
				continue
			}
			// No more candidates; surface the last shard's 5xx to
			// the client without resetting its breaker.
			return resp, nil
		}
		t.proxy.breaker.RecordSuccess(req.Context(), u.Host)
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("jmap proxy: no candidate shards available")
	}
	return nil, lastErr
}

// ServeHTTP implements http.Handler. It expects to run behind the
// OIDC middleware: the acting tenant and KChat user are read from
// the request context. Missing context values result in 500 because
// the caller wired the mux incorrectly — 401 would hide the bug.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		http.Error(w, "jmap proxy: missing tenant or user context (OIDC middleware not wired)", http.StatusInternalServerError)
		return
	}

	accountID, err := p.resolveAccount(r.Context(), tenantID, kchatUserID)
	if err != nil {
		w.Header().Set("Content-Type", "application/problem+json")
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// An unresolved account is expected while the Tenant Service
			// has not yet provisioned the user; surface it as 404 with a
			// JMAP-compatible error shape.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:accountNotFound","title":"stalwart account not provisioned"}` + "\n"))
		default:
			// Infrastructure failures (Postgres outage, pool exhaustion,
			// GUC errors, context cancellation, etc.) surface as 502 so
			// on-call doesn't chase a spurious "not provisioned" signal.
			p.logger.Printf("jmap proxy resolveAccount err tenant=%s kchat_user=%s err=%v", tenantID, kchatUserID, err)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:serverUnavailable","title":"account lookup failed"}` + "\n"))
		}
		return
	}

	ctx := middleware.WithStalwartAccountID(r.Context(), accountID)
	if urls := p.resolveShardURLs(ctx, tenantID); len(urls) > 0 {
		ctx = withShardURLs(ctx, urls)
	}
	// Pre-delivery scan hook (Phase 8). JMAP (RFC 8620) uses POST
	// for both reads (`Email/get`, `Mailbox/get`, `Email/query`,
	// `Thread/get`) and writes; scanning every POST would put a
	// ClamAV TCP round-trip in front of read-heavy traffic. We
	// therefore only invoke the hook on the two paths where actual
	// message content flows:
	//
	//   • The blob upload path (typically `/jmap/upload/...`),
	//     which is how MIME bodies and attachments enter Stalwart.
	//   • The JMAP request endpoint when the body advertises an
	//     `Email/set` or `EmailSubmission/set` invocation, which
	//     are the only methods that submit (or stage for
	//     submission) a message.
	sendInterceptor := p.loadSendInterceptor()
	if (p.cfg.PreDeliverHook != nil || sendInterceptor != nil) && (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.Body != nil && requestCarriesMessageContent(r) {
		// Buffer up to 33 MiB (32 MiB cap + 1 byte sentinel) into
		// the scan buffer. We deliberately do NOT stream-pipe to
		// ClamAV in parallel with the upstream: ClamAV INSTREAM can
		// return a FOUND verdict mid-body, but anything already
		// forwarded to Stalwart would have leaked past the
		// quarantine. The double-buffer that previously existed in
		// the shard round tripper is eliminated by setting
		// `r.GetBody` after the scan: the round tripper detects
		// GetBody and reuses the same scan buffer for every retry
		// instead of draining `r.Body` into a fresh allocation.
		const maxScanBytes = 32 * 1024 * 1024
		body, err := io.ReadAll(io.LimitReader(r.Body, maxScanBytes+1))
		_ = r.Body.Close()
		if err != nil {
			p.logger.Printf("jmap proxy: read body for malware scan: %v", err)
			http.Error(w, `{"type":"urn:ietf:params:jmap:error:serverFail","title":"read body"}`, http.StatusBadGateway)
			return
		}
		if len(body) > maxScanBytes {
			p.logger.Printf("jmap proxy: pre-deliver scan rejected tenant=%s reason=body-too-large bytes>%d", tenantID, maxScanBytes)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:tooLarge","title":"message exceeds malware-scan size limit"}` + "\n"))
			return
		}
		if shouldScanBody(r, body) {
			if p.cfg.PreDeliverHook != nil {
				if err := p.cfg.PreDeliverHook(ctx, body); err != nil {
					p.logger.Printf("jmap proxy: pre-deliver hook rejected tenant=%s err=%v", tenantID, err)
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:rejectedByPolicy","title":"message rejected by malware scanner"}` + "\n"))
					return
				}
			}
			if sendInterceptor != nil {
				intercepted, err := sendInterceptor.Intercept(ctx, w, r.WithContext(ctx), body)
				// `intercepted` is the source of truth for whether the
				// hook (or its writeJMAPResponse helper) has already
				// committed bytes to the ResponseWriter. We MUST honor
				// it regardless of `err` — falling through after the
				// hook wrote a response would have `p.rp.ServeHTTP`
				// attempt a second WriteHeader and either panic or
				// corrupt the connection. `err` is purely diagnostic
				// past this point.
				if err != nil {
					p.logger.Printf("jmap proxy: send interceptor error tenant=%s err=%v intercepted=%v", tenantID, err, intercepted)
				}
				if intercepted {
					return
				}
				// err != nil AND !intercepted means the hook decided
				// not to handle the request but produced a diagnostic.
				// Fall through to Stalwart so a transient Valkey /
				// Postgres blip can't break the send path entirely.
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		// Hand the same byte slice to the round tripper so it can
		// retry across shards without re-buffering. Each call to
		// GetBody returns a fresh reader over the SAME backing
		// array (`bytes.NewReader` is a zero-copy wrapper), which
		// is what eliminates the second allocation that the
		// failover path used to make.
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// requestCarriesMessageContent is the cheap path-only filter for
// the malware pre-delivery hook. Returning false short-circuits
// the body buffering for read-only JMAP traffic.
func requestCarriesMessageContent(r *http.Request) bool {
	p := r.URL.Path
	// Blob-upload paths always carry MIME / attachment bytes.
	if strings.Contains(p, "/jmap/upload") || strings.HasSuffix(p, "/upload") {
		return true
	}
	// The JMAP request endpoint itself can carry an Email/set or
	// EmailSubmission/set invocation; we still buffer there but
	// `shouldScanBody` decides whether to actually invoke the
	// scanner based on the JSON-RPC method names. Match either
	// `/jmap` (or `/jmap/`) at the end of the path, but require
	// it to be a path component — `/.well-known/jmap` is a
	// discovery doc, not a method call.
	if strings.HasSuffix(p, "/jmap") || strings.HasSuffix(p, "/jmap/") {
		return !strings.Contains(p, "/.well-known/")
	}
	return false
}

// jmapSubmitMethods is the subset of JMAP method names whose
// invocations stage or submit user-supplied message content.
// Everything else is a read or metadata mutation we can safely
// skip.
var jmapSubmitMethods = []string{
	`"Email/set"`,
	`"EmailSubmission/set"`,
	`"EmailSubmission/create"`,
}

// shouldScanBody decides whether the buffered body should be
// passed to the malware scanner. Upload paths always scan; the
// JMAP request endpoint only scans when its body references one
// of `jmapSubmitMethods`. The check is a cheap byte-level scan
// so we avoid a full JSON parse on the hot path.
func shouldScanBody(r *http.Request, body []byte) bool {
	p := r.URL.Path
	if strings.Contains(p, "/jmap/upload") || strings.HasSuffix(p, "/upload") {
		return true
	}
	for _, m := range jmapSubmitMethods {
		if bytes.Contains(body, []byte(m)) {
			return true
		}
	}
	return false
}

// resolveShardURLs returns the ordered candidate Stalwart URLs for
// the tenant: primary first, then `shard_failover_config` backups.
// Falls back to an empty list when no shard service is wired or the
// tenant has no assignment, which the transport interprets as
// "no failover available".
func (p *Proxy) resolveShardURLs(ctx context.Context, tenantID string) []string {
	if p.cfg.Shards == nil || tenantID == "" {
		return nil
	}
	// Serve from the per-request memo when one is installed, so a
	// degradation-eligible read resolves shards from Postgres once
	// (health check) instead of twice (health check + ServeHTTP).
	memo, _ := ctx.Value(shardResolveMemoKey{}).(*shardResolveMemo)
	if memo != nil {
		memo.mu.Lock()
		if memo.done && memo.tenant == tenantID {
			urls := memo.urls
			memo.mu.Unlock()
			return urls
		}
		memo.mu.Unlock()
	}
	urls := p.resolveShardURLsUncached(ctx, tenantID)
	if memo != nil {
		memo.mu.Lock()
		// First resolver for this tenant wins; a later one reuses it
		// so the memo never flaps within a request.
		if !memo.done || memo.tenant != tenantID {
			memo.tenant = tenantID
			memo.urls = urls
			memo.done = true
		} else {
			urls = memo.urls
		}
		memo.mu.Unlock()
	}
	return urls
}

func (p *Proxy) resolveShardURLsUncached(ctx context.Context, tenantID string) []string {
	primary, err := p.cfg.Shards.GetTenantShard(ctx, tenantID)
	if err != nil || primary == "" {
		return nil
	}
	urls := []string{primary}
	secondaries, err := p.cfg.Shards.GetSecondaryShards(ctx, tenantID)
	if err == nil {
		urls = append(urls, secondaries...)
	}
	return urls
}

// rewrite adapts the incoming request to the upstream Stalwart URL.
// It forwards the request path unchanged — Stalwart serves JMAP at
// `/jmap` and `/jmap/session`, which are exactly the paths the BFF
// exposes — and injects the resolved Stalwart account ID as a header
// the upstream can trust in internal-network deployments.
func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	accountID := middleware.StalwartAccountIDFrom(r.In.Context())
	tenantID := middleware.TenantIDFrom(r.In.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.In.Context())

	r.SetURL(p.target)
	r.Out.Host = p.target.Host
	// Path is forwarded as-is: `SetURL` joins the (empty) target
	// path with the inbound path, so `/jmap/session` stays
	// `/jmap/session` and POST `/jmap` stays `/jmap`. Stalwart
	// serves JMAP under `/jmap` (root `/` is the admin UI), so the
	// prefix must be preserved.

	r.Out.Header.Set("X-KMail-Tenant-Id", tenantID)
	r.Out.Header.Set("X-KMail-Kchat-User-Id", kchatUserID)
	if accountID != "" {
		r.Out.Header.Set("X-KMail-Stalwart-Account-Id", accountID)
	}
	// Production: the BFF authenticates itself to Stalwart via the
	// mutual-TLS client certificate presented by the transport (see
	// `ClientTLSConfig`). Stalwart pins the issuing CA and refuses
	// any connection that does not chain to it, so the X-KMail-*
	// identity headers are only honoured for callers the transport
	// already vouched for cryptographically. The inbound
	// `Authorization` header (the user's OIDC bearer) is stripped
	// because Stalwart neither needs it nor trusts it — the BFF is
	// the authentication boundary, the mTLS handshake is the
	// BFF→Stalwart trust boundary.
	//
	// Dev/CI only: `DevStalwartAuthHeader` is set (see ProxyConfig)
	// because the official Stalwart image does not implement the
	// X-KMail-* header-trust feature and would 401 the plain-HTTP
	// BFF. We replace `Authorization` with the recovery-admin Basic
	// credential so the smoke tests exercise a real mailbox. This
	// branch is unreachable in production (the env gate in main.go
	// leaves the field empty).
	if p.cfg.DevStalwartAuthHeader != "" {
		r.Out.Header.Set("Authorization", p.cfg.DevStalwartAuthHeader)
	} else {
		r.Out.Header.Del("Authorization")
	}
}

// errorHandler maps upstream failures into BFF-visible errors per
// `docs/JMAP-CONTRACT.md` §7.
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	p.logger.Printf("jmap proxy upstream error path=%s err=%v", r.URL.Path, err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:serverUnavailable","title":"upstream unavailable"}` + "\n"))
}

// resolveAccount returns the Stalwart account ID for the given
// (tenant_id, kchat_user_id) pair, preferring the in-process cache.
// Cache misses go to Postgres and populate the cache.
func (p *Proxy) resolveAccount(ctx context.Context, tenantID, kchatUserID string) (string, error) {
	if accountID, ok := p.cache.get(tenantID, kchatUserID); ok {
		return accountID, nil
	}

	var accountID string
	err := pgx.BeginFunc(ctx, p.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return fmt.Errorf("set tenant GUC: %w", err)
		}
		row := tx.QueryRow(ctx, `
			SELECT stalwart_account_id
			FROM users
			WHERE tenant_id = $1::uuid AND kchat_user_id = $2
		`, tenantID, kchatUserID)
		return row.Scan(&accountID)
	})
	if err != nil {
		return "", err
	}
	p.cache.set(tenantID, kchatUserID, accountID)
	return accountID, nil
}

// accountCacheMaxEntries bounds the in-process
// `(tenant_id, kchat_user_id) → stalwart_account_id` cache. 50,000
// entries at ~150 B per entry keeps the cache under ~8 MiB even on
// a fully-warm shard while still comfortably exceeding the
// typical concurrent-active-user count per pod (which is bound by
// the BFF's outbound connection budget to Stalwart). Pair with a
// 5-minute TTL so the Valkey-backed shared cache (Phase 3) sees
// every key turn over within a session.
const (
	accountCacheMaxEntries = 50_000
	accountCacheDefaultTTL = 5 * time.Minute
)

// accountCache is the bounded, TTL'd in-process cache for the
// `(tenant_id, kchat_user_id) → stalwart_account_id` mapping. The
// hashicorp/golang-lru `expirable` LRU gives us bounded size + TTL
// in one structure and is internally locked, so the public
// `get` / `set` methods stay as thin pass-throughs that the proxy
// already uses everywhere.
type accountCache struct {
	inner *lru.LRU[string, string]
}

func newAccountCache(ttl time.Duration) *accountCache {
	if ttl <= 0 {
		ttl = accountCacheDefaultTTL
	}
	return &accountCache{
		inner: lru.NewLRU[string, string](accountCacheMaxEntries, nil, ttl),
	}
}

func (c *accountCache) key(tenantID, kchatUserID string) string {
	return tenantID + "|" + kchatUserID
}

func (c *accountCache) get(tenantID, kchatUserID string) (string, bool) {
	return c.inner.Get(c.key(tenantID, kchatUserID))
}

func (c *accountCache) set(tenantID, kchatUserID, accountID string) {
	c.inner.Add(c.key(tenantID, kchatUserID), accountID)
}
