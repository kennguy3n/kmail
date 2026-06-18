// Package stalwartauth mints and serves the OIDC bearer credentials
// the BFF presents to Stalwart on the production data plane.
//
// Trust model. The official `stalwartlabs/stalwart` image does not
// honour the legacy `X-KMail-*` header-trust scheme (it 401s on it);
// the only mechanism it implements for "the BFF acts on behalf of a
// principal" is OpenID Connect bearer validation. Stalwart is
// configured with an OIDC directory whose `issuerUrl` points back at
// this BFF; at directory-open time it fetches
// `{issuer}/.well-known/openid-configuration` and the advertised
// `jwks_uri`, then validates every inbound `Authorization: Bearer`
// JWT locally against those keys. The principal is taken from the
// configured username claim (KMail uses `email`).
//
// So the BFF is, for Stalwart's eyes only, a minimal OpenID Provider:
// it mints a short-lived RS256 JWT per principal (audience `stalwart`)
// and exposes the discovery + JWKS documents Stalwart needs to verify
// them. The signing key never leaves the BFF; the trust boundary is
// the key plus the `aud`/`iss` checks. Because the `email` claim is
// fully trusted by Stalwart, it MUST always be a value the BFF
// derives server-side (the resolved `stalwart_account_id`) and never
// anything the end user can influence.
//
// The interactive auth-code endpoints (`token`, `authorize`) are
// advertised because Stalwart's discovery deserialiser requires them,
// but they are never exercised in this service-to-service,
// directly-minted-token model; the handlers return 501 by design.
package stalwartauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

const (
	// DefaultAudience is the `aud` claim Stalwart's OIDC directory
	// pins via `requireAudience`.
	DefaultAudience = "stalwart"
	// DefaultTokenTTL is how long a minted bearer is valid.
	DefaultTokenTTL = 5 * time.Minute
	// defaultCacheSize bounds the per-principal token cache.
	defaultCacheSize = 50_000
	// clockSkew back-dates `nbf`/`iat` so a minted token survives
	// small clock differences between the BFF and Stalwart.
	clockSkew = 5 * time.Second
)

// Config configures a Signer. Issuer is mandatory; the rest default.
type Config struct {
	// Issuer is the exact OIDC issuer URL. It MUST match the
	// `issuerUrl` configured on Stalwart's OIDC directory and must
	// be reachable by the Stalwart process. Any path component
	// (e.g. `/oidc/stalwart`) is preserved so the discovery / JWKS
	// endpoints can be namespaced away from the BFF's own routes.
	Issuer string
	// Audience is the `aud` claim. Defaults to "stalwart".
	Audience string
	// KeyID overrides the JWK `kid`. Empty derives the RFC 7638
	// thumbprint of the public key (so key rotation yields a new
	// kid automatically).
	KeyID string
	// TokenTTL is the minted-token lifetime. Defaults to 5m.
	TokenTTL time.Duration

	// now is a test seam for the clock. nil uses time.Now.
	now func() time.Time
}

// Signer mints short-lived RS256 bearer tokens for Stalwart and
// serves the discovery + JWKS documents that validate them. It is
// safe for concurrent use.
type Signer struct {
	key        *rsa.PrivateKey
	issuer     string
	issuerPath string
	audience   string
	kid        string
	ttl        time.Duration
	now        func() time.Time

	cache *lru.LRU[string, string]
	jwks  []byte
	disc  []byte
}

