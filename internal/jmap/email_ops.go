// Package jmap — email lifecycle operations.
//
// email_ops.go defines the EmailOperator abstraction the retention
// enforcement worker depends on (gap-closure Session 1). It lets a
// background worker destroy or page through a tenant's mail without
// reaching into the JMAP wire protocol directly, while the concrete
// StalwartEmailOperator proxies the real `Email/query` /
// `Email/set` calls through the existing shard-aware InternalClient.
//
// # Account-qualified message IDs
//
// JMAP object IDs are unique only WITHIN an account (RFC 8620 §1.2),
// and a single tenant fans out across many Stalwart accounts (one
// per user / shared inbox). A bare email ID is therefore ambiguous
// at the tenant level: the same string can name different messages
// in two accounts, so destroying by a bare ID could delete the
// wrong message in the wrong account.
//
// To keep the tenant-scoped `[]string` signatures the worker wants
// while staying correct, every ID that crosses this boundary is
// "account-qualified": `"<stalwartAccountID>:<emailID>"`. JMAP IDs
// only contain `[A-Za-z0-9_-]` (RFC 8620 §1.2), so ":" is an
// unambiguous separator. QueryEmailsByDate returns qualified IDs;
// DestroyEmails consumes them. Callers treat the strings as opaque
// and pass the query output straight into destroy.
package jmap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// EmailOperator is the mailbox-mutation surface the retention
// enforcer drives. Implementations are safe for concurrent use.
type EmailOperator interface {
	// DestroyEmails permanently deletes the given account-qualified
	// message IDs (see package doc) for the tenant. It is
	// idempotent: an ID that no longer exists is treated as already
	// destroyed, not an error. A nil/empty slice is a no-op.
	DestroyEmails(ctx context.Context, tenantID string, messageIDs []string) error

	// QueryEmailsByDate returns up to `limit` account-qualified IDs
	// of messages received strictly before `olderThan`, oldest
	// first, across the tenant's accounts. When `mailboxID` is
	// non-empty it scopes the query to that JMAP mailbox
	// (`inMailbox`). The enforcer pages by repeatedly querying and
	// destroying: because destroyed messages drop out of the
	// `receivedBefore` window, the next call returns the next batch.
	QueryEmailsByDate(ctx context.Context, tenantID, mailboxID string, olderThan time.Time, limit int) ([]string, error)
}

// JMAP capability URNs sent in the `using` array of every request
// these operators issue (RFC 8620 §2 + RFC 8621 §1).
const (
	jmapCoreCapability = "urn:ietf:params:jmap:core"
	jmapMailCapability = "urn:ietf:params:jmap:mail"
)

// qualifiedIDSeparator joins a Stalwart account ID and a JMAP email
// ID into a single tenant-scoped token. Safe because JMAP IDs never
// contain ":" (RFC 8620 §1.2).
const qualifiedIDSeparator = ":"

// QualifyEmailID builds the account-qualified ID used across the
// EmailOperator / EmailExporter boundary.
func QualifyEmailID(accountID, emailID string) string {
	return accountID + qualifiedIDSeparator + emailID
}

// SplitQualifiedEmailID reverses QualifyEmailID. `ok` is false when
// the token is not in `<accountID>:<emailID>` form (either part
// empty).
func SplitQualifiedEmailID(qualified string) (accountID, emailID string, ok bool) {
	i := strings.Index(qualified, qualifiedIDSeparator)
	if i <= 0 || i >= len(qualified)-1 {
		return "", "", false
	}
	return qualified[:i], qualified[i+1:], true
}

// Default batch sizes. DestroyEmails chunks JMAP `Email/set` destroy
// arrays so a single round-trip stays well under Stalwart's request
// limits; QueryEmailsByDate clamps a non-positive limit to a sane
// page size.
const (
	destroyBatchSize  = 100
	defaultQueryLimit = 500
)

// tenantAccount is one (kchatUserID, stalwartAccountID) pair for an
// active mailbox in a tenant.
type tenantAccount struct {
	kchatUserID string
	accountID   string
}

