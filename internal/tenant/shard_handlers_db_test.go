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

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestShardHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	svc := NewShardService(pool, log.New(io.Discard, "", 0))
	h := NewShardHandlers(svc)

	// register
	rec := httptest.NewRecorder()
	body := `{"name":"` + uniqueName("hsh") + `","stalwart_url":"http://h:8080","max_mailboxes":50}`
	h.register(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Shard
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	// list
	rec = httptest.NewRecorder()
	h.list(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list=%d body=%s", rec.Code, rec.Body.String())
	}

	// health snapshot
	rec = httptest.NewRecorder()
	h.health(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health=%d body=%s", rec.Code, rec.Body.String())
	}

	// get (with tenants list)
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.SetPathValue("id", created.ID)
	h.get(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("get=%d body=%s", rec.Code, rec.Body.String())
	}

	// update
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"name":"`+created.Name+`","stalwart_url":"http://h2:8080","max_mailboxes":75,"status":"active"}`))
	r.SetPathValue("id", created.ID)
	h.update(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "h2:8080") {
		t.Fatalf("update=%d body=%s", rec.Code, rec.Body.String())
	}

	// rebalance: assign tenant to this shard.
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"tenant_id":"`+tenant+`"}`))
	r.SetPathValue("id", created.ID)
	h.rebalance(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("rebalance=%d body=%s", rec.Code, rec.Body.String())
	}

	// rebalance missing tenant_id → 400
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
	r.SetPathValue("id", created.ID)
	h.rebalance(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("rebalance no tenant=%d want 400", rec.Code)
	}

	// register malformed JSON → 400
	rec = httptest.NewRecorder()
	h.register(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{bad`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("register bad json=%d want 400", rec.Code)
	}
}

func TestPlacementHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "privacy", "active")

	// Seed a storage credential row so GetPlacementPolicy has a row.
	prov := NewZKFabricProvisioner(ZKFabricProvisioner{Pool: pool, Logger: log.New(io.Discard, "", 0)})
	if _, err := prov.Provision(context.Background(), tenant, "privacy"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	svc := NewPlacementService(pool, "") // no console → DB-only path
	h := NewPlacementHandlers(svc, pool)

	// regions
	rec := httptest.NewRecorder()
	h.regions(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "United States") {
		t.Fatalf("regions=%d body=%s", rec.Code, rec.Body.String())
	}

	// get
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.SetPathValue("id", tenant)
	h.get(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tenant) {
		t.Fatalf("get=%d body=%s", rec.Code, rec.Body.String())
	}

	// put: privacy plan allows client_side.
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"countries":["US"],"encryption_mode":"client_side"}`))
	r.SetPathValue("id", tenant)
	h.put(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("put=%d body=%s", rec.Code, rec.Body.String())
	}

	// put missing countries → 400
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"encryption_mode":"managed"}`))
	r.SetPathValue("id", tenant)
	h.put(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("put no countries=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// put malformed JSON → 400
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{bad`))
	r.SetPathValue("id", tenant)
	h.put(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("put bad json=%d want 400", rec.Code)
	}
}
