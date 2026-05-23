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
	"time"

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
	CircuitBreakThreshold int

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

// build assembles the *tls.Config that the BFF transport presents
// to Stalwart. The initial keypair and CA bundle loads run at
// startup so a misconfigured deployment fails fast on boot;
// subsequent rotations are picked up on the next handshake via
// the `GetClientCertificate` and `VerifyConnection` callbacks
// (see keypairLoader / caPoolLoader). This means cert-manager
// rotation — of either the BFF leaf certificate OR the trust
// root — continues to work even in clusters where the Reloader
// controller is not installed.
func (c *ClientTLSConfig) build(logger *log.Logger) (*tls.Config, error) {
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
	cfg := &tls.Config{
		GetClientCertificate: kpLoader.get,
		MinVersion:           min,
		ServerName:           c.ServerName,
	}
	if strings.TrimSpace(c.CAFile) != "" {
		caLoader := &caPoolLoader{
			caFile: c.CAFile,
			logger: logger,
		}
		// Validate the trust root at startup for the same fail-fast
		// reason as the keypair. The pool is cached and re-loaded on
		// the next handshake whenever the file's mtime changes.
		if _, err := caLoader.load(); err != nil {
			return nil, fmt.Errorf("jmap.ClientTLSConfig: load CA bundle: %w", err)
		}
		// `InsecureSkipVerify=true` *combined with* `VerifyConnection`
		// is the documented Go stdlib pattern for swapping in a
		// custom verifier — it disables the built-in cert chain
		// check so we can do it ourselves against a freshly-loaded
		// pool. The verification logic below is otherwise identical
		// to the default behaviour: it pins the chain to our CA
		// roots and enforces the SNI hostname matches a SAN.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("jmap.ClientTLSConfig: peer presented no certificate")
			}
			pool, err := caLoader.load()
			if err != nil {
				return fmt.Errorf("jmap.ClientTLSConfig: load CA bundle: %w", err)
			}
			opts := x509.VerifyOptions{
				Roots:         pool,
				Intermediates: x509.NewCertPool(),
				DNSName:       state.ServerName,
			}
			for _, ic := range state.PeerCertificates[1:] {
				opts.Intermediates.AddCert(ic)
			}
			if _, err := state.PeerCertificates[0].Verify(opts); err != nil {
				return fmt.Errorf("jmap.ClientTLSConfig: verify peer: %w", err)
			}
			return nil
		}
	}
	return cfg, nil
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

	mu        sync.RWMutex
	cert      *tls.Certificate
	certMTime time.Time
	keyMTime  time.Time
}

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
		if len(loaded.Certificate) > 0 {
			if leaf, perr := x509.ParseCertificate(loaded.Certificate[0]); perr == nil {
				leafNotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
				leafSubject = leaf.Subject.CommonName
				if leafSubject == "" && len(leaf.DNSNames) > 0 {
					leafSubject = leaf.DNSNames[0]
				}
			}
		}
		if prev == nil {
			l.logger.Printf("jmap proxy: loaded client TLS keypair subject=%q notAfter=%s", leafSubject, leafNotAfter)
		} else {
			l.logger.Printf("jmap proxy: rotated client TLS keypair subject=%q notAfter=%s", leafSubject, leafNotAfter)
		}
	}
	return l.cert, nil
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

// newClientTLSTransport returns an *http.Transport configured for
// mTLS to Stalwart. The dialer and timeout values match the
// stdlib defaults (`http.DefaultTransport`) so retry / dial
// behaviour is unchanged when the only addition is a client cert.
func newClientTLSTransport(tlsCfg *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsCfg,
	}
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
	stripPR string

	// breakerMu guards the circuit-breaker counters keyed by
	// shard host (URL.Host). Counters live in-process for Phase 4 —
	// a Valkey-backed shared breaker is a Phase 5 follow-up.
	breakerMu sync.Mutex
	breakers  map[string]int
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
		ttl = 5 * time.Minute
	}
	p := &Proxy{
		cfg:      cfg,
		logger:   logger,
		cache:    newAccountCache(ttl),
		target:   target,
		stripPR:  "/jmap",
		breakers: map[string]int{},
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
		base = newClientTLSTransport(tlsCfg)
	} else if target.Scheme == "https" {
		logger.Printf("jmap proxy: WARNING StalwartURL=%s is HTTPS but no client TLS configured \u2014 falling back to default transport (no mutual auth)", cfg.StalwartURL)
	}
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
		// No shard wiring; behave as the unmodified proxy.
		return t.base.RoundTrip(req)
	}
	threshold := t.proxy.cfg.CircuitBreakThreshold
	if threshold <= 0 {
		threshold = 3
	}
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
		// shard HealthWorker).
		if t.proxy.breakerOpen(u.Host, threshold) && i+1 < len(urls) {
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
			t.proxy.breakerInc(u.Host)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// Always count a 5xx against the breaker, even on the
			// last candidate. The previous code only incremented
			// when a fallback existed (`i+1 < len(urls)`), so the
			// last shard could fail forever without ever tripping
			// its breaker.
			t.proxy.breakerInc(u.Host)
			if i+1 < len(urls) {
				resp.Body.Close()
				lastErr = fmt.Errorf("upstream %s returned %d", u.Host, resp.StatusCode)
				continue
			}
			// No more candidates; surface the last shard's 5xx to
			// the client without resetting its breaker.
			return resp, nil
		}
		t.proxy.breakerReset(u.Host)
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("jmap proxy: no candidate shards available")
	}
	return nil, lastErr
}

