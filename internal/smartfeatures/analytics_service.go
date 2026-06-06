package smartfeatures

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// AnalyticsSource supplies the analytics handler with the caller's
// recent Sent and Inbox message windows. It is an interface so the
// handler is unit-testable against an in-memory fake.
type AnalyticsSource interface {
	// Window returns sent and received messages with ReceivedAt at
	// or after `since`, each capped at `limit` (newest first).
	Window(ctx context.Context, tenantID, kchatUserID string, since time.Time, limit int) (sent, received []Message, err error)
}

// JMAPAnalyticsSource implements AnalyticsSource against the BFF
// JMAP InternalClient. It resolves the Sent and Inbox mailboxes by
// RFC 8621 role, queries each within the window, and hydrates the
// ids via the shared fetcher so header/address parsing matches the
// rest of the package.
type JMAPAnalyticsSource struct {
	client  dispatcher
	fetcher EmailFetcher
}

// NewJMAPAnalyticsSource builds the source. The client is required.
func NewJMAPAnalyticsSource(client *jmap.InternalClient) (*JMAPAnalyticsSource, error) {
	if client == nil {
		return nil, errors.New("smartfeatures.NewJMAPAnalyticsSource: client is required")
	}
	fetcher, err := NewJMAPFetcher(client)
	if err != nil {
		return nil, err
	}
	return &JMAPAnalyticsSource{client: client, fetcher: fetcher}, nil
}

// Window resolves the Sent and Inbox mailboxes and queries each.
func (s *JMAPAnalyticsSource) Window(ctx context.Context, tenantID, kchatUserID string, since time.Time, limit int) ([]Message, []Message, error) {
	if limit <= 0 {
		limit = 500
	}
	roles, err := s.mailboxIDsByRole(ctx, tenantID, kchatUserID)
	if err != nil {
		return nil, nil, err
	}
	sent, err := s.queryWindow(ctx, tenantID, kchatUserID, roles["sent"], since, limit)
	if err != nil {
		return nil, nil, err
	}
	received, err := s.queryWindow(ctx, tenantID, kchatUserID, roles["inbox"], since, limit)
	if err != nil {
		return nil, nil, err
	}
	return sent, received, nil
}

func (s *JMAPAnalyticsSource) queryWindow(ctx context.Context, tenantID, kchatUserID, mailboxID string, since time.Time, limit int) ([]Message, error) {
	filter := map[string]any{"after": since.UTC().Format(time.RFC3339)}
	if mailboxID != "" {
		filter["inMailbox"] = mailboxID
	}
	req := jmap.JmapRequest{
		Using: []string{jmapCoreCapability, jmapMailCapability},
		MethodCalls: [][]any{
			{"Email/query", map[string]any{
				"filter":         filter,
				"sort":           []map[string]any{{"property": "receivedAt", "isAscending": false}},
				"limit":          limit,
				"calculateTotal": false,
			}, "q0"},
		},
	}
	resp, err := s.client.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		return nil, fmt.Errorf("smartfeatures.analytics query: %w", err)
	}
	if cerr := resp.FirstCallError(); cerr != nil {
		return nil, fmt.Errorf("smartfeatures.analytics query: %w", cerr)
	}
	_, args, ok := resp.CallByID("q0")
	if !ok {
		return nil, errors.New("smartfeatures.analytics: missing Email/query response")
	}
	ids := stringSlice(args["ids"])
	if len(ids) == 0 {
		return []Message{}, nil
	}
	byID, err := s.fetcher.FetchMessages(ctx, tenantID, kchatUserID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// mailboxIDsByRole maps lower-cased RFC 8621 roles ("inbox",
// "sent", ...) to mailbox ids. Roles that don't exist are simply
// absent, in which case the query falls back to unfiltered mail.
func (s *JMAPAnalyticsSource) mailboxIDsByRole(ctx context.Context, tenantID, kchatUserID string) (map[string]string, error) {
	req := jmap.JmapRequest{
		Using: []string{jmapCoreCapability, jmapMailCapability},
		MethodCalls: [][]any{
			{"Mailbox/get", map[string]any{"properties": []string{"id", "role"}}, "m0"},
		},
	}
	resp, err := s.client.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		return nil, fmt.Errorf("smartfeatures.analytics mailboxes: %w", err)
	}
	if cerr := resp.FirstCallError(); cerr != nil {
		return nil, fmt.Errorf("smartfeatures.analytics mailboxes: %w", cerr)
	}
	_, args, ok := resp.CallByID("m0")
	if !ok {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	list, _ := args["list"].([]any)
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := asString(obj["role"])
		if role == "" {
			continue
		}
		out[role] = asString(obj["id"])
	}
	return out, nil
}
