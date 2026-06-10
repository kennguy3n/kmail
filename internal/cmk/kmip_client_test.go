package cmk

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// kmipTestServer is an in-process TLS endpoint that speaks just
// enough KMIP to round-trip one batch item. handler receives the
// decoded operation + request payload and returns a response
// payload that the server frames into a ResponseMessage.
func kmipTestServer(t *testing.T, resultStatus int32, respPayload func(op int32, reqPayload []byte) []byte) (addr string, tlsCfg *tls.Config) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reqBytes, err := readTTLV(conn)
		if err != nil {
			return
		}
		op, payload := decodeRequestOp(reqBytes)
		var inner []byte
		if respPayload != nil {
			inner = respPayload(op, payload)
		}
		batch := encodeStructure(tagBatchItem, concat(
			encodeEnumeration(tagResultStatus, resultStatus),
			encodeStructure(tagResponsePayload, inner),
		))
		_, _ = conn.Write(encodeStructure(tagResponseMessage, batch))
	}()
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })

	return ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

// decodeRequestOp pulls the operation enum + request payload out of
// a framed RequestMessage for the server side of the test.
func decodeRequestOp(msg []byte) (int32, []byte) {
	_, _, val, err := decodeTTLV(msg)
	if err != nil {
		return 0, nil
	}
	rest := val
	for len(rest) > 0 {
		t, ty, v, err := decodeTTLV(rest)
		if err != nil {
			return 0, nil
		}
		rest = rest[paddedLen(len(v)+8):]
		if t == tagBatchItem && ty == ttlvStructure {
			inner := v
			var op int32
			var payload []byte
			for len(inner) > 0 {
				it, ity, iv, err := decodeTTLV(inner)
				if err != nil {
					return 0, nil
				}
				inner = inner[paddedLen(len(iv)+8):]
				if it == tagOperation && ity == ttlvEnumeration && len(iv) >= 4 {
					op = int32(uint32(iv[0])<<24 | uint32(iv[1])<<16 | uint32(iv[2])<<8 | uint32(iv[3]))
				}
				if it == tagRequestPayload && ity == ttlvStructure {
					payload = iv
				}
			}
			return op, payload
		}
	}
	return 0, nil
}

func TestNewKMIPClientDefaults(t *testing.T) {
	c := NewKMIPClient("hsm:5696", nil)
	if c.TLS == nil || c.TLS.InsecureSkipVerify {
		t.Errorf("default TLS must verify identity: %+v", c.TLS)
	}
	if c.Timeout == 0 {
		t.Error("expected non-zero timeout")
	}
}

func TestKMIPEncryptDecryptLocateRoundTrip(t *testing.T) {
	addr, tlsCfg := kmipTestServer(t, 0, func(op int32, _ []byte) []byte {
		switch op {
		case opEncrypt:
			return concat(encodeBytes(tagData, []byte("CIPHER")), encodeBytes(tagIVCounterNonce, []byte("IV123456")))
		case opDecrypt:
			return encodeBytes(tagData, []byte("PLAIN"))
		case opLocate:
			return encodeText(tagUniqueIdentifier, "key-123")
		}
		return nil
	})
	c := NewKMIPClient(addr, tlsCfg)

	ct, iv, err := c.Encrypt("key-123", []byte("PLAIN"))
	if err != nil || string(ct) != "CIPHER" || string(iv) != "IV123456" {
		t.Fatalf("Encrypt ct=%q iv=%q err=%v", ct, iv, err)
	}
}

func TestKMIPDecrypt(t *testing.T) {
	addr, tlsCfg := kmipTestServer(t, 0, func(op int32, _ []byte) []byte {
		return encodeBytes(tagData, []byte("PLAIN"))
	})
	c := NewKMIPClient(addr, tlsCfg)
	pt, err := c.Decrypt("key-123", []byte("CIPHER"), []byte("IV123456"))
	if err != nil || string(pt) != "PLAIN" {
		t.Fatalf("Decrypt pt=%q err=%v", pt, err)
	}
}

func TestKMIPLocate(t *testing.T) {
	addr, tlsCfg := kmipTestServer(t, 0, func(op int32, _ []byte) []byte {
		return encodeText(tagUniqueIdentifier, "key-xyz")
	})
	c := NewKMIPClient(addr, tlsCfg)
	id, err := c.Locate("kmail-cmk-tenant-1")
	if err != nil || id != "key-xyz" {
		t.Fatalf("Locate id=%q err=%v", id, err)
	}
}

func TestKMIPNonSuccessStatus(t *testing.T) {
	addr, tlsCfg := kmipTestServer(t, 1, func(op int32, _ []byte) []byte {
		return encodeText(tagUniqueIdentifier, "ignored")
	})
	c := NewKMIPClient(addr, tlsCfg)
	if _, err := c.Locate("x"); err == nil {
		t.Error("expected error for non-success ResultStatus")
	}
}

func TestKMIPExchangeNoAddress(t *testing.T) {
	c := &KMIPClient{Timeout: time.Second}
	if _, _, err := c.Encrypt("k", []byte("p")); err == nil {
		t.Error("expected error when address is empty")
	}
}

func TestKMIPDialFailure(t *testing.T) {
	c := NewKMIPClient("127.0.0.1:1", &tls.Config{MinVersion: tls.VersionTLS12})
	c.Timeout = 500 * time.Millisecond
	if _, err := c.Locate("x"); err == nil {
		t.Error("expected dial failure")
	}
}
