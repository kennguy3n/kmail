package middleware

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// jwks is a JSON Web Key Set document as returned by a standard
// OIDC `jwks_uri` endpoint (RFC 7517).
type jwks struct {
	Keys []jwk `json:"keys"`
}

// jwk is a single JSON Web Key. Only the fields KMail needs for
// signature verification are decoded; unknown fields pass through.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`

	// RSA
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`

	// EC
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// JWKSFetcher discovers the OIDC issuer's JWKS, caches the parsed
// keyset in-process, and refreshes it on a configurable interval.
// It is safe for concurrent use.
type JWKSFetcher struct {
	issuer    string
	http      *http.Client
	refresh   time.Duration
	now       func() time.Time
	fetchURL  func(ctx context.Context) (string, error)
	discovery bool

	mu        sync.RWMutex
	fetchedAt time.Time
	keys      map[string]any // kid → *rsa.PublicKey / *ecdsa.PublicKey
}

// JWKSConfig wires a JWKSFetcher.
type JWKSConfig struct {
	// Issuer is the OIDC issuer URL. If non-empty, the fetcher
	// discovers jwks_uri from `{Issuer}/.well-known/openid-configuration`.
	Issuer string
	// JWKSURL overrides discovery. Useful for tests or issuers
	// that do not advertise OpenID discovery.
	JWKSURL string
	// Refresh bounds how stale the cached keyset is allowed to
	// get. Defaults to 1h.
	Refresh time.Duration
	// HTTPClient overrides the default *http.Client used for
	// fetching JWKS + discovery documents. Defaults to an
	// `http.Client` with a 10s timeout.
	HTTPClient *http.Client
	// Now overrides time.Now for tests.
	Now func() time.Time
}

// NewJWKSFetcher constructs a JWKSFetcher. Either Issuer or
// JWKSURL must be set.
func NewJWKSFetcher(cfg JWKSConfig) (*JWKSFetcher, error) {
	if cfg.Issuer == "" && cfg.JWKSURL == "" {
		return nil, errors.New("JWKSFetcher: Issuer or JWKSURL is required")
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = time.Hour
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	f := &JWKSFetcher{
		issuer:    cfg.Issuer,
		http:      cfg.HTTPClient,
		refresh:   cfg.Refresh,
		now:       cfg.Now,
		keys:      map[string]any{},
		discovery: cfg.JWKSURL == "",
	}
	if cfg.JWKSURL != "" {
		jwksURL := cfg.JWKSURL
		f.fetchURL = func(_ context.Context) (string, error) { return jwksURL, nil }
	} else {
		f.fetchURL = f.discoverJWKSURL
	}
	return f, nil
}

// KeyFunc returns the signing key registered under the given `kid`,
// refreshing the cache if it is stale or if the kid is unknown.
// Intended to be used as the `Keyfunc` argument to
// `jwt.Parse` / `jwt.ParseWithClaims`.
func (f *JWKSFetcher) KeyFunc(ctx context.Context, kid string) (any, error) {
	if key, ok := f.lookup(kid); ok && !f.isStale() {
		return key, nil
	}
	if err := f.refreshLocked(ctx); err != nil {
		// If refresh failed but we still have a cached key, fall
		// back to it — preserves availability during transient
		// issuer outages.
		if key, ok := f.lookup(kid); ok {
			return key, nil
		}
		return nil, fmt.Errorf("refresh JWKS: %w", err)
	}
	if key, ok := f.lookup(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("JWKS: unknown kid %q", kid)
}

func (f *JWKSFetcher) lookup(kid string) (any, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	key, ok := f.keys[kid]
	return key, ok
}

func (f *JWKSFetcher) isStale() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.fetchedAt.IsZero() || f.now().Sub(f.fetchedAt) > f.refresh
}

// Refresh forces an immediate refetch of the JWKS. Exposed for
// tests; production callers rely on the automatic refresh driven
// by KeyFunc.
func (f *JWKSFetcher) Refresh(ctx context.Context) error {
	return f.refreshLocked(ctx)
}

func (f *JWKSFetcher) refreshLocked(ctx context.Context) error {
	url, err := f.fetchURL(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS: HTTP %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var keyset jwks
	if err := json.Unmarshal(body, &keyset); err != nil {
		return fmt.Errorf("JWKS: decode body: %w", err)
	}
	parsed := map[string]any{}
	for _, k := range keyset.Keys {
		// Ignore encryption-only keys; we only consume `sig` keys.
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		key, err := parseJWK(k)
		if err != nil {
			// Skip unparseable key — the issuer may publish more
			// types than we support.
			continue
		}
		if k.Kid == "" {
			// An issuer without `kid` is legal but cannot drive a
			// lookup; we index such keys under the empty string so
			// a single-key issuer still works when the token
			// header omits kid.
			parsed[""] = key
			continue
		}
		parsed[k.Kid] = key
	}
	f.mu.Lock()
	f.keys = parsed
	f.fetchedAt = f.now()
	f.mu.Unlock()
	return nil
}

// discoveryDoc is the subset of the OIDC discovery document KMail
// consults. See OpenID Connect Discovery 1.0 §4.
type discoveryDoc struct {
	JWKSURI string `json:"jwks_uri"`
}

func (f *JWKSFetcher) discoverJWKSURL(ctx context.Context) (string, error) {
	url := strings.TrimRight(f.issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery: HTTP %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("OIDC discovery: decode body: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("OIDC discovery: jwks_uri missing")
	}
	return doc.JWKSURI, nil
}

// parseJWK decodes a single JWK into a crypto public key. Only the
// signature key types KMail expects from a KChat OIDC issuer are
// supported: RSA (RS256/RS384/RS512/PS256/PS384/PS512) and EC
// (ES256/ES384/ES512).
func parseJWK(k jwk) (any, error) {
	switch k.Kty {
	case "RSA":
		return parseRSA(k)
	case "EC":
		return parseEC(k)
	default:
		return nil, fmt.Errorf("JWK: unsupported kty %q", k.Kty)
	}
}

func parseRSA(k jwk) (*rsa.PublicKey, error) {
	if k.N == "" || k.E == "" {
		return nil, errors.New("JWK RSA: missing n or e")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("JWK RSA n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("JWK RSA e: %w", err)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: e,
	}, nil
}

func parseEC(k jwk) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("JWK EC: unsupported crv %q", k.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("JWK EC x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("JWK EC y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}, nil
}
