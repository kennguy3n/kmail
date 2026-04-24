package deliverability

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// DMARCReport is the API representation of a stored DMARC aggregate
// report.
type DMARCReport struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenant_id"`
	DomainID       string          `json:"domain_id,omitempty"`
	ReportID       string          `json:"report_id"`
	OrgName        string          `json:"org_name"`
	Email          string          `json:"email"`
	DateRangeBegin time.Time       `json:"date_range_begin"`
	DateRangeEnd   time.Time       `json:"date_range_end"`
	Domain         string          `json:"domain"`
	ADKIM          string          `json:"adkim"`
	ASPF           string          `json:"aspf"`
	Policy         string          `json:"policy"`
	PassCount      int64           `json:"pass_count"`
	FailCount      int64           `json:"fail_count"`
	Records        json.RawMessage `json:"records"`
	CreatedAt      time.Time       `json:"created_at"`
}

// aggregateReport mirrors the RFC 7489 aggregate DMARC XML shape.
type aggregateReport struct {
	XMLName         xml.Name `xml:"feedback"`
	ReportMetadata  struct {
		OrgName   string `xml:"org_name"`
		Email     string `xml:"email"`
		ReportID  string `xml:"report_id"`
		DateRange struct {
			Begin int64 `xml:"begin"`
			End   int64 `xml:"end"`
		} `xml:"date_range"`
	} `xml:"report_metadata"`
	PolicyPublished struct {
		Domain string `xml:"domain"`
		ADKIM  string `xml:"adkim"`
		ASPF   string `xml:"aspf"`
		P      string `xml:"p"`
	} `xml:"policy_published"`
	Records []struct {
		Row struct {
			SourceIP        string `xml:"source_ip"`
			Count           int64  `xml:"count"`
			PolicyEvaluated struct {
				Disposition string `xml:"disposition"`
				DKIM        string `xml:"dkim"`
				SPF         string `xml:"spf"`
			} `xml:"policy_evaluated"`
		} `xml:"row"`
		Identifiers struct {
			HeaderFrom string `xml:"header_from"`
		} `xml:"identifiers"`
		AuthResults struct {
			DKIM []struct {
				Domain string `xml:"domain"`
				Result string `xml:"result"`
			} `xml:"dkim"`
			SPF []struct {
				Domain string `xml:"domain"`
				Result string `xml:"result"`
			} `xml:"spf"`
		} `xml:"auth_results"`
	} `xml:"record"`
}

// DMARCRecord is the flattened per-source-IP summary persisted in
// `dmarc_reports.records`.
type DMARCRecord struct {
	SourceIP    string `json:"source_ip"`
	Count       int64  `json:"count"`
	Disposition string `json:"disposition"`
	DKIMResult  string `json:"dkim_result"`
	SPFResult   string `json:"spf_result"`
	HeaderFrom  string `json:"header_from"`
}

// DMARCService owns the `dmarc_reports` table.
type DMARCService struct {
	pool *pgxpool.Pool
}

// IngestReport parses `xmlData` as an RFC 7489 aggregate DMARC XML
// report and persists the resulting row under `tenantID`. The
// tenant's domain is resolved via the `domains` table so per-domain
// summaries can join back.
func (s *DMARCService) IngestReport(ctx context.Context, tenantID string, xmlData []byte) (*DMARCReport, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	var agg aggregateReport
	if err := xml.Unmarshal(xmlData, &agg); err != nil {
		return nil, fmt.Errorf("%w: parse DMARC XML: %v", ErrInvalidInput, err)
	}
	if agg.PolicyPublished.Domain == "" {
		return nil, fmt.Errorf("%w: DMARC report missing policy_published.domain", ErrInvalidInput)
	}

	records := make([]DMARCRecord, 0, len(agg.Records))
	var pass, fail int64
	for _, r := range agg.Records {
		records = append(records, DMARCRecord{
			SourceIP:    r.Row.SourceIP,
			Count:       r.Row.Count,
			Disposition: r.Row.PolicyEvaluated.Disposition,
			DKIMResult:  r.Row.PolicyEvaluated.DKIM,
			SPFResult:   r.Row.PolicyEvaluated.SPF,
			HeaderFrom:  r.Identifiers.HeaderFrom,
		})
		if r.Row.PolicyEvaluated.DKIM == "pass" || r.Row.PolicyEvaluated.SPF == "pass" {
			pass += r.Row.Count
		} else {
			fail += r.Row.Count
		}
	}
	recordsJSON, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	begin := time.Unix(agg.ReportMetadata.DateRange.Begin, 0).UTC()
	end := time.Unix(agg.ReportMetadata.DateRange.End, 0).UTC()

	if s.pool == nil {
		return nil, fmt.Errorf("no pool configured")
	}
	var out DMARCReport
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Resolve domain_id best-effort; null is fine if the tenant
		// hasn't registered the reporting domain yet.
		var domainID *string
		var did string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM domains
			WHERE tenant_id = $1::uuid AND domain = $2
		`, tenantID, agg.PolicyPublished.Domain).Scan(&did)
		if err == nil {
			domainID = &did
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO dmarc_reports (
				tenant_id, domain_id, report_id, org_name, email,
				date_range_begin, date_range_end, domain,
				adkim, aspf, policy, pass_count, fail_count,
				records, raw_xml
			) VALUES (
				$1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8,
				$9, $10, $11, $12, $13, $14::jsonb, $15
			)
			RETURNING id::text, tenant_id::text, COALESCE(domain_id::text, ''),
			          report_id, org_name, email,
			          date_range_begin, date_range_end, domain,
			          adkim, aspf, policy, pass_count, fail_count,
			          records, created_at
		`,
			tenantID, domainID, agg.ReportMetadata.ReportID,
			agg.ReportMetadata.OrgName, agg.ReportMetadata.Email,
			begin, end, agg.PolicyPublished.Domain,
			agg.PolicyPublished.ADKIM, agg.PolicyPublished.ASPF,
			agg.PolicyPublished.P, pass, fail,
			string(recordsJSON), string(xmlData),
		).Scan(
			&out.ID, &out.TenantID, &out.DomainID, &out.ReportID,
			&out.OrgName, &out.Email, &out.DateRangeBegin, &out.DateRangeEnd,
			&out.Domain, &out.ADKIM, &out.ASPF, &out.Policy,
			&out.PassCount, &out.FailCount, &out.Records, &out.CreatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert dmarc_report: %w", err)
	}
	return &out, nil
}

