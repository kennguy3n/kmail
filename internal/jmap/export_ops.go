// Package jmap — full-message export operations.
//
// export_ops.go defines the EmailExporter abstraction the
// eDiscovery export runner depends on (gap-closure Session 2). It
// fetches whole RFC 5322 messages (headers, body, and decoded
// attachment parts) for a set of messages so the runner can write
// mbox / eml archives, without the runner having to speak JMAP or
// drive Stalwart's blob-download endpoint itself.
//
// Message IDs are account-qualified exactly as in email_ops.go
// (`"<stalwartAccountID>:<emailID>"`) — see that file's package
// doc for the rationale. The runner obtains qualified IDs from its
// scope query and passes them straight through to FetchFullMessages.
package jmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ExportedMessage is a single fully-hydrated message ready to be
// serialised into an export archive.
type ExportedMessage struct {
	// ID is the account-qualified message ID this row was fetched
	// for (see package doc).
	ID string `json:"id"`
	// BlobID is the JMAP blobId of the whole RFC 5322 message.
	BlobID string `json:"blob_id"`
	// From is the first From address (bare email), used for the
	// mbox "From " separator line.
	From string `json:"from"`
	// Subject is the decoded Subject header (convenience metadata).
	Subject string `json:"subject"`
	// ReceivedAt is the message's receivedAt timestamp; zero if
	// Stalwart did not return one.
	ReceivedAt time.Time `json:"received_at"`
	// Raw is the complete RFC 5322 message (headers + blank line +
	// body) as downloaded from Stalwart — what the mbox / eml
	// writers serialise.
	Raw []byte `json:"-"`
	// Headers is the raw header block (everything up to, but not
	// including, the first blank line) split out of Raw.
	Headers []byte `json:"-"`
	// Body is the raw body (everything after the first blank line)
	// split out of Raw.
	Body []byte `json:"-"`
	// Attachments holds the decoded bytes of each body part marked
	// `disposition: "attachment"` that carries a blobId.
	Attachments [][]byte `json:"-"`
}

// EmailExporter fetches whole messages by account-qualified ID.
// Implementations are safe for concurrent use.
type EmailExporter interface {
	// FetchFullMessages returns one ExportedMessage per resolvable
	// input ID. IDs that no longer exist on the server are skipped
	// (not an error) so a long-running export tolerates concurrent
	// deletion; the caller compares counts. Results are returned in
	// the input order, minus any skipped IDs.
	FetchFullMessages(ctx context.Context, tenantID string, messageIDs []string) ([]ExportedMessage, error)
}

// StalwartEmailExporter implements EmailExporter against Stalwart
// via the shard-aware InternalClient (Email/get + Blob/download).
type StalwartEmailExporter struct {
	client *InternalClient
	pool   *pgxpool.Pool
	logger *log.Logger

	// accountsFn maps the tenant's accounts to their kchat user IDs
	// (needed for Dispatch / DownloadBlob identity headers).
	// Defaults to the pool-backed query; overridable in tests.
	accountsFn func(ctx context.Context, tenantID string) ([]tenantAccount, error)
}

var _ EmailExporter = (*StalwartEmailExporter)(nil)

