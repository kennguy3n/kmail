// Package export — JMAP eDiscovery export runner.
//
// JMAPExportRunner performs the real fan-out the Phase 5 stub
// promised. For a given export job it:
//
//   - resolves the job's scope (all / mailbox / date_range) into a
//     set of account-qualified JMAP message IDs via the Session 0
//     EmailOperator.QueryEmailsByDate scope query;
//   - fetches the full RFC 5322 messages in batches via the
//     Session 0 jmap.EmailExporter;
//   - serialises them into the requested format (mbox / eml /
//     pst_stub) inside a tar.gz archive, optionally appending the
//     tenant's audit trail (audit.json) and calendar (.ics);
//   - streams the archive to the tenant's zk-object-fabric bucket;
//   - returns a Result carrying the download URL, the canonical
//     artifact reference, the archive size, its SHA-256 checksum,
//     and the exact set of message IDs included.
//
// The runner performs NO database writes: persisting the artifact
// columns + the export_job_messages join rows + the audit-log entry
// is the Service's job (it owns the pool and the RLS transaction).
// Keeping the runner pure makes it trivially unit-testable with a
// fake exporter and uploader.
package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/jmap"
)

// Supported export formats. They mirror the `format` CHECK
// constraint on `export_jobs` (migrations/001_baseline.sql).
const (
	FormatMbox    = "mbox"
	FormatEML     = "eml"
	FormatPSTStub = "pst_stub"
)

// Supported scopes. They mirror the `scope` CHECK constraint on
// `export_jobs`.
const (
	ScopeAll       = "all"
	ScopeMailbox   = "mailbox"
	ScopeDateRange = "date_range"
)

// fetchBatchSize is how many message IDs are passed to one
// EmailExporter.FetchFullMessages call. The exporter batches its
// own JMAP Email/get internally too, but chunking here bounds the
// number of fully-hydrated messages (raw blobs + attachments) held
// in memory at once for a large export.
const fetchBatchSize = 100

// defaultMaxMessages caps how many message IDs a single export
// enumerates. The Session 0 QueryEmailsByDate scope query is not
// offset-paginated, so enumeration is a single bounded call; this
// is the bound. Overridable via WithMaxMessages.
const defaultMaxMessages = 100000

// EmailQuerier resolves an export scope into account-qualified
// message IDs. Satisfied by *jmap.StalwartEmailOperator
// (QueryEmailsByDate). Defined here so the runner depends on a
// narrow surface and tests can inject a fake.
type EmailQuerier interface {
	QueryEmailsByDate(ctx context.Context, tenantID, mailboxID string, olderThan time.Time, limit int) ([]string, error)
}

// CalendarClient is the optional subset of calendarbridge.Service
// the runner uses to fold a tenant's calendars into the archive.
type CalendarClient interface {
	ListCalendars(ctx context.Context, accountID string) ([]calendarbridge.Calendar, error)
	GetEvents(ctx context.Context, accountID, calendarID string, r calendarbridge.TimeRange) ([]calendarbridge.Event, error)
}

// AuditQuerier is the optional subset of audit.Service used to
// append the tenant's audit trail to the archive.
type AuditQuerier interface {
	Query(ctx context.Context, tenantID string, f audit.QueryFilters) ([]audit.Entry, error)
}

// Uploader streams the packaged archive to zk-object-fabric and
// returns a presigned download URL. The signature matches
// *jmap.AttachmentService.UploadLargeAttachment so production wiring
// passes the attachment service directly.
type Uploader interface {
	UploadLargeAttachment(ctx context.Context, tenantID, filename, contentType string, body io.Reader, size int64) (*jmap.Presigned, error)
}

// JMAPExportRunnerConfig wires the runner's dependencies. Exporter
// and Querier are required; the rest are optional (a nil Uploader
// yields an inline data: URL for dev, a nil Calendar/Audit simply
// omits that section from the archive).
type JMAPExportRunnerConfig struct {
	Exporter    jmap.EmailExporter
	Querier     EmailQuerier
	Uploader    Uploader
	Calendar    CalendarClient
	Audit       AuditQuerier
	Logger      *log.Logger
	MaxMessages int
}

