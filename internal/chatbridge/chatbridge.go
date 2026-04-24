// Package chatbridge hosts the Email-to-Chat Bridge business
// logic — the "KMail lives inside KChat" integration surface on
// the mail side.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): share an email to a KChat channel,
// route inbound alert emails to KChat channels via configured
// aliases. Talks to the Stalwart JMAP endpoint (Email/get) and the
// KChat channel-message API.
package chatbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Config wires the Email-to-Chat Bridge.
type Config struct {
	// KChatAPIURL is the base URL for the KChat channel-message
	// API (e.g. `https://kchat.example.com`).
	KChatAPIURL string
	// KChatAPIToken is the service-account bearer token the
	// bridge presents when posting to KChat channels.
	KChatAPIToken string
	// StalwartURL is the base URL for Stalwart's JMAP endpoint
	// (e.g. `http://stalwart:8080`). Used for `Email/get` lookups
	// when formatting a shared-message card.
	StalwartURL string
	// StalwartAuth is the Authorization header value the bridge
	// presents when calling Stalwart JMAP on behalf of the
	// system user. Empty when the caller supplies their own
	// bearer through context (future).
	StalwartAuth string
	// Pool is the control-plane Postgres pool used for alert-route
	// storage.
	Pool *pgxpool.Pool
	// HTTPClient overrides the HTTP client used for both KChat and
	// JMAP calls. Defaults to an http.Client with a 10s timeout.
	HTTPClient *http.Client
	// Logger overrides the logger. Defaults to log.Default().
	Logger *log.Logger
	// KChat is the KChat client interface. When non-nil it is
	// used in place of the default HTTP client — lets tests
	// substitute a fake.
	KChat KChatClient
	// Now overrides time.Now for tests.
	Now func() time.Time
}

// KChatClient is the narrow interface over the KChat
// channel-message API the bridge consumes. Implementations must be
// safe for concurrent use.
type KChatClient interface {
	PostChannelMessage(ctx context.Context, channelID string, msg ChannelMessage) error
}

// ChannelMessage is the KChat message payload. `Attachments` mirrors
// the slack-compatible rich-card shape KChat speaks.
type ChannelMessage struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one card block on a channel message.
type Attachment struct {
	Title   string `json:"title,omitempty"`
	Text    string `json:"text,omitempty"`
	Footer  string `json:"footer,omitempty"`
	TitleLink string `json:"title_link,omitempty"`
}

// Service is the Email-to-Chat Bridge.
type Service struct {
	cfg   Config
	kchat KChatClient
}

// NewService wires a Service. When cfg.KChat is nil a default HTTP
// KChat client is used.
func NewService(cfg Config) *Service {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	k := cfg.KChat
	if k == nil {
		k = &httpKChatClient{cfg: cfg}
	}
	return &Service{cfg: cfg, kchat: k}
}

// Route describes a configured email-alias → KChat-channel route.
type Route struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenantId"`
	AliasAddress string    `json:"aliasAddress"`
	ChannelID    string    `json:"channelId"`
	CreatedAt    time.Time `json:"createdAt"`
}

// ErrInvalidInput wraps caller-visible validation failures. HTTP
// handlers surface these as 400.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when the target route or email cannot be
// located.
var ErrNotFound = errors.New("not found")

// ShareEmailToChannel fetches `emailID` via JMAP `Email/get`, formats
// a rich channel message, and posts it to `channelID`.
func (s *Service) ShareEmailToChannel(ctx context.Context, tenantID, emailID, channelID, userID string) error {
	if tenantID == "" || emailID == "" || channelID == "" {
		return fmt.Errorf("%w: tenantID, emailID, and channelID required", ErrInvalidInput)
	}
	summary, err := s.fetchEmailSummary(ctx, emailID)
	if err != nil {
		return err
	}
	msg := ChannelMessage{
		Text: fmt.Sprintf("Email shared by user %s", userID),
		Attachments: []Attachment{{
			Title:     summary.Subject,
			Text:      summary.Preview,
			Footer:    fmt.Sprintf("from %s", summary.From),
			TitleLink: fmt.Sprintf("/mail/%s/%s", summary.MailboxID, emailID),
		}},
	}
	return s.kchat.PostChannelMessage(ctx, channelID, msg)
}

