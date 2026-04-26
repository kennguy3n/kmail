// Package export — RealRunner.
//
// The RealRunner does the actual fan-out the Phase 5 stub
// promised: it pulls emails from JMAP, calendar events from
// CalDAV, audit log entries from `audit.Service`, packages the
// result, uploads it to the tenant's zk-object-fabric bucket, and
// returns a presigned URL.
package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/jmap"
)

// JMAPClient is the subset of JMAP the runner needs. Defined as
// an interface so unit tests can inject a fake.
type JMAPClient interface {
	QueryEmails(ctx context.Context, accountID string, after, before time.Time) ([]string, error)
	GetEmailRaw(ctx context.Context, accountID, emailID string) ([]byte, error)
}

// CalendarClient is the subset of calendarbridge.Service the runner
// needs.
type CalendarClient interface {
	ListCalendars(ctx context.Context, accountID string) ([]calendarbridge.Calendar, error)
	GetEvents(ctx context.Context, accountID, calendarID string, r calendarbridge.TimeRange) ([]calendarbridge.Event, error)
}

// AuditClient is the subset of audit.Service.
type AuditClient interface {
	Query(ctx context.Context, tenantID string, f audit.QueryFilters) ([]audit.Entry, error)
}

// Uploader uploads the packaged archive and returns a download URL.
type Uploader interface {
	UploadLargeAttachment(ctx context.Context, tenantID, filename, contentType string, body io.Reader, size int64) (*jmap.Presigned, error)
}

// RealRunnerConfig wires the dependencies.
type RealRunnerConfig struct {
	JMAP     JMAPClient
	Calendar CalendarClient
	Audit    AuditClient
	Uploader Uploader
}

