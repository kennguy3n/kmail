package priority

import (
	"context"
	"errors"
	"fmt"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/smartfeatures"
)

// Source supplies the priority service with the recent inbox
// window and the user's own address. Both are behind this narrow
// interface so the service unit-tests against an in-memory fake
// with no Stalwart dependency.
type Source interface {
	// ListInbox returns the most recent messages in the user's
	// Inbox, newest first, capped at limit.
	ListInbox(ctx context.Context, tenantID, kchatUserID string, limit int) ([]smartfeatures.Message, error)
	// UserAddress returns the user's primary email address (for
	// @mention and same-tenant detection). Returns "" with no error
	// when it can't be resolved — the scorer treats that as "unknown"
	// rather than failing the whole request.
	UserAddress(ctx context.Context, tenantID, kchatUserID string) (string, error)
}

type dispatcher interface {
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

const (
	jmapCoreCapability = "urn:ietf:params:jmap:core"
	jmapMailCapability = "urn:ietf:params:jmap:mail"
)

// JMAPSource implements Source against the BFF JMAP InternalClient.
// It composes a smartfeatures fetcher to hydrate the queried ids
// into Message values, reusing that package's header-parsing so
// the two features can't drift on which headers they request.
type JMAPSource struct {
	client  dispatcher
	fetcher smartfeatures.EmailFetcher
}

// NewJMAPSource builds the source. The InternalClient is required.
func NewJMAPSource(client *jmap.InternalClient) (*JMAPSource, error) {
	if client == nil {
		return nil, errors.New("priority.NewJMAPSource: client is required")
	}
	fetcher, err := smartfeatures.NewJMAPFetcher(client)
	if err != nil {
		return nil, err
	}
	return &JMAPSource{client: client, fetcher: fetcher}, nil
}

// ListInbox resolves the Inbox mailbox (by RFC 8621 role), queries
// the most recent message ids in it, then hydrates them via the
// shared smartfeatures fetcher. Query order (newest first) is
// preserved in the returned slice.
func (s *JMAPSource) ListInbox(ctx context.Context, tenantID, kchatUserID string, limit int) ([]smartfeatures.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	inboxID, err := s.inboxMailboxID(ctx, tenantID, kchatUserID)
	if err != nil {
		return nil, err
	}

	filter := map[string]any{}
	if inboxID != "" {
		filter["inMailbox"] = inboxID
	}
	req := jmap.JmapRequest{
		Using: []string{jmapCoreCapability, jmapMailCapability},
		MethodCalls: [][]any{
			{"Email/query", map[string]any{
				"filter": filter,
				"sort": []map[string]any{
					{"property": "receivedAt", "isAscending": false},
				},
				"limit":          limit,
				"calculateTotal": false,
			}, "q0"},
		},
	}
	resp, err := s.client.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		return nil, fmt.Errorf("priority.ListInbox: query: %w", err)
	}
	if cerr := resp.FirstCallError(); cerr != nil {
		return nil, fmt.Errorf("priority.ListInbox: %w", cerr)
	}
	_, args, ok := resp.CallByID("q0")
	if !ok {
		return nil, errors.New("priority.ListInbox: missing Email/query response")
	}
	ids := stringSlice(args["ids"])
	if len(ids) == 0 {
		return []smartfeatures.Message{}, nil
	}

	byID, err := s.fetcher.FetchMessages(ctx, tenantID, kchatUserID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]smartfeatures.Message, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// inboxMailboxID returns the id of the mailbox with role "inbox",
// or "" when none is found (in which case ListInbox falls back to
// an unfiltered recent-mail query).
func (s *JMAPSource) inboxMailboxID(ctx context.Context, tenantID, kchatUserID string) (string, error) {
	req := jmap.JmapRequest{
		Using: []string{jmapCoreCapability, jmapMailCapability},
		MethodCalls: [][]any{
			{"Mailbox/get", map[string]any{
				"properties": []string{"id", "role"},
			}, "m0"},
		},
	}
	resp, err := s.client.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		return "", fmt.Errorf("priority.inboxMailboxID: %w", err)
	}
	if cerr := resp.FirstCallError(); cerr != nil {
		return "", fmt.Errorf("priority.inboxMailboxID: %w", cerr)
	}
	_, args, ok := resp.CallByID("m0")
	if !ok {
		return "", nil
	}
	list, _ := args["list"].([]any)
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := obj["role"].(string); role == "inbox" {
			id, _ := obj["id"].(string)
			return id, nil
		}
	}
	return "", nil
}

// UserAddress resolves the user's primary identity email via
// Identity/get. A missing identity is not an error — it returns
// "" so same-tenant / @mention signals simply don't fire.
func (s *JMAPSource) UserAddress(ctx context.Context, tenantID, kchatUserID string) (string, error) {
	req := jmap.JmapRequest{
		Using: []string{jmapCoreCapability, "urn:ietf:params:jmap:submission"},
		MethodCalls: [][]any{
			{"Identity/get", map[string]any{
				"properties": []string{"id", "email"},
			}, "i0"},
		},
	}
	resp, err := s.client.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		// Identity resolution is best-effort; downgrade to "unknown".
		return "", nil
	}
	if resp.FirstCallError() != nil {
		return "", nil
	}
	_, args, ok := resp.CallByID("i0")
	if !ok {
		return "", nil
	}
	list, _ := args["list"].([]any)
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if email, _ := obj["email"].(string); email != "" {
			return email, nil
		}
	}
	return "", nil
}

func stringSlice(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