// JMAPExportRunner is the production Runner.
type JMAPExportRunner struct {
	exporter    jmap.EmailExporter
	querier     EmailQuerier
	uploader    Uploader
	calendar    CalendarClient
	audit       AuditQuerier
	logger      *log.Logger
	maxMessages int
}

var _ Runner = (*JMAPExportRunner)(nil)

// NewJMAPExportRunner builds the runner. It returns an error when a
// required dependency is missing so misconfiguration fails fast at
// wiring time rather than on the first job.
func NewJMAPExportRunner(cfg JMAPExportRunnerConfig) (*JMAPExportRunner, error) {
	if cfg.Exporter == nil {
		return nil, errors.New("export.NewJMAPExportRunner: Exporter is required")
	}
	if cfg.Querier == nil {
		return nil, errors.New("export.NewJMAPExportRunner: Querier is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 {
		maxMessages = defaultMaxMessages
	}
	return &JMAPExportRunner{
		exporter:    cfg.Exporter,
		querier:     cfg.Querier,
		uploader:    cfg.Uploader,
		calendar:    cfg.Calendar,
		audit:       cfg.Audit,
		logger:      logger,
		maxMessages: maxMessages,
	}, nil
}

// Run implements Runner.
func (r *JMAPExportRunner) Run(ctx context.Context, job Job) (Result, error) {
	format := job.Format
	if format == "" {
		format = FormatMbox
	}
	switch format {
	case FormatMbox, FormatEML, FormatPSTStub:
	default:
		return Result{}, fmt.Errorf("export: unsupported format %q", format)
	}

	ids, after, before, err := r.resolveScope(ctx, job)
	if err != nil {
		return Result{}, err
	}

	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)

	included, err := r.writeMail(ctx, tw, job, format, ids, after, before)
	if err != nil {
		return Result{}, fmt.Errorf("export mail: %w", err)
	}
	if r.calendar != nil {
		if err := r.writeCalendars(ctx, tw, job, after, before); err != nil {
			return Result{}, fmt.Errorf("export calendar: %w", err)
		}
	}
	if r.audit != nil {
		if err := r.writeAudit(ctx, tw, job, after, before); err != nil {
			return Result{}, fmt.Errorf("export audit: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return Result{}, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Result{}, fmt.Errorf("close gzip: %w", err)
	}

	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	res := Result{
		ArtifactSizeBytes: int64(len(archive)),
		ArtifactChecksum:  hex.EncodeToString(sum[:]),
		MessageIDs:        included,
	}

	filename := fmt.Sprintf("kmail-export-%s-%s.tar.gz", job.TenantID, job.ID)
	if r.uploader == nil {
		// Dev fallback: no object store wired. Surface size + name
		// so the job still completes with a meaningful, inert URL.
		res.DownloadURL = fmt.Sprintf("data:application/gzip;size=%d;name=%s", len(archive), filename)
		res.ArtifactURL = res.DownloadURL
		return res, nil
	}
	signed, err := r.uploader.UploadLargeAttachment(ctx, job.TenantID, filename, "application/gzip", bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return Result{}, fmt.Errorf("upload archive: %w", err)
	}
	res.DownloadURL = signed.URL
	// artifact_url is the canonical, stable reference the admin UI
	// re-presigns from (the short-lived signed.URL expires). The
	// attachment_links row id is that stable handle; fall back to
	// the signed URL only when the uploader did not persist a row.
	if signed.ID != "" {
		res.ArtifactURL = "kmail-attachment:" + signed.ID
	} else {
		res.ArtifactURL = signed.URL
	}
	return res, nil
}

// resolveScope turns a job's scope into the set of account-qualified
// message IDs to export plus, for date_range, the [after, before)
// window used to bound the calendar/audit sections and to
// post-filter messages by receivedAt.
func (r *JMAPExportRunner) resolveScope(ctx context.Context, job Job) (ids []string, after, before time.Time, err error) {
	switch job.Scope {
	case "", ScopeAll:
		ids, err = r.querier.QueryEmailsByDate(ctx, job.TenantID, "", maxQueryTime(), r.maxMessages)
	case ScopeMailbox:
		if strings.TrimSpace(job.ScopeRef) == "" {
			return nil, after, before, errors.New("export: mailbox scope requires scope_ref (mailbox id)")
		}
		ids, err = r.querier.QueryEmailsByDate(ctx, job.TenantID, job.ScopeRef, maxQueryTime(), r.maxMessages)
	case ScopeDateRange:
		after, before, err = parseDateRange(job.ScopeRef)
		if err != nil {
			return nil, after, before, err
		}
		// QueryEmailsByDate filters on `before` (olderThan); the
		// lower bound is applied per-message after fetch via
		// receivedAt, since the scope query has no `after` filter.
		ids, err = r.querier.QueryEmailsByDate(ctx, job.TenantID, "", before, r.maxMessages)
	default:
		return nil, after, before, fmt.Errorf("export: unsupported scope %q", job.Scope)
	}
	if err != nil {
		return nil, after, before, fmt.Errorf("scope query: %w", err)
	}
	if len(ids) >= r.maxMessages {
		r.logger.Printf("export: job %s hit the %d-message enumeration cap; archive may be truncated", job.ID, r.maxMessages)
	}
	return ids, after, before, nil
}

// writeMail fetches the messages in batches and serialises them in
// the requested format, returning the IDs actually included (after
// any date_range lower-bound filtering).
func (r *JMAPExportRunner) writeMail(ctx context.Context, tw *tar.Writer, job Job, format string, ids []string, after, before time.Time) ([]string, error) {
	var (
		mbox     bytes.Buffer
		mw       = NewMboxWriter(&mbox)
		manifest []manifestEntry
		included = make([]string, 0, len(ids))
	)

	for start := 0; start < len(ids); start += fetchBatchSize {
		end := min(start+fetchBatchSize, len(ids))
		batch := ids[start:end]

		msgs, err := r.exporter.FetchFullMessages(ctx, job.TenantID, batch)
		if err != nil {
			return nil, fmt.Errorf("fetch messages: %w", err)
		}
		for _, msg := range msgs {
			if !inDateRange(msg.ReceivedAt, after, before) {
				continue
			}
			included = append(included, msg.ID)
			switch format {
			case FormatMbox:
				if err := mw.WriteMessage(msg.From, msg.ReceivedAt, msg.Raw); err != nil {
					return nil, fmt.Errorf("write mbox message %s: %w", msg.ID, err)
				}
			case FormatEML:
				if err := writeTarFile(tw, "mail/"+sanitize(msg.ID)+".eml", msg.Raw); err != nil {
					return nil, err
				}
			case FormatPSTStub:
				manifest = append(manifest, manifestEntry{
					ID:         msg.ID,
					From:       msg.From,
					Subject:    msg.Subject,
					ReceivedAt: msg.ReceivedAt,
					SizeBytes:  len(msg.Raw),
				})
			}
		}
	}

	switch format {
	case FormatMbox:
		return included, writeTarFile(tw, "mailbox.mbox", mbox.Bytes())
	case FormatPSTStub:
		return included, writePSTStub(tw, job, manifest)
	default: // eml entries already streamed
		return included, nil
	}
}

// manifestEntry is one row of the pst_stub manifest.
type manifestEntry struct {
	ID         string    `json:"id"`
	From       string    `json:"from"`
	Subject    string    `json:"subject"`
	ReceivedAt time.Time `json:"received_at"`
	SizeBytes  int       `json:"size_bytes"`
}

// pstStubManifest is the top-level pst_stub document.
type pstStubManifest struct {
	Format       string          `json:"format"`
	Note         string          `json:"note"`
	TenantID     string          `json:"tenant_id"`
	JobID        string          `json:"job_id"`
	GeneratedAt  time.Time       `json:"generated_at"`
	MessageCount int             `json:"message_count"`
	Messages     []manifestEntry `json:"messages"`
}

// pstStubNote documents why a real PST is not produced.
const pstStubNote = "PST (MS Outlook .pst) conversion is intentionally out of scope: " +
	"it requires the proprietary MS-PST (MAPI) container format, whose faithful " +
	"generation needs a Windows/Outlook or libpff toolchain not available in this " +
	"service. This manifest enumerates every message that WOULD be included so an " +
	"external PST conversion step (or the eml export) can reproduce the set."

func writePSTStub(tw *tar.Writer, job Job, entries []manifestEntry) error {
	if entries == nil {
		entries = []manifestEntry{}
	}
	doc := pstStubManifest{
		Format:       FormatPSTStub,
		Note:         pstStubNote,
		TenantID:     job.TenantID,
		JobID:        job.ID,
		GeneratedAt:  time.Now().UTC(),
		MessageCount: len(entries),
		Messages:     entries,
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, "manifest.json", body); err != nil {
		return err
	}
	return writeTarFile(tw, "README.txt", []byte(pstStubNote+"\n"))
}

func (r *JMAPExportRunner) writeCalendars(ctx context.Context, tw *tar.Writer, job Job, after, before time.Time) error {
	cals, err := r.calendar.ListCalendars(ctx, job.TenantID)
	if err != nil {
		return err
	}
	tr := calendarbridge.TimeRange{Start: after, End: before}
	for _, cal := range cals {
		evs, err := r.calendar.GetEvents(ctx, job.TenantID, cal.ID, tr)
		if err != nil {
			r.logger.Printf("export: calendar %s events: %v (skipping)", cal.ID, err)
			continue
		}
		var ics bytes.Buffer
		ics.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//KMail//Export//EN\r\n")
		for _, ev := range evs {
			if v := extractVEvent(ev.ICalData); v != "" {
				ics.WriteString(v)
				ics.WriteString("\r\n")
			}
		}
		ics.WriteString("END:VCALENDAR\r\n")
		if err := writeTarFile(tw, "calendar/"+sanitize(cal.ID)+".ics", ics.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (r *JMAPExportRunner) writeAudit(ctx context.Context, tw *tar.Writer, job Job, after, before time.Time) error {
	f := audit.QueryFilters{Limit: 1000}
	if !after.IsZero() {
		f.Since = after
	}
	if !before.IsZero() {
		f.Until = before
	}
	entries, err := r.audit.Query(ctx, job.TenantID, f)
	if err != nil {
		return err
	}
	if entries == nil {
		entries = []audit.Entry{}
	}
	body, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeTarFile(tw, "audit.json", body)
}

// --- helpers ---------------------------------------------------------------

// maxQueryTime returns a far-future bound for the "before" filter
// so the "all" / "mailbox" scopes enumerate every message (mail is
// never received in the future).
func maxQueryTime() time.Time {
	return time.Now().UTC().AddDate(1000, 0, 0)
}

// inDateRange reports whether t falls within [after, before). Zero
// bounds are treated as open-ended. The exporter may return a zero
// receivedAt (server omitted it); such a message is included only
// when there is no lower bound, so an unknown date is never silently
// dropped from an unbounded export but is excluded from a strict
// date_range window.
func inDateRange(t, after, before time.Time) bool {
	if !after.IsZero() {
		if t.IsZero() || t.Before(after) {
			return false
		}
	}
	if !before.IsZero() && !t.IsZero() && !t.Before(before) {
		return false
	}
	return true
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

// parseDateRange parses a "YYYY-MM-DD..YYYY-MM-DD" scope_ref into a
// half-open [after, before) window. `before` is the day AFTER the
// end date so the end day is fully included.
func parseDateRange(ref string) (after, before time.Time, err error) {
	parts := strings.SplitN(ref, "..", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, errors.New("export: scope_ref must be 'YYYY-MM-DD..YYYY-MM-DD'")
	}
	a, err := time.Parse("2006-01-02", strings.TrimSpace(parts[0]))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("export: invalid range start: %w", err)
	}
	b, err := time.Parse("2006-01-02", strings.TrimSpace(parts[1]))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("export: invalid range end: %w", err)
	}
	if b.Before(a) {
		return time.Time{}, time.Time{}, errors.New("export: range end is before range start")
	}
	return a.UTC(), b.AddDate(0, 0, 1).UTC(), nil
}

func extractVEvent(ical string) string {
	start := strings.Index(ical, "BEGIN:VEVENT")
	end := strings.Index(ical, "END:VEVENT")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return ical[start : end+len("END:VEVENT")]
}

// sanitize makes an arbitrary id safe to use as a tar entry path
// component (no separators, no traversal, no spaces).
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}