// NewSigner builds a Signer around an RSA private key.
func NewSigner(key *rsa.PrivateKey, cfg Config) (*Signer, error) {
	if key == nil {
		return nil, errors.New("stalwartauth: nil signing key")
	}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("stalwartauth: signing key too small (%d bits); need >= 2048", key.N.BitLen())
	}
	issuer := strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if issuer == "" {
		return nil, errors.New("stalwartauth: empty issuer")
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return nil, fmt.Errorf("stalwartauth: invalid issuer %q: %w", issuer, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("stalwartauth: issuer %q must be an absolute URL", issuer)
	}
	aud := strings.TrimSpace(cfg.Audience)
	if aud == "" {
		aud = DefaultAudience
	}
	ttl := cfg.TokenTTL
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	pubJWK := jwkFromPublic(&key.PublicKey)
	kid := strings.TrimSpace(cfg.KeyID)
	if kid == "" {
		kid = thumbprint(&key.PublicKey)
	}
	pubJWK.Kid = kid

	jwksBytes, err := json.Marshal(jwkSet{Keys: []jwk{pubJWK}})
	if err != nil {
		return nil, fmt.Errorf("stalwartauth: marshal jwks: %w", err)
	}
	discBytes, err := json.Marshal(buildDiscovery(issuer))
	if err != nil {
		return nil, fmt.Errorf("stalwartauth: marshal discovery: %w", err)
	}

	// Cache tokens for half their lifetime so a cached token always
	// has at least ttl/2 remaining when handed out, comfortably
	// covering Stalwart's validation plus any clock skew.
	cacheTTL := ttl / 2
	if cacheTTL < time.Second {
		cacheTTL = time.Second
	}

	return &Signer{
		key:        key,
		issuer:     issuer,
		issuerPath: strings.TrimRight(u.Path, "/"),
		audience:   aud,
		kid:        kid,
		ttl:        ttl,
		now:        now,
		cache:      lru.NewLRU[string, string](defaultCacheSize, nil, cacheTTL),
		jwks:       jwksBytes,
		disc:       discBytes,
	}, nil
}

// Mint returns a short-lived bearer token authenticating the BFF as
// the given principal (the resolved Stalwart account email). Repeated
// calls for the same principal within the cache window return the
// same token.
func (s *Signer) Mint(principal string) (string, error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return "", errors.New("stalwartauth: empty principal")
	}
	if tok, ok := s.cache.Get(principal); ok {
		return tok, nil
	}
	now := s.now()
	claims := jwt.MapClaims{
		"iss":                s.issuer,
		"aud":                s.audience,
		"sub":                principal,
		"email":              principal,
		"preferred_username": principal,
		"scope":              "openid email",
		"iat":                now.Unix(),
		"nbf":                now.Add(-clockSkew).Unix(),
		"exp":                now.Add(s.ttl).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("stalwartauth: sign: %w", err)
	}
	s.cache.Add(principal, signed)
	return signed, nil
}

// Verify parses and validates a token against this signer's key,
// issuer and audience, returning its claims. Used by the userinfo
// endpoint and tests; Stalwart validates independently via JWKS.
func (s *Signer) Verify(tokenStr string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", t.Header["alg"])
		}
		return &s.key.PublicKey, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(s.audience),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// JWKS returns the JSON Web Key Set as served at `{issuer}/jwks.json`.
func (s *Signer) JWKS() []byte { return s.jwks }

// Discovery returns the OpenID discovery document as served at
// `{issuer}/.well-known/openid-configuration`.
func (s *Signer) Discovery() []byte { return s.disc }

// Issuer returns the configured issuer URL.
func (s *Signer) Issuer() string { return s.issuer }

// KeyID returns the JWK `kid` of the current signing key.
func (s *Signer) KeyID() string { return s.kid }

// jwk is a single RSA public key in JWK form.
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

