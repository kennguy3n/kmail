package tenant

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/cmk"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func testEnvelope(t *testing.T) cmk.SecretsEnvelope {
	t.Helper()
	// 32-byte all-zero key, hex-encoded (64 chars).
	env, err := cmk.NewAESGCMEnvelopeFromKeyMaterial(strings.Repeat("0", 64))
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

func TestZKFabricPureHelpers(t *testing.T) {
	if got := BucketNameFor("abc-123"); got == "" || !strings.Contains(got, "abc-123") {
		t.Errorf("BucketNameFor=%q", got)
	}
	// Every plan currently defaults to managed encryption.
	for _, plan := range []string{"core", "pro", "privacy", "unknown"} {
		if got := PlanEncryptionDefault(plan); got != EncryptionModeManaged {
			t.Errorf("PlanEncryptionDefault(%q)=%q want %q", plan, got, EncryptionModeManaged)
		}
	}
	// Signing mutates the request with an Authorization header.
	req, _ := http.NewRequest(http.MethodPut, "http://s3.local/bucket", nil)
	signEmptyPayloadSigV4(req, "AKIA", "secret", "us-east-1", time.Unix(1700000000, 0).UTC())
	if req.Header.Get("Authorization") == "" {
		t.Error("signEmptyPayloadSigV4 did not set Authorization")
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("signEmptyPayloadSigV4 did not set X-Amz-Date")
	}
}

func TestZKFabricWrapUnwrapRoundTrip(t *testing.T) {
	p := NewZKFabricProvisioner(ZKFabricProvisioner{Envelope: testEnvelope(t), Logger: log.New(io.Discard, "", 0)})
	blob, wrapped, err := p.wrapSecretKey("super-secret")
	if err != nil || !wrapped {
		t.Fatalf("wrapSecretKey wrapped=%v err=%v", wrapped, err)
	}
	if string(blob) == "super-secret" {
		t.Error("wrapped blob must not equal plaintext")
	}
	plain, wasEnc, err := p.unwrapSecretKey(blob)
	if err != nil || !wasEnc || plain != "super-secret" {
		t.Fatalf("unwrap plain=%q wasEnc=%v err=%v", plain, wasEnc, err)
	}

	// No envelope → plaintext passthrough with wrapped=false.
	p2 := NewZKFabricProvisioner(ZKFabricProvisioner{Logger: log.New(io.Discard, "", 0)})
	blob2, wrapped2, err := p2.wrapSecretKey("plain-key")
	if err != nil || wrapped2 || string(blob2) != "plain-key" {
		t.Fatalf("no-envelope wrap blob=%q wrapped=%v err=%v", blob2, wrapped2, err)
	}
}

func TestZKFabricProvisionRoundTripDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")

	// Mock S3: any PUT (CreateBucket) → 200.
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3.Close)

	// Mock console: POST keys → access/secret; PUT placement → 200.
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/keys"):
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_key": "AKIAEXAMPLE",
				"secret_key": "s3cr3t-key-value",
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/placement"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(console.Close)

	p := NewZKFabricProvisioner(ZKFabricProvisioner{
		Pool:           pool,
		Envelope:       testEnvelope(t),
		S3URL:          s3.URL,
		ConsoleURL:     console.URL,
		AdminAccessKey: "admin",
		AdminSecretKey: "adminsecret",
		Region:         "us-east-1",
		Logger:         log.New(io.Discard, "", 0),
	})

	cred, err := p.Provision(context.Background(), tenant, "pro")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if cred.AccessKey != "AKIAEXAMPLE" || cred.SecretKey != "s3cr3t-key-value" {
		t.Fatalf("Provision cred=%+v", cred)
	}
	if !cred.WasEncrypted() {
		t.Error("Provision should report wasEncrypted=true with envelope")
	}

	// LookupStorageCredential should decrypt back to plaintext.
	got, err := p.LookupStorageCredential(context.Background(), tenant)
	if err != nil {
		t.Fatalf("LookupStorageCredential: %v", err)
	}
	if got.SecretKey != "s3cr3t-key-value" || got.BucketName != cred.BucketName {
		t.Fatalf("Lookup mismatch got=%+v", got)
	}
	if !got.WasEncrypted() {
		t.Error("Lookup should report wasEncrypted=true")
	}
}

func TestZKFabricProvisionValidation(t *testing.T) {
	p := NewZKFabricProvisioner(ZKFabricProvisioner{Logger: log.New(io.Discard, "", 0)})
	if _, err := p.Provision(context.Background(), "", "pro"); err == nil {
		t.Error("Provision with empty tenant must error")
	}
}
