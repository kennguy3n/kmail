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
	tlsCfg, err := cfg.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(tlsCfg.Certificates))
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want >= TLS 1.2", tlsCfg.MinVersion)
	}
	if tlsCfg.ServerName != "stalwart.kmail.internal" {
		t.Errorf("ServerName = %q", tlsCfg.ServerName)
	}
}

func TestClientTLSConfig_BuildLoadsCABundle(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := genCert(t, "kmail-bff", []string{"kmail-bff"}, false)
	caCert, _, _ := genCert(t, "kmail-ca", nil, true)
	cfg := ClientTLSConfig{
		CertFile: writeTempPEM(t, dir, "tls.crt", cert),
		KeyFile:  writeTempPEM(t, dir, "tls.key", key),
		CAFile:   writeTempPEM(t, dir, "ca.pem", caCert),
	}
	tlsCfg, err := cfg.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tlsCfg.RootCAs == nil {
		t.Fatal("RootCAs not populated")
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
	if _, err := cfg.build(); err == nil {
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
	tlsCfg, err := cfg.build()
	if err != nil {
		t.Fatalf("build TLS: %v", err)
	}
	transport := newClientTLSTransport(tlsCfg)

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
	// handshake. Reuse the same CA-pinned config but drop the
	// client material.
	noClientCfg := tlsCfg.Clone()
	noClientCfg.Certificates = nil
	noClientTransport := newClientTLSTransport(noClientCfg)
	if _, err := noClientTransport.RoundTrip(req.Clone(context.Background())); err == nil {
		t.Errorf("expected handshake failure without client cert")
	}

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