// ConfigureAlertRoute upserts a tenant-scoped alias → channel
// mapping. Duplicate (tenant, alias) pairs rotate the channel ID.
func (s *Service) ConfigureAlertRoute(ctx context.Context, tenantID, aliasAddress, channelID string) (*Route, error) {
	if tenantID == "" || aliasAddress == "" || channelID == "" {
		return nil, fmt.Errorf("%w: tenantID, aliasAddress, and channelID required", ErrInvalidInput)
	}
	route := &Route{
		TenantID:     tenantID,
		AliasAddress: strings.ToLower(aliasAddress),
		ChannelID:    channelID,
	}
	if s.cfg.Pool == nil {
		route.CreatedAt = s.cfg.Now().UTC()
		return route, nil
	}
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO chat_bridge_routes (tenant_id, alias_address, channel_id)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (tenant_id, alias_address)
			DO UPDATE SET channel_id = EXCLUDED.channel_id
			RETURNING id::text, created_at
		`, tenantID, route.AliasAddress, channelID).Scan(&route.ID, &route.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("upsert route: %w", err)
	}
	return route, nil
}

// ListRoutes returns every alert route for the tenant.
func (s *Service) ListRoutes(ctx context.Context, tenantID string) ([]Route, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil, nil
	}
	var out []Route
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, alias_address, channel_id, created_at
			FROM chat_bridge_routes
			ORDER BY created_at DESC
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Route
			if err := rows.Scan(&r.ID, &r.TenantID, &r.AliasAddress, &r.ChannelID, &r.CreatedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRoute removes a route by ID.
func (s *Service) DeleteRoute(ctx context.Context, tenantID, routeID string) error {
	if tenantID == "" || routeID == "" {
		return fmt.Errorf("%w: tenantID and routeID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM chat_bridge_routes WHERE id = $1::uuid`, routeID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ProcessInboundAlert is called by a Sieve hook or JMAP push
// listener when a message is delivered to an alert-route alias.
// The email is summarised and forwarded to the mapped channel.
func (s *Service) ProcessInboundAlert(ctx context.Context, tenantID, recipient, emailID string) error {
	if tenantID == "" || recipient == "" || emailID == "" {
		return fmt.Errorf("%w: tenantID, recipient, and emailID required", ErrInvalidInput)
	}
	route, err := s.lookupRoute(ctx, tenantID, recipient)
	if err != nil {
		return err
	}
	if route == nil {
		return nil // no route configured — caller decides whether to log
	}
	summary, err := s.fetchEmailSummary(ctx, emailID)
	if err != nil {
		return err
	}
	msg := ChannelMessage{
		Text: fmt.Sprintf("New alert on %s", recipient),
		Attachments: []Attachment{{
			Title:  summary.Subject,
			Text:   summary.Preview,
			Footer: fmt.Sprintf("from %s", summary.From),
		}},
	}
	return s.kchat.PostChannelMessage(ctx, route.ChannelID, msg)
}

func (s *Service) lookupRoute(ctx context.Context, tenantID, alias string) (*Route, error) {
	if s.cfg.Pool == nil {
		return nil, nil
	}
	var r Route
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, alias_address, channel_id, created_at
			FROM chat_bridge_routes
			WHERE alias_address = $1
		`, strings.ToLower(alias)).Scan(&r.ID, &r.TenantID, &r.AliasAddress, &r.ChannelID, &r.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// emailSummary is the subset of the JMAP Email object ShareEmail
// and ProcessInboundAlert care about.
type emailSummary struct {
	Subject   string
	From      string
	Preview   string
	MailboxID string
}

func (s *Service) fetchEmailSummary(ctx context.Context, emailID string) (*emailSummary, error) {
	if s.cfg.StalwartURL == "" {
		return &emailSummary{Subject: "(unknown)", From: "", Preview: "", MailboxID: ""}, nil
	}
	// JMAP Email/get with "preview" + header fields. The request
	// uses `accountId:*` as a placeholder — in production the
	// caller passes the resolved accountId; this first-cut
	// implementation uses the dev fallback.
	reqBody := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{
			{"Email/get", map[string]any{
				"accountId":  "",
				"ids":        []string{emailID},
				"properties": []string{"subject", "from", "preview", "mailboxIds"},
			}, "c1"},
		},
	}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.StalwartURL, "/")+"/jmap/api", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.StalwartAuth != "" {
		req.Header.Set("Authorization", s.cfg.StalwartAuth)
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JMAP Email/get: HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.MethodResponses) == 0 || len(envelope.MethodResponses[0]) < 2 {
		return &emailSummary{}, nil
	}
	var args struct {
		List []struct {
			Subject    string            `json:"subject"`
			Preview    string            `json:"preview"`
			MailboxIDs map[string]bool   `json:"mailboxIds"`
			From       []struct{ Email string } `json:"from"`
		} `json:"list"`
	}
	if err := json.Unmarshal(envelope.MethodResponses[0][1], &args); err != nil {
		return nil, err
	}
	sum := &emailSummary{}
	if len(args.List) > 0 {
		sum.Subject = args.List[0].Subject
		sum.Preview = args.List[0].Preview
		if len(args.List[0].From) > 0 {
			sum.From = args.List[0].From[0].Email
		}
		for mid := range args.List[0].MailboxIDs {
			sum.MailboxID = mid
			break
		}
	}
	return sum, nil
}

// ------------------------------------------------------------------
// HTTP KChat client
// ------------------------------------------------------------------

type httpKChatClient struct {
	cfg Config
}

func (h *httpKChatClient) PostChannelMessage(ctx context.Context, channelID string, msg ChannelMessage) error {
	if h.cfg.KChatAPIURL == "" {
		// In dev / tests without KChat, drop the message but don't
		// fail the caller.
		h.cfg.Logger.Printf("chatbridge: KChat not configured; dropping message for %s", channelID)
		return nil
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/v1/channels/%s/messages", strings.TrimRight(h.cfg.KChatAPIURL, "/"), channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.cfg.KChatAPIToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.KChatAPIToken)
	}
	resp, err := h.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("kchat: HTTP %d: %s", resp.StatusCode, string(snippet))
	}
	return nil
}
