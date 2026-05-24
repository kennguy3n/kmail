package jmap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// issuedCert is the bundle returned by issueCert: the leaf cert,
// its private key, and PEM-encoded representations.
type issuedCert struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte
}

// issueCert mints an ECDSA-P256 X.509 cert. When `issuer` is nil
// the cert self-signs (use for the root). When `issuer` is set
// the cert is signed by the issuer's key, producing a proper
// chain so RequireAndVerifyClientCert works.
func issueCert(t *testing.T, cn string, dnsNames []string, isCA bool, issuer *issuedCert) *issuedCert {
	return issueCertWithLifetime(t, cn, dnsNames, isCA, issuer, time.Hour)
}

// issueCertWithLifetime is the variant of issueCert that lets a
// test pin the leaf NotAfter horizon. Used by the near-expiry
// WARN regression so the cert can be minted with e.g. a 1-hour
// remaining lifetime relative to the loader's injected clock.
func issueCertWithLifetime(t *testing.T, cn string, dnsNames []string, isCA bool, issuer *issuedCert, lifetime time.Duration) *issuedCert {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	var (
		parentTmpl *x509.Certificate
		parentKey  *ecdsa.PrivateKey
	)
	if issuer == nil {
		parentTmpl = tmpl
		parentKey = priv
	} else {
		parentTmpl = issuer.cert
		parentKey = issuer.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentTmpl, &priv.PublicKey, parentKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &issuedCert{cert: parsed, key: priv, certPEM: certPEM, keyPEM: keyPEM}
}

// genCert is a thin shim retained for the standalone validation /
// load tests that just need *some* cert and don't care about
// chain semantics. New tests should prefer issueCert directly.
func genCert(t *testing.T, cn string, dnsNames []string, isCA bool) (certPEM, keyPEM []byte, leaf *x509.Certificate) {
	t.Helper()
	c := issueCert(t, cn, dnsNames, isCA, nil)
	return c.certPEM, c.keyPEM, c.cert
}

func writeTempPEM(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// testLogger returns a *log.Logger that discards output. The
// proxy plumbs a logger through ClientTLSConfig.build to report
// keypair (re)loads; tests don't care about that surface.
func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

func TestClientTLSConfig_ValidationRejectsEmpty(t *testing.T) {
	cases := []struct {
		name string
		cfg  ClientTLSConfig
	}{
		{"empty cert", ClientTLSConfig{KeyFile: "k"}},
		{"empty key", ClientTLSConfig{CertFile: "c"}},
		{"both empty", ClientTLSConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestClientTLSConfig_BuildLoadsCert(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := genCert(t, "kmail-bff", []string{"kmail-bff"}, false)
	cfg := ClientTLSConfig{
		CertFile:   writeTempPEM(t, dir, "tls.crt", cert),
		KeyFile:    writeTempPEM(t, dir, "tls.key", key),
		ServerName: "stalwart.kmail.internal",
	}
	built, err := cfg.build(testLogger(t))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tlsCfg := built.base
	if tlsCfg.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate not wired")
	}
	got, err := tlsCfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if got == nil || len(got.Certificate) == 0 {
		t.Fatal("GetClientCertificate returned empty cert")
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want >= TLS 1.2", tlsCfg.MinVersion)
	}
	if tlsCfg.ServerName != "stalwart.kmail.internal" {
		t.Errorf("ServerName = %q", tlsCfg.ServerName)
	}
	if built.pinnedSrvName != "stalwart.kmail.internal" {
		t.Errorf("pinnedSrvName = %q, want stalwart.kmail.internal", built.pinnedSrvName)
	}
}

// TestClientTLSConfig_BuildLeavesServerNameEmpty pins the
// shard-failover-friendly default: when the operator does not
// set ServerName, build() leaves it empty so Go's transport
// derives SNI per-connection from each upstream URL.
func TestClientTLSConfig_BuildLeavesServerNameEmpty(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := genCert(t, "kmail-bff", []string{"kmail-bff"}, false)
	cfg := ClientTLSConfig{
		CertFile: writeTempPEM(t, dir, "tls.crt", cert),
		KeyFile:  writeTempPEM(t, dir, "tls.key", key),
	}
	built, err := cfg.build(testLogger(t))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.base.ServerName != "" {
		t.Errorf("ServerName = %q, want empty so transport derives per-connection", built.base.ServerName)
	}
	if built.pinnedSrvName != "" {
		t.Errorf("pinnedSrvName = %q, want empty", built.pinnedSrvName)
	}
}

// TestKeypairLoader_HotReloads writes a keypair, snapshots the
// loader's cached cert, overwrites the files with a different
// keypair (and a fresh mtime), and confirms the next call returns
// the new cert. This is what makes cert-manager rotations land
// without a pod restart.
func TestKeypairLoader_HotReloads(t *testing.T) {
	dir := t.TempDir()
	first := issueCert(t, "kmail-bff-v1", []string{"kmail-bff-v1"}, false, nil)
	certPath := writeTempPEM(t, dir, "tls.crt", first.certPEM)
	keyPath := writeTempPEM(t, dir, "tls.key", first.keyPEM)

	loader := &keypairLoader{
		certFile: certPath,
		keyFile:  keyPath,
		logger:   testLogger(t),
	}
	c1, err := loader.load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	leaf1, err := x509.ParseCertificate(c1.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf 1: %v", err)
	}
	if leaf1.Subject.CommonName != "kmail-bff-v1" {
		t.Fatalf("initial CN = %q, want kmail-bff-v1", leaf1.Subject.CommonName)
	}

	// Rotate: overwrite the files with a different keypair and
	// bump mtime past whatever filesystem granularity the test
	// runner uses (HFS+ / ext4 with relatime can have 1s steps).
	second := issueCert(t, "kmail-bff-v2", []string{"kmail-bff-v2"}, false, nil)
	if err := os.WriteFile(certPath, second.certPEM, 0o600); err != nil {
		t.Fatalf("rewrite cert: %v", err)
	}
	if err := os.WriteFile(keyPath, second.keyPEM, 0o600); err != nil {
		t.Fatalf("rewrite key: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("chtimes cert: %v", err)
	}
	if err := os.Chtimes(keyPath, future, future); err != nil {
		t.Fatalf("chtimes key: %v", err)
	}

	c2, err := loader.load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	leaf2, err := x509.ParseCertificate(c2.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf 2: %v", err)
	}
	if leaf2.Subject.CommonName != "kmail-bff-v2" {
		t.Errorf("rotated CN = %q, want kmail-bff-v2", leaf2.Subject.CommonName)
	}

	// Calling again without changing mtimes must hit the cache
	// (same *tls.Certificate pointer).
	c3, err := loader.load()
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	if c3 != c2 {
		t.Errorf("expected cached pointer reuse when mtimes unchanged")
	}
}

// TestClientTLSConfig_BuildLoadsCABundle asserts the CA bundle
// is wired into the tls.Config as a custom verifier — `RootCAs`
// is deliberately left nil because the verifier loads the pool
// fresh on every handshake (see caPoolLoader). Pinning this
// behaviour stops a future refactor from quietly reintroducing
// the once-at-startup load that motivated the change.
func TestClientTLSConfig_BuildLoadsCABundle(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := genCert(t, "kmail-bff", []string{"kmail-bff"}, false)
	caCert, _, _ := genCert(t, "kmail-ca", nil, true)
	cfg := ClientTLSConfig{
		CertFile: writeTempPEM(t, dir, "tls.crt", cert),
		KeyFile:  writeTempPEM(t, dir, "tls.key", key),
		CAFile:   writeTempPEM(t, dir, "ca.pem", caCert),
	}
	built, err := cfg.build(testLogger(t))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !built.customVerifier {
		t.Fatal("expected customVerifier=true when CAFile is set")
	}
	if built.caLoader == nil {
		t.Fatal("caLoader not wired")
	}
	// perConnConfig should produce a tls.Config with the documented
	// stdlib pattern (InsecureSkipVerify=true + VerifyConnection)
	// every time it's called — the per-connection cloning is what
	// guarantees the dial host is in scope for VerifyConnection.
	perConn := built.perConnConfig("stalwart.kmail.internal")
	if perConn.VerifyConnection == nil {
		t.Fatal("perConnConfig: VerifyConnection callback not wired")
	}
	if !perConn.InsecureSkipVerify {
		t.Fatal("perConnConfig: expected InsecureSkipVerify=true so VerifyConnection is the only verifier")
	}
	if perConn.ServerName != "stalwart.kmail.internal" {
		t.Errorf("perConnConfig: ServerName = %q, want stalwart.kmail.internal", perConn.ServerName)
	}
}

// TestCAPoolLoader_HotReloads writes a CA bundle, snapshots the
// loaded pool, replaces the file with a different CA, and checks
// that the loader picks up the new bundle on the next call. This
// is the parity test with TestKeypairLoader_HotReloads.
func TestCAPoolLoader_HotReloads(t *testing.T) {
	dir := t.TempDir()
	caA, _, _ := genCert(t, "kmail-ca-a", nil, true)
	caB, _, _ := genCert(t, "kmail-ca-b", nil, true)
	path := writeTempPEM(t, dir, "ca.pem", caA)

	loader := &caPoolLoader{caFile: path, logger: testLogger(t)}
	first, err := loader.load()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first == nil {
		t.Fatal("expected non-nil pool")
	}

	// Cached load returns the same pool when mtime is unchanged.
	cached, err := loader.load()
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if cached != first {
		t.Fatal("expected cached pool to be returned without re-parse")
	}

	// Replace the file and advance mtime; next load must re-parse
	// from disk and return a different pool than the cached one.
	if err := os.WriteFile(path, caB, 0o600); err != nil {
		t.Fatalf("rewrite CA: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	reloaded, err := loader.load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded == first {
		t.Fatal("expected reload to return a fresh *x509.CertPool")
	}
}

func TestClientTLSConfig_BuildRejectsBadCABundle(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := genCert(t, "kmail-bff", []string{"kmail-bff"}, false)
	cfg := ClientTLSConfig{
		CertFile: writeTempPEM(t, dir, "tls.crt", cert),
		KeyFile:  writeTempPEM(t, dir, "tls.key", key),
		CAFile:   writeTempPEM(t, dir, "bad.pem", []byte("not a pem")),
	}
	if _, err := cfg.build(testLogger(t)); err == nil {
		t.Fatal("expected error for non-PEM CA bundle")
	}
}

// TestProxy_MTLSHandshake stands up an httptest TLS server that
// requires a client certificate, points the proxy at it via
// ClientTLSConfig, and asserts the handshake succeeds. This is
// the end-to-end proof that the BFF presents the configured cert
// to Stalwart.
func TestProxy_MTLSHandshake(t *testing.T) {
	dir := t.TempDir()

	// One self-signed CA stands in for "the kmail internal PKI".
	ca := issueCert(t, "kmail-ca", nil, true, nil)
	// Server cert (Stalwart side) issued by the CA.
	server := issueCert(t, "stalwart", []string{"127.0.0.1"}, false, ca)
	// Client cert (BFF side) issued by the same CA.
	client := issueCert(t, "kmail-bff", nil, false, ca)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)

	serverCert, err := tls.X509KeyPair(server.certPEM, server.keyPEM)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			t.Errorf("expected client to present a cert")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	cfg := &ClientTLSConfig{
		CertFile:   writeTempPEM(t, dir, "tls.crt", client.certPEM),
		KeyFile:    writeTempPEM(t, dir, "tls.key", client.keyPEM),
		CAFile:     writeTempPEM(t, dir, "ca.pem", ca.certPEM),
		ServerName: "127.0.0.1",
	}
	built, err := cfg.build(testLogger(t))
	if err != nil {
		t.Fatalf("build TLS: %v", err)
	}
	transport := newClientTLSTransport(built)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Sanity check: stripping the client cert should now fail the
	// handshake. Build a fresh clientTLSBuild from the same files
	// but null out the GetClientCertificate so the server's
	// RequireAndVerifyClientCert demand has nothing to satisfy.
	noClientBuilt, err := cfg.build(testLogger(t))
	if err != nil {
		t.Fatalf("build no-client TLS: %v", err)
	}
	noClientBuilt.base.GetClientCertificate = nil
	noClientBuilt.base.Certificates = nil
	noClientTransport := newClientTLSTransport(noClientBuilt)
	if _, err := noClientTransport.RoundTrip(req.Clone(context.Background())); err == nil {
		t.Errorf("expected handshake failure without client cert")
	}

}

// TestProxy_MTLSIPLiteralVerifiesAgainstIPAddressesSAN pins the
// fix for the round-5 Devin Review finding: when the upstream URL
// is an IP literal (e.g. `https://127.0.0.1:8443`) and the
// operator did NOT pin ServerName, the verifier must still enforce
// hostname verification against the cert's `IPAddresses` SAN list
// instead of silently falling through to chain-only verification.
//
// Prior to the per-connection refactor, Go's TLS stack strips SNI
// for IP literals (RFC 6066) so `state.ServerName` arrived empty
// in VerifyConnection and `DNSName: ""` skipped the SAN check
// entirely. The new perConnConfig path closes the dial host over
// the verifier, so the IP becomes the DNSName argument and Go's
// x509 verifier routes it through the IPAddresses SAN check.
func TestProxy_MTLSIPLiteralVerifiesAgainstIPAddressesSAN(t *testing.T) {
	dir := t.TempDir()

	ca := issueCert(t, "kmail-ca", nil, true, nil)
	// Server cert has 127.0.0.1 in its IPAddresses SAN (added by
	// issueCert by default) but NO DNS SANs. This is the shape of
	// a cert issued for an IP-only endpoint.
	server := issueCert(t, "stalwart-ip", nil, false, ca)
	client := issueCert(t, "kmail-bff", nil, false, ca)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)

	serverCert, err := tls.X509KeyPair(server.certPEM, server.keyPEM)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	// NO ServerName pin set: this is the failure case the bot
	// identified. Verifier should still enforce IP SAN match
	// against the dial host (127.0.0.1).
	cfg := &ClientTLSConfig{
		CertFile: writeTempPEM(t, dir, "tls.crt", client.certPEM),
		KeyFile:  writeTempPEM(t, dir, "tls.key", client.keyPEM),
		CAFile:   writeTempPEM(t, dir, "ca.pem", ca.certPEM),
	}
	built, err := cfg.build(testLogger(t))
	if err != nil {
		t.Fatalf("build TLS: %v", err)
	}
	transport := newClientTLSTransport(built)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip against IP-literal URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Negative side: a server cert that does NOT include the
	// dial IP in its IPAddresses SAN must FAIL verification.
	// This proves the verifier is actually consulting the SAN
	// list — without the fix it would have silently accepted any
	// cert chained to our CA regardless of the IP.
	wrongIPServer := issueCertForIP(t, "stalwart-wrong-ip", net.ParseIP("10.0.0.99"), ca)
	wrongCert, err := tls.X509KeyPair(wrongIPServer.certPEM, wrongIPServer.keyPEM)
	if err != nil {
		t.Fatalf("load wrong-IP cert: %v", err)
	}
	srv2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv2.TLS = &tls.Config{
		Certificates: []tls.Certificate{wrongCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv2.StartTLS()
	defer srv2.Close()

	req2, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv2.URL, nil)
	if err != nil {
		t.Fatalf("new request 2: %v", err)
	}
	if _, err := transport.RoundTrip(req2); err == nil {
		t.Fatal("expected handshake failure: dial host 127.0.0.1 does not match cert's IPAddresses SAN 10.0.0.99")
	} else if !strings.Contains(err.Error(), "verify peer") && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "IP") {
		t.Logf("got expected handshake error: %v", err)
	}
}

// issueCertForIP is a variant of issueCert that overrides the
// default 127.0.0.1 IPAddresses entry with a caller-supplied IP.
// Used by the negative branch of the IP-SAN verification test.
func issueCertForIP(t *testing.T, cn string, ip net.IP, issuer *issuedCert) *issuedCert {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{ip},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.cert, &priv.PublicKey, issuer.key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &issuedCert{cert: parsed, key: priv, certPEM: certPEM, keyPEM: keyPEM}
}

func TestNewProxy_LogsWarningOnHTTPSWithoutTLS(t *testing.T) {
	// HTTPS StalwartURL with no client cert is a misconfiguration
	// in production but explicitly tolerated in dev/staging. The
	// proxy should construct successfully and log a warning rather
	// than refuse to boot — refusing here would prevent operators
	// from rolling mTLS out incrementally.
	var logBuf strings.Builder
	cfg := ProxyConfig{
		StalwartURL: "https://stalwart.test",
		Pool:        newDummyPool(t),
		Logger:      log.New(&logBuf, "", 0),
	}
	if _, err := NewProxy(cfg); err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if !strings.Contains(logBuf.String(), "no client TLS configured") {
		t.Errorf("expected mTLS warning, got: %q", logBuf.String())
	}
}

// TestKeypairLoader_WarnsWithinExpiryThreshold pins the operator
// safety net: when a loaded client cert is within
// certExpiryWarnThreshold of expiry (default 24h), the loader
// MUST emit a WARN log. Default cert-manager config issues 24h
// certs with 8h renewal -- anything inside 24h means renewal is
// broken or Reloader never restarted the pod.
func TestKeypairLoader_WarnsWithinExpiryThreshold(t *testing.T) {
	dir := t.TempDir()
	c := issueCertWithLifetime(t, "kmail-bff-soon-to-expire", []string{"kmail-bff"}, false, nil, time.Hour)
	certPath := writeTempPEM(t, dir, "tls.crt", c.certPEM)
	keyPath := writeTempPEM(t, dir, "tls.key", c.keyPEM)

	var buf strings.Builder
	loader := &keypairLoader{
		certFile: certPath,
		keyFile:  keyPath,
		logger:   log.New(&buf, "", 0),
		now:      func() time.Time { return time.Now() },
	}
	if _, err := loader.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(buf.String(), "WARN") || !strings.Contains(buf.String(), "expires in") {
		t.Errorf("expected near-expiry WARN, got: %q", buf.String())
	}

	before := buf.Len()
	if _, err := loader.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if buf.Len() != before {
		t.Errorf("expected WARN dedup, but log grew from %d to %d", before, buf.Len())
	}
}

func TestKeypairLoader_DoesNotWarnFarFromExpiry(t *testing.T) {
	dir := t.TempDir()
	c := issueCertWithLifetime(t, "kmail-bff-fresh", []string{"kmail-bff"}, false, nil, 720*time.Hour)
	certPath := writeTempPEM(t, dir, "tls.crt", c.certPEM)
	keyPath := writeTempPEM(t, dir, "tls.key", c.keyPEM)

	var buf strings.Builder
	loader := &keypairLoader{
		certFile: certPath,
		keyFile:  keyPath,
		logger:   log.New(&buf, "", 0),
	}
	if _, err := loader.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Contains(buf.String(), "expires in") || strings.Contains(buf.String(), "EXPIRED") {
		t.Errorf("unexpected near-expiry WARN for fresh cert: %q", buf.String())
	}
}

func TestKeypairLoader_WarnsOnExpiredCert(t *testing.T) {
	dir := t.TempDir()
	c := issueCertWithLifetime(t, "kmail-bff-expired", []string{"kmail-bff"}, false, nil, -time.Hour)
	certPath := writeTempPEM(t, dir, "tls.crt", c.certPEM)
	keyPath := writeTempPEM(t, dir, "tls.key", c.keyPEM)

	var buf strings.Builder
	loader := &keypairLoader{
		certFile: certPath,
		keyFile:  keyPath,
		logger:   log.New(&buf, "", 0),
	}
	if _, err := loader.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(buf.String(), "EXPIRED") {
		t.Errorf("expected EXPIRED WARN, got: %q", buf.String())
	}
}

func TestNewProxy_LogsWarningOnMTLSWithBareSvcHostname(t *testing.T) {
	dir := t.TempDir()
	c := issueCert(t, "kmail-bff", []string{"kmail-bff"}, false, nil)
	certPath := writeTempPEM(t, dir, "tls.crt", c.certPEM)
	keyPath := writeTempPEM(t, dir, "tls.key", c.keyPEM)

	var buf strings.Builder
	_, err := NewProxy(ProxyConfig{
		StalwartURL: "https://kmail-stalwart-0.kmail-stalwart.svc:8443",
		Pool:        newDummyPool(t),
		Logger:      log.New(&buf, "", 0),
		TLS: &ClientTLSConfig{
			CertFile: certPath,
			KeyFile:  keyPath,
		},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if !strings.Contains(buf.String(), "WARNING mTLS is enabled but StalwartURL hostname") {
		t.Errorf("expected bare-svc mTLS WARN, got: %q", buf.String())
	}
}

func TestNewProxy_LogsWarningOnMTLSWithHTTPURL(t *testing.T) {
	dir := t.TempDir()
	c := issueCert(t, "kmail-bff", []string{"kmail-bff"}, false, nil)
	certPath := writeTempPEM(t, dir, "tls.crt", c.certPEM)
	keyPath := writeTempPEM(t, dir, "tls.key", c.keyPEM)

	var buf strings.Builder
	_, err := NewProxy(ProxyConfig{
		StalwartURL: "http://kmail-stalwart-0.kmail-stalwart.svc.cluster.local:8080",
		Pool:        newDummyPool(t),
		Logger:      log.New(&buf, "", 0),
		TLS: &ClientTLSConfig{
			CertFile: certPath,
			KeyFile:  keyPath,
		},
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if !strings.Contains(buf.String(), "mutual-TLS only fires on https URLs") {
		t.Errorf("expected http+mTLS WARN, got: %q", buf.String())
	}
}

func TestIsBareSvcHostname(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"kmail-stalwart-0.kmail-stalwart.svc", true},
		{"kmail-stalwart-0.kmail-stalwart.svc.cluster.local", false},
		{"kmail-stalwart-0.kmail-stalwart.svc.example.com", false},
		{"localhost", false},
		{"stalwart.kmail.internal", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := isBareSvcHostname(tc.host); got != tc.want {
				t.Errorf("isBareSvcHostname(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