// NewRealRunner returns a Runner that performs the real fan-out.
// Any nil dependency is silently skipped — e.g. in dev with no
// JMAP wired the archive only contains the audit log.
func NewRealRunner(cfg RealRunnerConfig) Runner {
	return func(ctx context.Context, job Job) (string, error) {
		buf := &bytes.Buffer{}
		gz := gzip.NewWriter(buf)
		tw := tar.NewWriter(gz)

		var (
			after, before time.Time
		)
		if job.Scope == "date_range" && job.ScopeRef != "" {
			a, b, err := parseDateRange(job.ScopeRef)
			if err == nil {
				after, before = a, b
			}
		}

		if cfg.JMAP != nil {
			if err := writeMailboxToTar(ctx, tw, cfg.JMAP, job, after, before); err != nil {
				return "", fmt.Errorf("export mailbox: %w", err)
			}
		}
		if cfg.Calendar != nil {
			if err := writeCalendarsToTar(ctx, tw, cfg.Calendar, job, after, before); err != nil {
				return "", fmt.Errorf("export calendar: %w", err)
			}
		}
		if cfg.Audit != nil {
			if err := writeAuditToTar(ctx, tw, cfg.Audit, job, after, before); err != nil {
				return "", fmt.Errorf("export audit: %w", err)
			}
		}

		if err := tw.Close(); err != nil {
			return "", err
		}
		if err := gz.Close(); err != nil {
			return "", err
		}

		filename := fmt.Sprintf("kmail-export-%s-%s.tar.gz", job.TenantID, job.ID)
		if cfg.Uploader == nil {
			return fmt.Sprintf("data:application/gzip;size=%d;name=%s", buf.Len(), filename), nil
		}
		signed, err := cfg.Uploader.UploadLargeAttachment(ctx, job.TenantID, filename, "application/gzip", bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		if err != nil {
			return "", err
		}
		return signed.URL, nil
	}
}

func writeMailboxToTar(ctx context.Context, tw *tar.Writer, j JMAPClient, job Job, after, before time.Time) error {
	ids, err := j.QueryEmails(ctx, job.TenantID, after, before)
	if err != nil {
		return err
	}
	if job.Format == "mbox" {
		var mbox bytes.Buffer
		for _, id := range ids {
			raw, err := j.GetEmailRaw(ctx, job.TenantID, id)
			if err != nil {
				continue
			}
			fmt.Fprintf(&mbox, "From kmail-export %s\n", time.Now().UTC().Format(time.RFC1123Z))
			mbox.Write(raw)
			mbox.WriteString("\n")
		}
		return writeTarFile(tw, "mailbox.mbox", mbox.Bytes())
	}
	for _, id := range ids {
		raw, err := j.GetEmailRaw(ctx, job.TenantID, id)
		if err != nil {
			continue
		}
		if err := writeTarFile(tw, fmt.Sprintf("mail/%s.eml", id), raw); err != nil {
			return err
		}
	}
	return nil
}

func writeCalendarsToTar(ctx context.Context, tw *tar.Writer, c CalendarClient, job Job, after, before time.Time) error {
	cals, err := c.ListCalendars(ctx, job.TenantID)
	if err != nil {
		return err
	}
	tr := calendarbridge.TimeRange{}
	if !after.IsZero() {
		tr.Start = after
	}
	if !before.IsZero() {
		tr.End = before
	}
	for _, cal := range cals {
		evs, err := c.GetEvents(ctx, job.TenantID, cal.ID, tr)
		if err != nil {
			continue
		}
		var ics bytes.Buffer
		ics.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//KMail//Export//EN\r\n")
		for _, ev := range evs {
			ics.WriteString(extractVEvent(ev.ICalData))
			ics.WriteString("\r\n")
		}
		ics.WriteString("END:VCALENDAR\r\n")
		name := fmt.Sprintf("calendar/%s.ics", sanitize(cal.ID))
		if err := writeTarFile(tw, name, ics.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func writeAuditToTar(ctx context.Context, tw *tar.Writer, a AuditClient, job Job, after, before time.Time) error {
	f := audit.QueryFilters{Limit: 5000}
	if !after.IsZero() {
		f.Since = after
	}
	if !before.IsZero() {
		f.Until = before
	}
	entries, err := a.Query(ctx, job.TenantID, f)
	if err != nil {
		return err
	}
	buf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeTarFile(tw, "audit_log.json", buf)
}

func writeTarFile(tw *tar.Writer, name string, body []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: time.Now().UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

func parseDateRange(ref string) (time.Time, time.Time, error) {
	parts := strings.SplitN(ref, "..", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, errors.New("export: scope_ref must be 'YYYY-MM-DD..YYYY-MM-DD'")
	}
	a, err := time.Parse("2006-01-02", parts[0])
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	b, err := time.Parse("2006-01-02", parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return a, b.AddDate(0, 0, 1), nil
}

func extractVEvent(ical string) string {
	start := strings.Index(ical, "BEGIN:VEVENT")
	end := strings.Index(ical, "END:VEVENT")
	if start < 0 || end < 0 {
		return ""
	}
	return ical[start : end+len("END:VEVENT")]
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// HTTPJMAPClient is the production JMAPClient. It speaks JMAP to
// Stalwart for `Email/query` + `Email/get` and downloads raw RFC
// 5322 blobs via the JMAP `download` endpoint.
type HTTPJMAPClient struct {
	StalwartURL string
	Auth        string
	HTTP        *http.Client
}

// NewHTTPJMAPClient returns a HTTPJMAPClient with a default 30s
// timeout.
func NewHTTPJMAPClient(stalwartURL, auth string) *HTTPJMAPClient {
	return &HTTPJMAPClient{
		StalwartURL: stalwartURL,
		Auth:        auth,
		HTTP:        &http.Client{Timeout: 30 * time.Second},
	}
}

// QueryEmails enumerates email IDs.
func (c *HTTPJMAPClient) QueryEmails(ctx context.Context, accountID string, after, before time.Time) ([]string, error) {
	filter := map[string]any{}
	if !after.IsZero() {
		filter["after"] = after.UTC().Format(time.RFC3339)
	}
	if !before.IsZero() {
		filter["before"] = before.UTC().Format(time.RFC3339)
	}
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{{
			"Email/query", map[string]any{
				"accountId": accountID,
				"filter":    filter,
				"limit":     1000,
			}, "c1",
		}},
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := c.post(ctx, "/jmap/api", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.MethodResponses) == 0 || len(resp.MethodResponses[0]) < 2 {
		return nil, nil
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &args); err != nil {
		return nil, err
	}
	return args.IDs, nil
}

// GetEmailRaw downloads the RFC 5322 blob for an email.
func (c *HTTPJMAPClient) GetEmailRaw(ctx context.Context, accountID, emailID string) ([]byte, error) {
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{{
			"Email/get", map[string]any{
				"accountId":  accountID,
				"ids":        []string{emailID},
				"properties": []string{"blobId"},
			}, "c1",
		}},
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := c.post(ctx, "/jmap/api", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.MethodResponses) == 0 || len(resp.MethodResponses[0]) < 2 {
		return nil, nil
	}
	var args struct {
		List []struct {
			BlobID string `json:"blobId"`
		} `json:"list"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &args); err != nil {
		return nil, err
	}
	if len(args.List) == 0 {
		return nil, nil
	}
	dlURL := fmt.Sprintf("%s/jmap/download/%s/%s/%s.eml",
		strings.TrimRight(c.StalwartURL, "/"), accountID, args.List[0].BlobID, emailID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, err
	}
	if c.Auth != "" {
		req.Header.Set("Authorization", c.Auth)
	}
	resp2, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 300 {
		return nil, fmt.Errorf("jmap download HTTP %d", resp2.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp2.Body, 50<<20))
}

func (c *HTTPJMAPClient) post(ctx context.Context, path string, payload any, out any) error {
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.StalwartURL, "/")+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Auth != "" {
		req.Header.Set("Authorization", c.Auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("jmap HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return json.Unmarshal(raw, out)
}
