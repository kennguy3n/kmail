package search

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func nilPoolHandlers(be *recordingBackend) *Handlers {
	if be != nil {
		be.name = BackendSharedMeilisearch
	}
	var backends []SearchBackend
	if be != nil {
		backends = []SearchBackend{be}
	}
	svc := NewService(Config{Logger: log.New(io.Discard, "", 0), Backends: backends})
	return NewHandlers(svc, log.New(io.Discard, "", 0))
}

func TestSearchHandlers_CrossTenantForbidden(t *testing.T) {
	h := nilPoolHandlers(&recordingBackend{})
	for _, tc := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"getBackend", h.getBackend, searchReq("t1", "t2", http.MethodGet, "")},
		{"putBackend", h.putBackend, searchReq("t1", "t2", http.MethodPut, `{"backend":"shared_meilisearch"}`)},
		{"reindex", h.reindex, searchReq("t1", "t2", http.MethodPost, "")},
	} {
		rec := httptest.NewRecorder()
		tc.fn(rec, tc.req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-tenant=%d want 403", tc.name, rec.Code)
		}
	}

	// Missing tenant context ⇒ also forbidden.
	rec := httptest.NewRecorder()
	h.getBackend(rec, searchReq("t1", "", http.MethodGet, ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing-context=%d want 403", rec.Code)
	}
}

func TestSearchHandlers_PutBackendErrors(t *testing.T) {
	h := nilPoolHandlers(&recordingBackend{})

	// Malformed JSON ⇒ 400.
	rec := httptest.NewRecorder()
	h.putBackend(rec, searchReq("t1", "t1", http.MethodPut, `{not-json`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad-json=%d want 400", rec.Code)
	}

	// Unrecognised backend value ⇒ ErrInvalidInput ⇒ 400.
	rec = httptest.NewRecorder()
	h.putBackend(rec, searchReq("t1", "t1", http.MethodPut, `{"backend":"bogus"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid-backend=%d want 400", rec.Code)
	}

	// Valid value but unwired in this BFF ⇒ ErrBackendUnavailable ⇒ 400.
	rec = httptest.NewRecorder()
	h.putBackend(rec, searchReq("t1", "t1", http.MethodPut, `{"backend":"dedicated_opensearch"}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("unavailable=%d body=%s want 400", rec.Code, rec.Body.String())
	}
}

func TestSearchHandlers_ReindexErrorPath(t *testing.T) {
	// Backend whose DeleteIndex fails ⇒ Reindex bubbles ⇒ 500.
	h := nilPoolHandlers(&recordingBackend{failOn: "delete"})
	rec := httptest.NewRecorder()
	h.reindex(rec, searchReq("t1", "t1", http.MethodPost, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("reindex-error=%d want 500", rec.Code)
	}
}

func TestCutoverHandlers_GuardPaths(t *testing.T) {
	// nil service is fine: every assertion below returns before the
	// handler touches the CutoverService.
	h := NewCutoverHandlers(nil, log.New(io.Discard, "", 0))

	rec := httptest.NewRecorder()
	h.listJobs(rec, searchReq("t1", "t2", http.MethodGet, ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("listJobs cross-tenant=%d want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.initiate(rec, searchReq("t1", "t2", http.MethodPost, `{"target_backend":"shared_opensearch"}`))
	if rec.Code != http.StatusForbidden {
		t.Errorf("initiate cross-tenant=%d want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.initiate(rec, searchReq("t1", "t1", http.MethodPost, `{bad json`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("initiate bad-json=%d want 400", rec.Code)
	}
}

func TestStatusFor(t *testing.T) {
	cases := map[error]int{
		ErrInvalidInput:       http.StatusBadRequest,
		ErrBackendUnavailable: http.StatusBadRequest,
		ErrNotFound:           http.StatusNotFound,
		io.EOF:                http.StatusInternalServerError,
	}
	for err, want := range cases {
		if got := statusFor(err); got != want {
			t.Errorf("statusFor(%v)=%d want %d", err, got, want)
		}
	}
}
