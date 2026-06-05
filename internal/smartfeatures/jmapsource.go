package smartfeatures

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// EmailFetcher loads the metadata view of one or more emails for a
// user. The smart-reply / categorization / unsubscribe handlers
// depend only on this narrow surface so they can be unit-tested
// against an in-memory fake without a live Stalwart.
type EmailFetcher interface {
	FetchMessages(ctx context.Context, tenantID, kchatUserID string, emailIDs []string) (map[string]Message, error)
}

// dispatcher is the subset of *jmap.InternalClient the JMAP-backed
// fetcher uses. Declared as an interface so the adapter itself can
// be tested with a fake dispatcher.
type dispatcher interface {
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

const (
	jmapCoreCapability = "urn:ietf:params:jmap:core"
	jmapMailCapability = "urn:ietf:params:jmap:mail"
)

// headerProps are the RFC 8621 `header:Name:asText` properties the
// fetcher requests so categorization and unsubscribe parsing have
// the raw header values they need. JMAP only returns headers a
// client explicitly asks for, so this list is the contract for
// which signals the rules can rely on.
var headerProps = []string{
	"header:List-Unsubscribe:asText",
	"header:List-Unsubscribe-Post:asText",
	"header:List-Id:asText",
	"header:List-Post:asText",
	"header:Precedence:asText",
	"header:Auto-Submitted:asText",
	"header:X-Auto-Response-Suppress:asText",
	"header:X-Campaign:asText",
	"header:X-Mailchimp-Campaign:asText",
}

// JMAPFetcher implements EmailFetcher against the BFF's JMAP
// InternalClient.
type JMAPFetcher struct {
	client dispatcher
}

// NewJMAPFetcher constructs a fetcher. The client is required.
func NewJMAPFetcher(client *jmap.InternalClient) (*JMAPFetcher, error) {
	if client == nil {
		return nil, errors.New("smartfeatures.NewJMAPFetcher: client is required")
	}
	return &JMAPFetcher{client: client}, nil
}

// FetchMessages issues a single Email/get for the requested ids
// and parses the response into Message values keyed by email id.
// Account-qualified ids ("<account>:<email>") are accepted and
// reduced to the bare email id before the JMAP call, since
// Email/get addresses ids within the resolved account.
func (f *JMAPFetcher) FetchMessages(ctx context.Context, tenantID, kchatUserID string, emailIDs []string) (map[string]Message, error) {
	if len(emailIDs) == 0 {
		return map[string]Message{}, nil
	}

	// Map bare id -> original (possibly qualified) id so the result
	// is keyed exactly as the caller asked.
	bareToOriginal := make(map[string]string, len(emailIDs))
	bare := make([]string, 0, len(emailIDs))
	for _, id := range emailIDs {
		b := id
		if _, e, ok := jmap.SplitQualifiedEmailID(id); ok {
			b = e
		}
		if _, dup := bareToOriginal[b]; dup {
			continue
		}
		bareToOriginal[b] = id
		bare = append(bare, b)
	}

	props := append([]string{
		"id", "threadId", "subject", "preview",
		"from", "to", "cc", "keywords", "receivedAt",
	}, headerProps...)

	req := jmap.JmapRequest{
		Using: []string{jmapCoreCapability, jmapMailCapability},
		MethodCalls: [][]any{
			{"Email/get", map[string]any{
				"ids":        bare,
				"properties": props,
			}, "g0"},
		},
	}
	resp, err := f.client.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		return nil, fmt.Errorf("smartfeatures.FetchMessages: dispatch: %w", err)
	}
	if cerr := resp.FirstCallError(); cerr != nil {
		return nil, fmt.Errorf("smartfeatures.FetchMessages: %w", cerr)
	}
	_, args, ok := resp.CallByID("g0")
	if !ok {
		return nil, errors.New("smartfeatures.FetchMessages: missing Email/get response")
	}
	list, _ := args["list"].([]any)

	out := make(map[string]Message, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		msg := messageFromJMAP(obj)
		key := msg.ID
		if orig, ok := bareToOriginal[msg.ID]; ok {
			key = orig
		}
		out[key] = msg
	}
	return out, nil
}

// messageFromJMAP converts a single Email/get list entry into a
// Message. Missing fields degrade gracefully to zero values so a
// sparse Stalwart response never panics the rule engine.
func messageFromJMAP(obj map[string]any) Message {
	m := Message{
		ID:       asString(obj["id"]),
		ThreadID: asString(obj["threadId"]),
		Subject:  asString(obj["subject"]),
		Preview:  asString(obj["preview"]),
		From:     addressesFromJMAP(obj["from"]),
		To:       addressesFromJMAP(obj["to"]),
		Cc:       addressesFromJMAP(obj["cc"]),
		Keywords: boolMapFromJMAP(obj["keywords"]),
		Headers:  map[string]string{},
	}
	if ra := asString(obj["receivedAt"]); ra != "" {
		if t, err := time.Parse(time.RFC3339, ra); err == nil {
			m.ReceivedAt = t
		}
	}
	// header:Name:asText props come back keyed by the full property
	// string; normalize to the bare header name for Message.Header.
	for k, v := range obj {
		if !strings.HasPrefix(k, "header:") {
			continue
		}
		name := strings.TrimPrefix(k, "header:")
		name = strings.TrimSuffix(name, ":asText")
		if s := strings.TrimSpace(asString(v)); s != "" {
			m.Headers[name] = s
		}
	}
	return m
}

func addressesFromJMAP(v any) []Address {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]Address, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Address{
			Name:  asString(obj["name"]),
			Email: asString(obj["email"]),
		})
	}
	return out
}

func boolMapFromJMAP(v any) map[string]bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]bool, len(obj))
	for k, val := range obj {
		if b, ok := val.(bool); ok {
			out[k] = b
		}
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
