package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/jmap"
)

// --- fakes -----------------------------------------------------------------

type fakeQuerier struct {
	ids        []string
	gotMailbox string
	gotOlder   time.Time
	gotLimit   int
	err        error
}

func (f *fakeQuerier) QueryEmailsByDate(_ context.Context, _ string, mailboxID string, olderThan time.Time, limit int) ([]string, error) {
	f.gotMailbox = mailboxID
	f.gotOlder = olderThan
	f.gotLimit = limit
	return f.ids, f.err
}

type fakeExporter struct {
	byID       map[string]jmap.ExportedMessage
	batchSizes []int
	calls      int
}

func (f *fakeExporter) FetchFullMessages(_ context.Context, _ string, ids []string) ([]jmap.ExportedMessage, error) {
	f.calls++
	f.batchSizes = append(f.batchSizes, len(ids))
	out := make([]jmap.ExportedMessage, 0, len(ids))
	for _, id := range ids {
		if m, ok := f.byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

type fakeUploader struct {
	gotBody     []byte
	gotSize     int64
	gotFilename string
	presigned   *jmap.Presigned
}

func (f *fakeUploader) UploadLargeAttachment(_ context.Context, _, filename, _ string, body io.Reader, size int64) (*jmap.Presigned, error) {
	b, _ := io.ReadAll(body)
	f.gotBody = b
	f.gotSize = size
	f.gotFilename = filename
	if f.presigned != nil {
		return f.presigned, nil
	}
	return &jmap.Presigned{ID: "att-123", URL: "https://fabric.example/get/att-123"}, nil
}

type fakeAuditQuerier struct {
	entries []audit.Entry
	gotF    audit.QueryFilters
	calls   int
}

// Query honours Offset/Limit so the runner's pagination loop is
// exercised exactly as it would be against the real audit service.
func (f *fakeAuditQuerier) Query(_ context.Context, _ string, ff audit.QueryFilters) ([]audit.Entry, error) {
	f.gotF = ff
	f.calls++
	start := ff.Offset
	if start > len(f.entries) {
		start = len(f.entries)
	}
	end := len(f.entries)
	if ff.Limit > 0 && start+ff.Limit < end {
		end = start + ff.Limit
	}
	return append([]audit.Entry(nil), f.entries[start:end]...), nil
}

// --- helpers ---------------------------------------------------------------

func msg(id, from, subject string, received time.Time, raw string) jmap.ExportedMessage {
	return jmap.ExportedMessage{ID: id, From: from, Subject: subject, ReceivedAt: received, Raw: []byte(raw)}
}

func untar(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		files[hdr.Name] = b
	}
	return files
}

func newRunner(t *testing.T, cfg JMAPExportRunnerConfig) *JMAPExportRunner {
	t.Helper()
	r, err := NewJMAPExportRunner(cfg)
	if err != nil {
		t.Fatalf("NewJMAPExportRunner: %v", err)
	}
	return r
}

// --- tests -----------------------------------------------------------------

func TestNewJMAPExportRunner_RequiresDeps(t *testing.T) {
	t.Parallel()
	if _, err := NewJMAPExportRunner(JMAPExportRunnerConfig{Querier: &fakeQuerier{}}); err == nil {
		t.Error("expected error when Exporter is nil")
	}
	if _, err := NewJMAPExportRunner(JMAPExportRunnerConfig{Exporter: &fakeExporter{}}); err == nil {
		t.Error("expected error when Querier is nil")
	}
}

func TestRun_MboxArchive(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q := &fakeQuerier{ids: []string{"acct:1", "acct:2"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:1": msg("acct:1", "a@x", "One", now, "Subject: One\r\n\r\nbody one\r\n"),
		"acct:2": msg("acct:2", "b@x", "Two", now, "Subject: Two\r\n\r\nbody two\r\n"),
	}}
	up := &fakeUploader{}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up})

	res, err := r.Run(context.Background(), Job{ID: "job1", TenantID: "t1", Format: FormatMbox, Scope: ScopeAll})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	files := untar(t, up.gotBody)
	mbox, ok := files["mailbox.mbox"]
	if !ok {
		t.Fatalf("mailbox.mbox missing; files=%v", keys(files))
	}
	if !bytes.Contains(mbox, []byte("body one")) || !bytes.Contains(mbox, []byte("body two")) {
		t.Errorf("mbox missing message bodies: %q", mbox)
	}
	if strings.Count(string(mbox), "From a@x ")+strings.Count(string(mbox), "From b@x ") != 2 {
		t.Errorf("expected two mbox separator lines: %q", mbox)
	}

	// Result metadata: checksum + size must match the uploaded bytes.
	sum := sha256.Sum256(up.gotBody)
	if res.ArtifactChecksum != hex.EncodeToString(sum[:]) {
		t.Errorf("checksum mismatch: got %s", res.ArtifactChecksum)
	}
	if res.ArtifactSizeBytes != int64(len(up.gotBody)) || up.gotSize != int64(len(up.gotBody)) {
		t.Errorf("size mismatch: res=%d up=%d len=%d", res.ArtifactSizeBytes, up.gotSize, len(up.gotBody))
	}
	if res.DownloadURL != "https://fabric.example/get/att-123" {
		t.Errorf("download url: %s", res.DownloadURL)
	}
	if res.ArtifactURL != "kmail-attachment:att-123" {
		t.Errorf("artifact url: %s", res.ArtifactURL)
	}
	if len(res.MessageIDs) != 2 {
		t.Errorf("expected 2 message ids, got %v", res.MessageIDs)
	}
	if up.gotFilename != "kmail-export-t1-job1.tar.gz" {
		t.Errorf("filename: %s", up.gotFilename)
	}
}