// ListReports returns a paginated list of DMARC reports.
func (s *DMARCService) ListReports(ctx context.Context, tenantID, domainID string, limit, offset int) ([]DMARCReport, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []DMARCReport
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var rows pgx.Rows
		var err error
		if domainID != "" {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, COALESCE(domain_id::text, ''),
				       report_id, org_name, email,
				       date_range_begin, date_range_end, domain,
				       adkim, aspf, policy, pass_count, fail_count,
				       records, created_at
				FROM dmarc_reports
				WHERE tenant_id = $1::uuid AND domain_id = $2::uuid
				ORDER BY date_range_begin DESC
				LIMIT $3 OFFSET $4
			`, tenantID, domainID, limit, offset)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id::text, tenant_id::text, COALESCE(domain_id::text, ''),
				       report_id, org_name, email,
				       date_range_begin, date_range_end, domain,
				       adkim, aspf, policy, pass_count, fail_count,
				       records, created_at
				FROM dmarc_reports
				WHERE tenant_id = $1::uuid
				ORDER BY date_range_begin DESC
				LIMIT $2 OFFSET $3
			`, tenantID, limit, offset)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r DMARCReport
			if err := rows.Scan(
				&r.ID, &r.TenantID, &r.DomainID, &r.ReportID,
				&r.OrgName, &r.Email, &r.DateRangeBegin, &r.DateRangeEnd,
				&r.Domain, &r.ADKIM, &r.ASPF, &r.Policy,
				&r.PassCount, &r.FailCount, &r.Records, &r.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list dmarc reports: %w", err)
	}
	return out, nil
}

// DMARCSummary is the per-domain 30-day aggregate returned by
// GetReportSummary.
type DMARCSummary struct {
	TenantID      string  `json:"tenant_id"`
	DomainID      string  `json:"domain_id,omitempty"`
	Domain        string  `json:"domain"`
	PassCount     int64   `json:"pass_count"`
	FailCount     int64   `json:"fail_count"`
	Total         int64   `json:"total"`
	PassRate      float64 `json:"pass_rate"`
	ReportCount   int     `json:"report_count"`
	WindowDays    int     `json:"window_days"`
}

// GetReportSummary returns the 30-day aggregate pass/fail rates for
// the tenant (optionally filtered by domain).
func (s *DMARCService) GetReportSummary(ctx context.Context, tenantID, domainID string) (*DMARCSummary, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return &DMARCSummary{TenantID: tenantID, DomainID: domainID, WindowDays: 30}, nil
	}
	var out DMARCSummary
	out.TenantID = tenantID
	out.DomainID = domainID
	out.WindowDays = 30
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		since := time.Now().Add(-30 * 24 * time.Hour)
		if domainID != "" {
			return tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(pass_count),0)::bigint,
				       COALESCE(SUM(fail_count),0)::bigint,
				       COUNT(*)::int,
				       COALESCE(MIN(domain), '')
				FROM dmarc_reports
				WHERE tenant_id = $1::uuid AND domain_id = $2::uuid
				  AND date_range_begin >= $3
			`, tenantID, domainID, since).Scan(&out.PassCount, &out.FailCount, &out.ReportCount, &out.Domain)
		}
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(pass_count),0)::bigint,
			       COALESCE(SUM(fail_count),0)::bigint,
			       COUNT(*)::int
			FROM dmarc_reports
			WHERE tenant_id = $1::uuid
			  AND date_range_begin >= $2
		`, tenantID, since).Scan(&out.PassCount, &out.FailCount, &out.ReportCount)
	})
	if err != nil {
		return nil, fmt.Errorf("dmarc summary: %w", err)
	}
	out.Total = out.PassCount + out.FailCount
	if out.Total > 0 {
		out.PassRate = float64(out.PassCount) / float64(out.Total)
	}
	return &out, nil
}