func jwkFromPublic(pub *rsa.PublicKey) jwk {
	return jwk{
		Kty: "RSA",
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// thumbprint computes the RFC 7638 JWK thumbprint of an RSA public
// key, used as a stable, rotation-aware default `kid`.
func thumbprint(pub *rsa.PublicKey) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	// Canonical members in lexicographic order, no whitespace.
	canonical := `{"e":"` + e + `","kty":"RSA","n":"` + n + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// discoveryDocument is the subset of the OpenID Provider metadata
// Stalwart's discovery deserialiser requires (issuer, jwks_uri,
// userinfo/token/authorization endpoints) plus the advertised
// scopes and signing algorithm.
type discoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	UserinfoEndpoint                 string   `json:"userinfo_endpoint"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	AuthorizationEndpoint            string   `json:"authorization_endpoint"`
	ScopesSupported                  []string `json:"scopes_supported"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported                  []string `json:"claims_supported"`
}

func buildDiscovery(issuer string) discoveryDocument {
	return discoveryDocument{
		Issuer:                           issuer,
		JWKSURI:                          issuer + "/jwks.json",
		UserinfoEndpoint:                 issuer + "/userinfo",
		TokenEndpoint:                    issuer + "/token",
		AuthorizationEndpoint:            issuer + "/authorize",
		ScopesSupported:                  []string{"openid", "email"},
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256"},
		ClaimsSupported:                  []string{"sub", "iss", "aud", "exp", "iat", "email", "preferred_username"},
	}
}

// ParsePrivateKeyPEM decodes a PEM-encoded RSA private key in either
// PKCS#1 (`RSA PRIVATE KEY`) or PKCS#8 (`PRIVATE KEY`) form.
func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("stalwartauth: no PEM block found in key material")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("stalwartauth: PKCS#8 key is %T, not RSA", k)
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("stalwartauth: unsupported PEM block type %q", block.Type)
	}
}

// GenerateEphemeralKey produces a fresh in-memory RSA key. Used only
// in dev/CI where no signing key is provisioned; the key is discarded
// on restart, so every boot rotates the JWKS automatically.
func GenerateEphemeralKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
}

// KeySource describes where a Signer's RSA private key comes from, in
// precedence order: an explicit PEM file, an inline PEM string, or —
// only when AllowEphemeral is set (dev/CI) — a freshly generated
// ephemeral key.
type KeySource struct {
	// KeyFile is a path to a PEM-encoded RSA private key. Takes
	// precedence over Key.
	KeyFile string
	// Key is an inline PEM-encoded RSA private key (for deployments
	// that inject the key via a Secret env var rather than a file).
	Key string
	// AllowEphemeral permits generating a throwaway key when neither
	// KeyFile nor Key is set. Enable ONLY in dev/CI — a production
	// deployment must fail closed rather than mint with a key that is
	// not the one whose JWKS Stalwart fetched.
	AllowEphemeral bool
}

// LoadSigningKey resolves the RSA private key per KeySource's
// precedence (KeyFile → Key → ephemeral-if-allowed) and reports
// whether an ephemeral key was generated so the caller can log the
// non-production warning. When no key is configured and
// AllowEphemeral is false it returns an error (never a key), so a
// misconfigured production deployment fails closed rather than
// booting keyless. It is the shared key-loading path for every
// binary that mints Stalwart bearers (kmail-api and the standalone
// kmail-worker), which MUST use the same key material so a token
// minted by one validates against the JWKS the other publishes.
func LoadSigningKey(src KeySource) (key *rsa.PrivateKey, ephemeral bool, err error) {
	switch {
	case strings.TrimSpace(src.KeyFile) != "":
		pemBytes, rerr := os.ReadFile(src.KeyFile)
		if rerr != nil {
			return nil, false, fmt.Errorf("stalwartauth: read key file %s: %w", src.KeyFile, rerr)
		}
		k, perr := ParsePrivateKeyPEM(pemBytes)
		if perr != nil {
			return nil, false, fmt.Errorf("stalwartauth: parse key file %s: %w", src.KeyFile, perr)
		}
		return k, false, nil
	case strings.TrimSpace(src.Key) != "":
		k, perr := ParsePrivateKeyPEM([]byte(src.Key))
		if perr != nil {
			return nil, false, fmt.Errorf("stalwartauth: parse inline signing key: %w", perr)
		}
		return k, false, nil
	case src.AllowEphemeral:
		k, gerr := GenerateEphemeralKey()
		if gerr != nil {
			return nil, false, fmt.Errorf("stalwartauth: generate ephemeral key: %w", gerr)
		}
		return k, true, nil
	default:
		return nil, false, errors.New("stalwartauth: no signing key configured — refusing to mint without a key")
	}
}