func TestRun_EMLArchive(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{ids: []string{"acct:a/b"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:a/b": msg("acct:a/b", "a@x", "S", time.Now(), "raw-eml-bytes"),
	}}
	up := &fakeUploader{}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := untar(t, up.gotBody)
	// "/" in the id must be sanitised in the entry path.
	want := "mail/acct:a_b.eml"
	if got, ok := files[want]; !ok || string(got) != "raw-eml-bytes" {
		t.Fatalf("eml entry missing/incorrect; want %q in %v", want, keys(files))
	}
}

func TestRun_PSTStubManifest(t *testing.T) {
	t.Parallel()
	now := time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC)
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:1": msg("acct:1", "a@x", "Subj", now, "raw"),
	}}
	up := &fakeUploader{}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatPSTStub, Scope: ScopeAll}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := untar(t, up.gotBody)
	body, ok := files["manifest.json"]
	if !ok {
		t.Fatalf("manifest.json missing: %v", keys(files))
	}
	var doc pstStubManifest
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if doc.MessageCount != 1 || len(doc.Messages) != 1 || doc.Messages[0].Subject != "Subj" {
		t.Errorf("manifest contents wrong: %+v", doc)
	}
	if _, ok := files["README.txt"]; !ok {
		t.Errorf("README.txt missing")
	}
}

func TestRun_BatchesOf100(t *testing.T) {
	t.Parallel()
	ids := make([]string, 250)
	byID := map[string]jmap.ExportedMessage{}
	for i := range ids {
		id := fmt.Sprintf("acct:%d", i)
		ids[i] = id
		byID[id] = msg(id, "a@x", "s", time.Now(), "raw")
	}
	q := &fakeQuerier{ids: ids}
	ex := &fakeExporter{byID: byID}
	up := &fakeUploader{}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up})

	res, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantBatches := []int{100, 100, 50}
	if fmt.Sprint(ex.batchSizes) != fmt.Sprint(wantBatches) {
		t.Errorf("batch sizes = %v, want %v", ex.batchSizes, wantBatches)
	}
	if len(res.MessageIDs) != 250 {
		t.Errorf("expected 250 ids, got %d", len(res.MessageIDs))
	}
}

func TestRun_MailboxScopePassesMailboxID(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{"acct:1": msg("acct:1", "a@x", "s", time.Now(), "raw")}}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: &fakeUploader{}})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeMailbox, ScopeRef: "mbx-7"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if q.gotMailbox != "mbx-7" {
		t.Errorf("mailbox id not forwarded: %q", q.gotMailbox)
	}
}

func TestRun_MailboxScopeRequiresRef(t *testing.T) {
	t.Parallel()
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: &fakeExporter{}, Querier: &fakeQuerier{}, Uploader: &fakeUploader{}})
	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeMailbox}); err == nil {
		t.Error("expected error for mailbox scope without scope_ref")
	}
}

func TestRun_DateRangeFilters(t *testing.T) {
	t.Parallel()
	// before-bound is passed to the querier; the after-bound is
	// applied per-message by receivedAt. The querier returns three
	// ids; only the middle one is inside [2024-02-01, 2024-02-29].
	inRange := time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC)
	tooOld := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	zeroDate := time.Time{} // unknown received: excluded by a lower bound
	q := &fakeQuerier{ids: []string{"acct:old", "acct:in", "acct:zero"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:old":  msg("acct:old", "a@x", "old", tooOld, "raw-old"),
		"acct:in":   msg("acct:in", "a@x", "in", inRange, "raw-in"),
		"acct:zero": msg("acct:zero", "a@x", "zero", zeroDate, "raw-zero"),
	}}
	up := &fakeUploader{}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up})

	res, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeDateRange, ScopeRef: "2024-02-01..2024-02-28"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.MessageIDs) != 1 || res.MessageIDs[0] != "acct:in" {
		t.Fatalf("date_range filter wrong: %v", res.MessageIDs)
	}
	// Querier received the exclusive upper bound (end day + 1).
	// 2024 is a leap year, so 2024-02-28 + 1 day = 2024-02-29.
	wantBefore := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	if !q.gotOlder.Equal(wantBefore) {
		t.Errorf("querier olderThan = %v, want %v", q.gotOlder, wantBefore)
	}
	files := untar(t, up.gotBody)
	if _, ok := files["mail/acct:in.eml"]; !ok {
		t.Errorf("in-range message missing from archive: %v", keys(files))
	}
	if _, ok := files["mail/acct:old.eml"]; ok {
		t.Errorf("out-of-range message wrongly included")
	}
}

