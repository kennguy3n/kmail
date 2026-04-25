package confidentialsend

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCreateSecureMessage_Validation(t *testing.T) {
	s := NewService(nil)
	ctx := context.Background()

	cases := []struct {
		name string
		req  CreateRequest
		want string
	}{
		{"missing tenant", CreateRequest{SenderID: "s", EncryptedBlobRef: "r"}, "tenant_id required"},
		{"missing sender", CreateRequest{TenantID: "t", EncryptedBlobRef: "r"}, "sender_id required"},
		{"missing blob", CreateRequest{TenantID: "t", SenderID: "s"}, "encrypted_blob_ref required"},
		{"too long expiry", CreateRequest{TenantID: "t", SenderID: "s", EncryptedBlobRef: "r", ExpiresIn: 31 * 24 * time.Hour}, "30 days"},
		{"negative max views", CreateRequest{TenantID: "t", SenderID: "s", EncryptedBlobRef: "r", MaxViews: -1}, "max_views"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateSecureMessage(ctx, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateSecureMessage(%+v) = %v, want %q", tc.req, err, tc.want)
			}
		})
	}
}

func TestCreateSecureMessage_NilPool(t *testing.T) {
	s := NewService(nil)
	_, err := s.CreateSecureMessage(context.Background(), CreateRequest{
		TenantID:         "t",
		SenderID:         "s",
		EncryptedBlobRef: "r",
	})
	if err == nil || !strings.Contains(err.Error(), "pool not configured") {
		t.Fatalf("CreateSecureMessage nil pool = %v", err)
	}
}

func TestGetSecureMessage_EmptyToken(t *testing.T) {
	s := NewService(nil)
	if _, err := s.GetSecureMessage(context.Background(), "", ""); err != ErrLinkNotFound {
		t.Fatalf("GetSecureMessage empty token = %v, want ErrLinkNotFound", err)
	}
}

func TestRevokeLink_RequiresIDs(t *testing.T) {
	s := NewService(nil)
	if err := s.RevokeLink(context.Background(), "", "l"); err == nil {
		t.Fatalf("RevokeLink empty tenant expected error")
	}
	if err := s.RevokeLink(context.Background(), "t", ""); err == nil {
		t.Fatalf("RevokeLink empty link expected error")
	}
}

func TestListSentSecureMessages_RequiresTenantID(t *testing.T) {
	s := NewService(nil)
	if _, err := s.ListSentSecureMessages(context.Background(), "", ""); err == nil {
		t.Fatalf("ListSentSecureMessages empty tenant expected error")
	}
}

func TestNewToken_Uniqueness(t *testing.T) {
	a, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("newToken returned identical tokens")
	}
	if len(a) < 40 {
		t.Errorf("token too short: %d", len(a))
	}
}
