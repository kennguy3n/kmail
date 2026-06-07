package sharedinbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// seedInboxAndUser inserts a shared_inboxes row and a users row and
// returns their IDs, so assignment/note handlers satisfy their FKs.
func seedInboxAndUser(t *testing.T, svc *WorkflowService, tenant string) (inboxID, userID string) {
	t.Helper()
	ctx := context.Background()
	u := fmt.Sprintf("%d", time.Now().UnixNano())
	err := pgx.BeginFunc(ctx, svc.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenant); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO shared_inboxes (tenant_id, address, display_name, mls_group_id)
			VALUES ($1::uuid, $2, $3, $4) RETURNING id::text
		`, tenant, "si-"+u+"@example.com", "Team", "mls-"+u).Scan(&inboxID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name)
			VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text
		`, tenant, "kc-"+u, "sw-"+u, "u-"+u+"@example.com", "User").Scan(&userID)
	})
	if err != nil {
		t.Fatalf("seed inbox/user: %v", err)
	}
	return inboxID, userID
}

func siReq(tenant, method, body string, pv map[string]string) *http.Request {
	ctx := middleware.WithTenantID(context.Background(), tenant)
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(ctx, method, "/x", nil)
	} else {
		r = httptest.NewRequestWithContext(ctx, method, "/x", strings.NewReader(body))
	}
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestSharedInboxHandlersDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	inbox, user := seedInboxAndUser(t, svc, tenant)
	email := "email-123"
	pv := map[string]string{"inboxId": inbox, "emailId": email}

	// mlsStatus (no MLS manager → enabled:false)
	rec := httptest.NewRecorder()
	h.mlsStatus(rec, siReq(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("mlsStatus=%d body=%s", rec.Code, rec.Body.String())
	}

	// assign
	rec = httptest.NewRecorder()
	h.assign(rec, siReq(tenant, http.MethodPost, `{"assignee_user_id":"`+user+`"}`, pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("assign=%d body=%s", rec.Code, rec.Body.String())
	}

	// setStatus
	rec = httptest.NewRecorder()
	h.setStatus(rec, siReq(tenant, http.MethodPatch, `{"status":"in_progress"}`, pv))
	if rec.Code != http.StatusOK {
		t.Fatalf("setStatus=%d body=%s", rec.Code, rec.Body.String())
	}

	// addNote + listNotes
	rec = httptest.NewRecorder()
	h.addNote(rec, siReq(tenant, http.MethodPost, `{"author_user_id":"`+user+`","note_text":"hello"}`, pv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("addNote=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.listNotes(rec, siReq(tenant, http.MethodGet, "", pv))
	if rec.Code != http.StatusOK {
		t.Errorf("listNotes=%d", rec.Code)
	}

	// listAssignments
	rec = httptest.NewRecorder()
	h.listAssignments(rec, siReq(tenant, http.MethodGet, "", map[string]string{"inboxId": inbox}))
	if rec.Code != http.StatusOK {
		t.Errorf("listAssignments=%d", rec.Code)
	}

	// unassign
	rec = httptest.NewRecorder()
	h.unassign(rec, siReq(tenant, http.MethodDelete, "", pv))
	if rec.Code != http.StatusNoContent {
		t.Errorf("unassign=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSharedInboxHandlersValidation(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	svc := NewService(pool, log.New(io.Discard, "", 0))
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	pv := map[string]string{"inboxId": "inbox", "emailId": "email"}

	// Missing tenant context → 403.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.SetPathValue("inboxId", "inbox")
	h.listAssignments(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no tenant listAssignments=%d want 403", rec.Code)
	}

	// Invalid status → 400.
	rec = httptest.NewRecorder()
	h.setStatus(rec, siReq(tenant, http.MethodPatch, `{"status":"bogus"}`, pv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}

	// Malformed JSON on assign → 400.
	rec = httptest.NewRecorder()
	h.assign(rec, siReq(tenant, http.MethodPost, `{bad`, pv))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad assign json=%d want 400", rec.Code)
	}
}