func TestRun_BadDateRange(t *testing.T) {
	t.Parallel()
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: &fakeExporter{}, Querier: &fakeQuerier{}, Uploader: &fakeUploader{}})
	for _, ref := range []string{"", "2024-01-01", "bad..worse", "2024-02-01..2024-01-01"} {
		if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeDateRange, ScopeRef: ref}); err == nil {
			t.Errorf("expected error for scope_ref %q", ref)
		}
	}
}

func TestRun_AppendsAuditJSON(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{"acct:1": msg("acct:1", "a@x", "s", time.Now(), "raw")}}
	up := &fakeUploader{}
	aud := &fakeAuditQuerier{entries: []audit.Entry{{Action: "mail.read", ResourceType: "email"}}}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up, Audit: aud})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := untar(t, up.gotBody)
	body, ok := files["audit.json"]
	if !ok {
		t.Fatalf("audit.json missing: %v", keys(files))
	}
	var entries []audit.Entry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("audit json: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "mail.read" {
		t.Errorf("audit entries wrong: %+v", entries)
	}
}

func TestRun_EMLDedupesCollidingNames(t *testing.T) {
	t.Parallel()
	// "acct:a b" and "acct:a/b" both sanitise to "acct:a_b": the
	// archive must keep both messages under distinct entry paths
	// rather than silently overwriting one with the other.
	now := time.Now()
	q := &fakeQuerier{ids: []string{"acct:a b", "acct:a/b"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{
		"acct:a b": msg("acct:a b", "a@x", "S", now, "raw-space"),
		"acct:a/b": msg("acct:a/b", "a@x", "S", now, "raw-slash"),
	}}
	up := &fakeUploader{}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up})

	res, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.MessageIDs) != 2 {
		t.Fatalf("expected both messages included, got %v", res.MessageIDs)
	}
	files := untar(t, up.gotBody)
	first, ok1 := files["mail/acct:a_b.eml"]
	second, ok2 := files["mail/acct:a_b-2.eml"]
	if !ok1 || !ok2 {
		t.Fatalf("expected two distinct eml entries, got %v", keys(files))
	}
	// Both payloads must survive — neither clobbered the other.
	got := string(first) + "|" + string(second)
	if !strings.Contains(got, "raw-space") || !strings.Contains(got, "raw-slash") {
		t.Errorf("colliding entries lost a payload: %q", got)
	}
}

func TestRun_AuditPaginatesBeyondPageSize(t *testing.T) {
	t.Parallel()
	// A trail larger than the per-call cap (auditPageSize) must be
	// fully exported, not silently truncated at the first page.
	const total = auditPageSize*2 + 500
	entries := make([]audit.Entry, total)
	for i := range entries {
		entries[i] = audit.Entry{Action: fmt.Sprintf("a%d", i), ResourceType: "email"}
	}
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{"acct:1": msg("acct:1", "a@x", "s", time.Now(), "raw")}}
	up := &fakeUploader{}
	aud := &fakeAuditQuerier{entries: entries}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q, Uploader: up, Audit: aud})

	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatEML, Scope: ScopeAll}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := untar(t, up.gotBody)
	var got []audit.Entry
	if err := json.Unmarshal(files["audit.json"], &got); err != nil {
		t.Fatalf("audit json: %v", err)
	}
	if len(got) != total {
		t.Errorf("exported %d audit entries, want %d (silent truncation?)", len(got), total)
	}
	if aud.calls != 3 {
		t.Errorf("expected 3 paged queries, got %d", aud.calls)
	}
}

func TestRun_NoUploaderProducesDataURL(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{ids: []string{"acct:1"}}
	ex := &fakeExporter{byID: map[string]jmap.ExportedMessage{"acct:1": msg("acct:1", "a@x", "s", time.Now(), "raw")}}
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: ex, Querier: q}) // no uploader

	res, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: FormatMbox, Scope: ScopeAll})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(res.DownloadURL, "data:application/gzip;") {
		t.Errorf("expected data url, got %q", res.DownloadURL)
	}
	if res.ArtifactSizeBytes == 0 || res.ArtifactChecksum == "" {
		t.Errorf("artifact metadata not populated: %+v", res)
	}
}

func TestRun_UnsupportedFormat(t *testing.T) {
	t.Parallel()
	r := newRunner(t, JMAPExportRunnerConfig{Exporter: &fakeExporter{}, Querier: &fakeQuerier{}, Uploader: &fakeUploader{}})
	if _, err := r.Run(context.Background(), Job{ID: "j", TenantID: "t", Format: "zip", Scope: ScopeAll}); err == nil {
		t.Error("expected error for unsupported format")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