// NewStalwartEmailExporter wires the exporter. `client` and `pool`
// are required; a nil `logger` falls back to log.Default().
func NewStalwartEmailExporter(client *InternalClient, pool *pgxpool.Pool, logger *log.Logger) (*StalwartEmailExporter, error) {
	if client == nil {
		return nil, errors.New("jmap.NewStalwartEmailExporter: client is required")
	}
	if pool == nil {
		return nil, errors.New("jmap.NewStalwartEmailExporter: pool is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	ex := &StalwartEmailExporter{client: client, pool: pool, logger: logger}
	// Reuse the operator's account-enumeration query so both
	// abstractions resolve (account → kchat user) identically.
	op := &StalwartEmailOperator{client: client, pool: pool, logger: logger}
	ex.accountsFn = op.queryTenantAccounts
	return ex, nil
}

// emailGetProperties is the property set FetchFullMessages requests.
// `bodyStructure` (shaped by exportBodyProperties) lets us walk the
// MIME tree for attachment blobIds; `blobId` is the whole-message
// blob we download for the raw RFC 5322 bytes.
var emailGetProperties = []string{
	"id", "blobId", "receivedAt", "subject", "from", "bodyStructure",
}

// exportBodyProperties is the EmailBodyPart property subset returned
// for every node of `bodyStructure`, enough to identify attachment
// parts and fetch their blobs.
var exportBodyProperties = []string{
	"partId", "blobId", "type", "name", "size", "disposition",
}

// FetchFullMessages implements EmailExporter.
func (e *StalwartEmailExporter) FetchFullMessages(ctx context.Context, tenantID string, messageIDs []string) ([]ExportedMessage, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("jmap.FetchFullMessages: tenantID is required")
	}
	if len(messageIDs) == 0 {
		return nil, nil
	}

	// Group qualified IDs by owning account, preserving first-seen
	// account order; keep the input order to re-sort results.
	byAccount := make(map[string][]string)
	accountOrder := make([]string, 0)
	for _, q := range messageIDs {
		acct, emailID, ok := SplitQualifiedEmailID(q)
		if !ok {
			return nil, fmt.Errorf("jmap.FetchFullMessages: malformed qualified id %q (want <accountID>:<emailID>)", q)
		}
		if _, seen := byAccount[acct]; !seen {
			accountOrder = append(accountOrder, acct)
		}
		byAccount[acct] = append(byAccount[acct], emailID)
	}

	accts, err := e.accountsFn(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("enumerate tenant accounts: %w", err)
	}
	userByAccount := make(map[string]string, len(accts))
	for _, a := range accts {
		userByAccount[a.accountID] = a.kchatUserID
	}

	// Hydrate into a map keyed by qualified ID so we can re-emit in
	// the caller's input order.
	hydrated := make(map[string]ExportedMessage, len(messageIDs))
	for _, acct := range accountOrder {
		kchatUserID, ok := userByAccount[acct]
		if !ok {
			return nil, fmt.Errorf("jmap.FetchFullMessages: account %s is not an active account of tenant %s", acct, tenantID)
		}
		emailIDs := byAccount[acct]

		emailGet := map[string]any{
			"accountId":      acct,
			"ids":            emailIDs,
			"properties":     emailGetProperties,
			"bodyProperties": exportBodyProperties,
		}
		req := JmapRequest{
			Using:       []string{jmapCoreCapability, jmapMailCapability},
			MethodCalls: [][]any{{"Email/get", emailGet, "g0"}},
		}
		resp, err := e.client.Dispatch(ctx, tenantID, kchatUserID, req)
		if err != nil {
			return nil, fmt.Errorf("Email/get account %s: %w", acct, err)
		}
		list, err := parseEmailGetList(resp, "g0")
		if err != nil {
			return nil, fmt.Errorf("parse Email/get account %s: %w", acct, err)
		}
		for _, em := range list {
			msg, err := e.hydrateMessage(ctx, tenantID, kchatUserID, acct, em)
			if err != nil {
				return nil, err
			}
			hydrated[msg.ID] = msg
		}
	}

	out := make([]ExportedMessage, 0, len(hydrated))
	for _, q := range messageIDs {
		if msg, ok := hydrated[q]; ok {
			out = append(out, msg)
		}
	}
	return out, nil
}

// hydrateMessage downloads the raw message blob (and any attachment
// blobs) for one Email/get list entry and assembles an
// ExportedMessage.
func (e *StalwartEmailExporter) hydrateMessage(ctx context.Context, tenantID, kchatUserID, accountID string, em map[string]any) (ExportedMessage, error) {
	emailID, _ := em["id"].(string)
	blobID, _ := em["blobId"].(string)
	if emailID == "" || blobID == "" {
		return ExportedMessage{}, fmt.Errorf("Email/get entry missing id/blobId (account %s)", accountID)
	}

	msg := ExportedMessage{
		ID:      QualifyEmailID(accountID, emailID),
		BlobID:  blobID,
		Subject: stringField(em, "subject"),
		From:    firstFromAddress(em),
	}
	if recv := stringField(em, "receivedAt"); recv != "" {
		if t, err := time.Parse(time.RFC3339, recv); err == nil {
			msg.ReceivedAt = t.UTC()
		}
	}

	raw, err := e.client.DownloadBlob(ctx, tenantID, kchatUserID, accountID, blobID, "message.eml")
	if err != nil {
		return ExportedMessage{}, fmt.Errorf("download message blob %s (account %s): %w", blobID, accountID, err)
	}
	msg.Raw = raw
	msg.Headers, msg.Body = splitHeadersBody(raw)

	for _, attBlobID := range attachmentBlobIDs(em["bodyStructure"]) {
		att, err := e.client.DownloadBlob(ctx, tenantID, kchatUserID, accountID, attBlobID, "attachment")
		if err != nil {
			return ExportedMessage{}, fmt.Errorf("download attachment blob %s (account %s): %w", attBlobID, accountID, err)
		}
		msg.Attachments = append(msg.Attachments, att)
	}
	return msg, nil
}

// parseEmailGetList returns the `list` entries of an Email/get
// response as maps, addressed by method-call ID.
func parseEmailGetList(resp *JmapResponse, callID string) ([]map[string]any, error) {
	name, args, ok := resp.CallByID(callID)
	if !ok {
		return nil, fmt.Errorf("missing Email/get response (%s)", callID)
	}
	if name != "Email/get" {
		return nil, fmt.Errorf("unexpected response method for %s: %q", callID, name)
	}
	rawList, _ := args["list"].([]any)
	out := make([]map[string]any, 0, len(rawList))
	for _, entry := range rawList {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// stringField reads a string property, tolerating absence/null.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// firstFromAddress extracts the first From address's bare email
// from a JMAP `from` property (array of {name, email}).
func firstFromAddress(em map[string]any) string {
	arr, _ := em["from"].([]any)
	for _, v := range arr {
		addr, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if email, _ := addr["email"].(string); email != "" {
			return email
		}
	}
	return ""
}

// attachmentBlobIDs walks a JMAP `bodyStructure` tree (EmailBodyPart
// + nested `subParts`) and returns the blobIds of every part whose
// disposition is "attachment". Order is depth-first, matching the
// MIME document order.
func attachmentBlobIDs(bodyStructure any) []string {
	var out []string
	var walk func(node any)
	walk = func(node any) {
		part, ok := node.(map[string]any)
		if !ok {
			return
		}
		disposition, _ := part["disposition"].(string)
		blobID, _ := part["blobId"].(string)
		if strings.EqualFold(disposition, "attachment") && blobID != "" {
			out = append(out, blobID)
		}
		if sub, ok := part["subParts"].([]any); ok {
			for _, child := range sub {
				walk(child)
			}
		}
	}
	walk(bodyStructure)
	return out
}

// splitHeadersBody splits a raw RFC 5322 message into its header
// block and body at the first blank line. It handles both CRLF and
// bare-LF separators (Stalwart emits CRLF, but defensive parsing
// keeps the export robust against re-encoded fixtures). When no
// blank line is found the whole input is treated as headers and the
// body is empty.
func splitHeadersBody(raw []byte) (headers, body []byte) {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i], raw[i+4:]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i], raw[i+2:]
	}
	return raw, nil
}