func (p *Proxy) breakerOpen(host string, threshold int) bool {
	p.breakerMu.Lock()
	defer p.breakerMu.Unlock()
	return p.breakers[host] >= threshold
}

func (p *Proxy) breakerInc(host string) {
	p.breakerMu.Lock()
	p.breakers[host]++
	p.breakerMu.Unlock()
}

func (p *Proxy) breakerReset(host string) {
	p.breakerMu.Lock()
	delete(p.breakers, host)
	p.breakerMu.Unlock()
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
	if p.cfg.PreDeliverHook != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) && r.Body != nil && requestCarriesMessageContent(r) {
		const maxScanBytes = 32 * 1024 * 1024
		body, err := io.ReadAll(io.LimitReader(r.Body, maxScanBytes))
		_ = r.Body.Close()
		if err != nil {
			p.logger.Printf("jmap proxy: read body for malware scan: %v", err)
			http.Error(w, `{"type":"urn:ietf:params:jmap:error:serverFail","title":"read body"}`, http.StatusBadGateway)
			return
		}
		if shouldScanBody(r, body) {
			if err := p.cfg.PreDeliverHook(ctx, body); err != nil {
				p.logger.Printf("jmap proxy: pre-deliver hook rejected tenant=%s err=%v", tenantID, err)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:rejectedByPolicy","title":"message rejected by malware scanner"}` + "\n"))
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
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
// It strips the `/jmap` prefix so clients can hit `/jmap/session`
// and Stalwart sees `/session`, and injects the resolved Stalwart
// account ID as a header the upstream can trust in internal-network
// deployments.
func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	accountID := middleware.StalwartAccountIDFrom(r.In.Context())
	tenantID := middleware.TenantIDFrom(r.In.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.In.Context())

	r.SetURL(p.target)
	r.Out.Host = p.target.Host

	// Strip the `/jmap` prefix from the outgoing path so the
	// upstream sees the JMAP path it actually implements. Leave
	// trailing `/` and deeper paths intact. Clear RawPath so
	// net/url regenerates it from the rewritten Path — otherwise a
	// non-empty RawPath (set whenever the incoming URL contained
	// percent-encoded bytes) would win inside RequestURI() and the
	// upstream would still see `/jmap`.
	outPath := r.Out.URL.Path
	if strings.HasPrefix(outPath, p.stripPR) {
		trimmed := strings.TrimPrefix(outPath, p.stripPR)
		if trimmed == "" {
			trimmed = "/"
		}
		r.Out.URL.Path = trimmed
		r.Out.URL.RawPath = ""
	}

	r.Out.Header.Set("X-KMail-Tenant-Id", tenantID)
	r.Out.Header.Set("X-KMail-Kchat-User-Id", kchatUserID)
	if accountID != "" {
		r.Out.Header.Set("X-KMail-Stalwart-Account-Id", accountID)
	}
	// The BFF authenticates itself to Stalwart via the mutual-TLS
	// client certificate presented by the transport (see
	// `ClientTLSConfig`). Stalwart pins the issuing CA and refuses
	// any connection that does not chain to it, so the
	// X-KMail-* identity headers are only honoured for callers
	// the transport already vouched for cryptographically. The
	// inbound `Authorization` header (the user's OIDC bearer) is
	// stripped because Stalwart neither needs it nor trusts it —
	// the BFF is the authentication boundary, the mTLS handshake
	// is the BFF→Stalwart trust boundary.
	r.Out.Header.Del("Authorization")
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

// accountCache is a TTL'd in-process cache for the
// `(tenant_id, kchat_user_id) → stalwart_account_id` mapping. It is
// deliberately simple; the Valkey-backed shared cache (10 000 entries,
// 5 min TTL) documented in `docs/JMAP-CONTRACT.md` §3.3 lands in
// Phase 2.
type accountCache struct {
	ttl time.Duration
	mu  sync.RWMutex
	m   map[string]accountCacheEntry
}

type accountCacheEntry struct {
	accountID string
	expiresAt time.Time
}

func newAccountCache(ttl time.Duration) *accountCache {
	return &accountCache{ttl: ttl, m: map[string]accountCacheEntry{}}
}

func (c *accountCache) key(tenantID, kchatUserID string) string {
	return tenantID + "|" + kchatUserID
}

func (c *accountCache) get(tenantID, kchatUserID string) (string, bool) {
	k := c.key(tenantID, kchatUserID)
	c.mu.RLock()
	entry, ok := c.m[k]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		// Drop the expired entry eagerly so callers that only hit
		// stale keys don't accumulate map entries forever.
		c.mu.Lock()
		if cur, still := c.m[k]; still && !time.Now().Before(cur.expiresAt) {
			delete(c.m, k)
		}
		c.mu.Unlock()
		return "", false
	}
	return entry.accountID, true
}

func (c *accountCache) set(tenantID, kchatUserID, accountID string) {
	c.mu.Lock()
	c.m[c.key(tenantID, kchatUserID)] = accountCacheEntry{
		accountID: accountID,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}