// StalwartEmailOperator implements EmailOperator against Stalwart
// via the shard-aware InternalClient.
type StalwartEmailOperator struct {
	client *InternalClient
	pool   *pgxpool.Pool
	logger *log.Logger

	// accountsFn enumerates the tenant's active mailbox accounts.
	// It defaults to the pool-backed query (queryTenantAccounts)
	// and is overridable so the JMAP request-shaping tests run
	// without a database.
	accountsFn func(ctx context.Context, tenantID string) ([]tenantAccount, error)
}

var _ EmailOperator = (*StalwartEmailOperator)(nil)

// NewStalwartEmailOperator wires the operator. `client` and `pool`
// are required; a nil `logger` falls back to log.Default().
func NewStalwartEmailOperator(client *InternalClient, pool *pgxpool.Pool, logger *log.Logger) (*StalwartEmailOperator, error) {
	if client == nil {
		return nil, errors.New("jmap.NewStalwartEmailOperator: client is required")
	}
	if pool == nil {
		return nil, errors.New("jmap.NewStalwartEmailOperator: pool is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	op := &StalwartEmailOperator{client: client, pool: pool, logger: logger}
	op.accountsFn = op.queryTenantAccounts
	return op, nil
}

// queryTenantAccounts loads the active user / shared-inbox accounts
// for a tenant inside an RLS-scoped transaction (matching the
// proxy's account-resolution path). `service` accounts are excluded
// — they hold no user mail subject to retention.
func (o *StalwartEmailOperator) queryTenantAccounts(ctx context.Context, tenantID string) ([]tenantAccount, error) {
	var accts []tenantAccount
	err := pgx.BeginFunc(ctx, o.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return fmt.Errorf("set tenant GUC: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT kchat_user_id, stalwart_account_id
			  FROM users
			 WHERE tenant_id = $1::uuid
			   AND status = 'active'
			   AND account_type IN ('user', 'shared_inbox')
			 ORDER BY created_at
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a tenantAccount
			if err := rows.Scan(&a.kchatUserID, &a.accountID); err != nil {
				return err
			}
			accts = append(accts, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return accts, nil
}

// QueryEmailsByDate implements EmailOperator.
func (o *StalwartEmailOperator) QueryEmailsByDate(ctx context.Context, tenantID, mailboxID string, olderThan time.Time, limit int) ([]string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("jmap.QueryEmailsByDate: tenantID is required")
	}
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	accts, err := o.accountsFn(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("enumerate tenant accounts: %w", err)
	}

	out := make([]string, 0, limit)
	for _, a := range accts {
		if len(out) >= limit {
			break
		}
		remaining := limit - len(out)

		filter := map[string]any{
			// JMAP UTCDate: RFC 3339 in UTC with a "Z" suffix and no
			// fractional seconds (RFC 8620 §1.4).
			"before": olderThan.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if mailboxID != "" {
			filter["inMailbox"] = mailboxID
		}
		emailQuery := map[string]any{
			"accountId": a.accountID,
			"filter":    filter,
			"sort": []map[string]any{
				{"property": "receivedAt", "isAscending": true},
			},
			"position":       0,
			"limit":          remaining,
			"calculateTotal": false,
		}
		req := JmapRequest{
			Using:       []string{jmapCoreCapability, jmapMailCapability},
			MethodCalls: [][]any{{"Email/query", emailQuery, "q0"}},
		}
		resp, err := o.client.Dispatch(ctx, tenantID, a.kchatUserID, req)
		if err != nil {
			return nil, fmt.Errorf("Email/query account %s: %w", a.accountID, err)
		}
		ids, err := parseEmailQueryIDs(resp, "q0")
		if err != nil {
			return nil, fmt.Errorf("parse Email/query account %s: %w", a.accountID, err)
		}
		for _, id := range ids {
			out = append(out, QualifyEmailID(a.accountID, id))
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// DestroyEmails implements EmailOperator.
func (o *StalwartEmailOperator) DestroyEmails(ctx context.Context, tenantID string, messageIDs []string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("jmap.DestroyEmails: tenantID is required")
	}
	if len(messageIDs) == 0 {
		return nil
	}

	// Group the qualified IDs back by owning account so each
	// Email/set destroy hits exactly the account that owns the IDs.
	byAccount := make(map[string][]string)
	order := make([]string, 0)
	for _, q := range messageIDs {
		acct, emailID, ok := SplitQualifiedEmailID(q)
		if !ok {
			return fmt.Errorf("jmap.DestroyEmails: malformed qualified id %q (want <accountID>:<emailID>)", q)
		}
		if _, seen := byAccount[acct]; !seen {
			order = append(order, acct)
		}
		byAccount[acct] = append(byAccount[acct], emailID)
	}

	accts, err := o.accountsFn(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("enumerate tenant accounts: %w", err)
	}
	userByAccount := make(map[string]string, len(accts))
	for _, a := range accts {
		userByAccount[a.accountID] = a.kchatUserID
	}

	for _, acct := range order {
		kchatUserID, ok := userByAccount[acct]
		if !ok {
			return fmt.Errorf("jmap.DestroyEmails: account %s is not an active account of tenant %s", acct, tenantID)
		}
		ids := byAccount[acct]
		for start := 0; start < len(ids); start += destroyBatchSize {
			end := min(start+destroyBatchSize, len(ids))
			batch := ids[start:end]

			emailSet := map[string]any{
				"accountId": acct,
				"destroy":   batch,
			}
			req := JmapRequest{
				Using:       []string{jmapCoreCapability, jmapMailCapability},
				MethodCalls: [][]any{{"Email/set", emailSet, "d0"}},
			}
			resp, err := o.client.Dispatch(ctx, tenantID, kchatUserID, req)
			if err != nil {
				return fmt.Errorf("Email/set destroy account %s: %w", acct, err)
			}
			if err := checkEmailSetDestroy(resp, "d0"); err != nil {
				return fmt.Errorf("Email/set destroy account %s: %w", acct, err)
			}
		}
	}
	return nil
}

// parseEmailQueryIDs pulls the `ids` array out of an `Email/query`
// response addressed by method-call ID. Dispatch has already
// surfaced any JMAP method-level error, so a non-error response of
// the wrong method name is the only remaining anomaly to guard.
func parseEmailQueryIDs(resp *JmapResponse, callID string) ([]string, error) {
	name, args, ok := resp.CallByID(callID)
	if !ok {
		return nil, fmt.Errorf("missing Email/query response (%s)", callID)
	}
	if name != "Email/query" {
		return nil, fmt.Errorf("unexpected response method for %s: %q", callID, name)
	}
	raw, _ := args["ids"].([]any)
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			ids = append(ids, s)
		}
	}
	return ids, nil
}

// checkEmailSetDestroy inspects an `Email/set` response and returns
// an error only for "hard" destroy failures. A `notFound`
// SetError means the message was already gone (idempotent destroy)
// and is ignored; any other SetError type (e.g. "forbidden",
// "serverFail") is surfaced so the enforcement run is recorded as
// failed rather than silently under-deleting.
func checkEmailSetDestroy(resp *JmapResponse, callID string) error {
	name, args, ok := resp.CallByID(callID)
	if !ok {
		return fmt.Errorf("missing Email/set response (%s)", callID)
	}
	if name != "Email/set" {
		return fmt.Errorf("unexpected response method for %s: %q", callID, name)
	}
	notDestroyed, _ := args["notDestroyed"].(map[string]any)
	for id, v := range notDestroyed {
		setErr, _ := v.(map[string]any)
		typ, _ := setErr["type"].(string)
		if typ == "notFound" {
			continue
		}
		desc, _ := setErr["description"].(string)
		if desc != "" {
			return fmt.Errorf("destroy %s failed: %s: %s", id, typ, desc)
		}
		return fmt.Errorf("destroy %s failed: %s", id, typ)
	}
	return nil
}
