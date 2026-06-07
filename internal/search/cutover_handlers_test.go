package search

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// staticGetter reports a fixed current backend for every tenant so
// InitiateCutover/ExecuteCutover can compute the source backend.
type staticGetter struct{ backend string }

func (g staticGetter) GetBackend(context.Context, string) (string, error) {
	return g.backend, nil
}

func newCutoverHandlers(t *testing.T) (*CutoverHandlers, *inMemoryCutoverStore, string) {
	t.Helper()
	const tenant = "tenant-cut"
	store := newInMemoryStore([]string{tenant})
	flipper := newFakeFlipper(store)
	svc, err := NewCutoverService(CutoverServiceConfig{
		Store:     store,
		Flipper:   flipper,
		Source:    MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil }),
		Sizer:     MailboxSizerFunc(func(context.Context, string) (int64, error) { return 1000, nil }),
		Getter:    staticGetter{backend: BackendMeilisearch},
		Threshold: 100,
		Logger:    log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewCutoverService: %v", err)
	}
	return NewCutoverHandlers(svc, log.New(io.Discard, "", 0)), store, tenant
}

func cutReq(tenant, ctxTenant, method, body string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), ctxTenant)
	ctx = middleware.WithKChatUserID(ctx, "operator-1")
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	r.SetPathValue("id", tenant)
	return r
}

func TestCutoverHandlersInitiateAndList(t *testing.T) {
	h, _, tenant := newCutoverHandlers(t)

	// listJobs with no history → empty array.
	rec := httptest.NewRecorder()
	h.listJobs(rec, cutReq(tenant, tenant, http.MethodGet, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"jobs"`) {
		t.Fatalf("listJobs=%d body=%s", rec.Code, rec.Body.String())
	}

	// initiate a cutover to shared_opensearch → drives to completion.
	rec = httptest.NewRecorder()
	h.initiate(rec, cutReq(tenant, tenant, http.MethodPost, `{"target_backend":"opensearch"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("initiate=%d body=%s", rec.Code, rec.Body.String())
	}

	// listJobs now reports the row.
	rec = httptest.NewRecorder()
	h.listJobs(rec, cutReq(tenant, tenant, http.MethodGet, ""))
	if !strings.Contains(rec.Body.String(), "opensearch") {
		t.Errorf("listJobs after initiate=%s", rec.Body.String())
	}
}

func TestCutoverHandlersScopeAndValidation(t *testing.T) {
	h, _, tenant := newCutoverHandlers(t)

	// Cross-tenant → 403.
	rec := httptest.NewRecorder()
	h.listJobs(rec, cutReq(tenant, "other-tenant", http.MethodGet, ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("listJobs cross-tenant=%d want 403", rec.Code)
	}

	// Malformed JSON → 400.
	rec = httptest.NewRecorder()
	h.initiate(rec, cutReq(tenant, tenant, http.MethodPost, `{bad`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("initiate bad json=%d want 400", rec.Code)
	}

	// Invalid backend value → 400.
	rec = httptest.NewRecorder()
	h.initiate(rec, cutReq(tenant, tenant, http.MethodPost, `{"target_backend":"bogus"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("initiate bad backend=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// Initiating onto the current backend → 400 (already on backend).
	rec = httptest.NewRecorder()
	h.initiate(rec, cutReq(tenant, tenant, http.MethodPost, `{"target_backend":"meilisearch"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("initiate same backend=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}
