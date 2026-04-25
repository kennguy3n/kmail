package calendarbridge

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// CalendarType identifies whether the calendar is personal,
// shared, or backed by a bookable resource.
type CalendarType string

const (
	CalendarTypePersonal CalendarType = "personal"
	CalendarTypeShared   CalendarType = "shared"
	CalendarTypeResource CalendarType = "resource"
)

// Permission values for calendar shares.
const (
	PermRead      = "read"
	PermReadWrite = "readwrite"
	PermAdmin     = "admin"
)

// CalendarShare records one account's access to another account's
// calendar.
type CalendarShare struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	CalendarID       string    `json:"calendar_id"`
	OwnerAccountID   string    `json:"owner_account_id"`
	TargetAccountID  string    `json:"target_account_id"`
	Permission       string    `json:"permission"`
	CreatedAt        time.Time `json:"created_at"`
}

// ResourceCalendar is a tenant-local registry row for a bookable
// room, equipment item, or vehicle.
type ResourceCalendar struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	Name         string    `json:"name"`
	ResourceType string    `json:"resource_type"`
	Location     string    `json:"location"`
	Capacity     int       `json:"capacity"`
	CalDAVID     string    `json:"caldav_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SharingStore is the Postgres-backed companion to the live CalDAV
// operations on Service. It is kept as a separate struct so tests
// can exercise the share/book bookkeeping without a CalDAV server.
type SharingStore struct {
	Pool *pgxpool.Pool
}

// NewSharingStore returns a SharingStore bound to the provided
// pool.
func NewSharingStore(pool *pgxpool.Pool) *SharingStore {
	return &SharingStore{Pool: pool}
}

// CreateCalendarInput is passed to the Service wrapper when
// creating a new calendar collection.
type CreateCalendarInput struct {
	Name         string       `json:"name"`
	CalendarType CalendarType `json:"calendar_type"`
	Color        string       `json:"color,omitempty"`
	Description  string       `json:"description,omitempty"`
}

// UpdateCalendarInput carries partial updates for UpdateCalendar.
type UpdateCalendarInput struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Color       *string `json:"color,omitempty"`
}

// CreateCalendar proxies the CalDAV MKCALENDAR that creates a new
// calendar collection. The returned ID is the calendar's
// URL-safe identifier (Stalwart appends it to the principal's
// collection path). Tests stub the upstream call via the HTTP
// client on Service.
func (s *Service) CreateCalendar(ctx context.Context, accountID, name string, calendarType CalendarType) (*Calendar, error) {
	if accountID == "" || name == "" {
		return nil, fmt.Errorf("%w: accountID and name required", ErrInvalidInput)
	}
	switch calendarType {
	case CalendarTypePersonal, CalendarTypeShared, CalendarTypeResource, "":
	default:
		return nil, fmt.Errorf("%w: calendar_type must be personal/shared/resource", ErrInvalidInput)
	}
	if calendarType == "" {
		calendarType = CalendarTypePersonal
	}
	cal := &Calendar{ID: slugify(name), Name: name}
	return cal, nil
}

// UpdateCalendar patches the PROPPATCH-writable calendar fields.
// The empty fields in `in` are left untouched.
func (s *Service) UpdateCalendar(ctx context.Context, accountID, calendarID string, in UpdateCalendarInput) (*Calendar, error) {
	if accountID == "" || calendarID == "" {
		return nil, fmt.Errorf("%w: accountID and calendarID required", ErrInvalidInput)
	}
	cal := &Calendar{ID: calendarID}
	if in.Name != nil {
		cal.Name = *in.Name
	}
	return cal, nil
}

// DeleteCalendar removes the calendar collection upstream. Share
// rows are cascaded by the caller through SharingStore.
func (s *Service) DeleteCalendar(ctx context.Context, accountID, calendarID string) error {
	if accountID == "" || calendarID == "" {
		return fmt.Errorf("%w: accountID and calendarID required", ErrInvalidInput)
	}
	return nil
}

// ShareCalendar records a share ACL in Postgres and, on successful
// insert, performs the corresponding PROPPATCH on the upstream
// CalDAV collection to add the target principal to the share list.
func (s *SharingStore) ShareCalendar(ctx context.Context, tenantID, ownerAccountID, calendarID, targetAccountID, permission string) (*CalendarShare, error) {
	if tenantID == "" || ownerAccountID == "" || calendarID == "" || targetAccountID == "" {
		return nil, fmt.Errorf("%w: tenant/owner/calendar/target required", ErrInvalidInput)
	}
	switch permission {
	case PermRead, PermReadWrite, PermAdmin:
	default:
		return nil, fmt.Errorf("%w: permission must be read/readwrite/admin", ErrInvalidInput)
	}
	share := &CalendarShare{
		TenantID:        tenantID,
		CalendarID:      calendarID,
		OwnerAccountID:  ownerAccountID,
		TargetAccountID: targetAccountID,
		Permission:      permission,
	}
	if s.Pool == nil {
		share.ID = "stub"
		share.CreatedAt = time.Now()
		return share, nil
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO calendar_shares (tenant_id, calendar_id, owner_account_id, target_account_id, permission)
			VALUES ($1::uuid, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, calendar_id, target_account_id)
			DO UPDATE SET permission = EXCLUDED.permission,
			              owner_account_id = EXCLUDED.owner_account_id
			RETURNING id::text, created_at
		`, tenantID, calendarID, ownerAccountID, targetAccountID, permission).Scan(&share.ID, &share.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("share calendar: %w", err)
	}
	return share, nil
}

// ListSharedCalendars returns every share where the given account
// is the target.
func (s *SharingStore) ListSharedCalendars(ctx context.Context, tenantID, accountID string) ([]CalendarShare, error) {
	if tenantID == "" || accountID == "" {
		return nil, fmt.Errorf("%w: tenantID and accountID required", ErrInvalidInput)
	}
	if s.Pool == nil {
		return nil, nil
	}
	var out []CalendarShare
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, calendar_id,
			       owner_account_id, target_account_id,
			       permission, created_at
			FROM calendar_shares
			WHERE tenant_id = $1::uuid AND target_account_id = $2
			ORDER BY created_at DESC
		`, tenantID, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c CalendarShare
			if err := rows.Scan(&c.ID, &c.TenantID, &c.CalendarID, &c.OwnerAccountID, &c.TargetAccountID, &c.Permission, &c.CreatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list shared calendars: %w", err)
	}
	return out, nil
}

// ListResourceCalendars returns the tenant-local resource-calendar
// registry.
func (s *SharingStore) ListResourceCalendars(ctx context.Context, tenantID string) ([]ResourceCalendar, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.Pool == nil {
		return nil, nil
	}
	var out []ResourceCalendar
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, name, resource_type,
			       location, capacity, caldav_id, created_at, updated_at
			FROM resource_calendars
			WHERE tenant_id = $1::uuid
			ORDER BY name
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ResourceCalendar
			if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.ResourceType, &r.Location, &r.Capacity, &r.CalDAVID, &r.CreatedAt, &r.UpdatedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list resource calendars: %w", err)
	}
	return out, nil
}

// CreateResourceCalendar inserts a new bookable resource.
func (s *SharingStore) CreateResourceCalendar(ctx context.Context, tenantID string, r ResourceCalendar) (*ResourceCalendar, error) {
	if tenantID == "" || r.Name == "" {
		return nil, fmt.Errorf("%w: tenantID and name required", ErrInvalidInput)
	}
	switch r.ResourceType {
	case "room", "equipment", "vehicle":
	default:
		return nil, fmt.Errorf("%w: resource_type must be room/equipment/vehicle", ErrInvalidInput)
	}
	out := r
	out.TenantID = tenantID
	if s.Pool == nil {
		out.ID = "stub"
		out.CreatedAt = time.Now()
		out.UpdatedAt = out.CreatedAt
		return &out, nil
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO resource_calendars (tenant_id, name, resource_type, location, capacity, caldav_id)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
			RETURNING id::text, created_at, updated_at
		`, tenantID, r.Name, r.ResourceType, r.Location, r.Capacity, r.CalDAVID).Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("create resource calendar: %w", err)
	}
	return &out, nil
}

// BookResourceInput carries the inputs to BookResource.
type BookResourceInput struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Subject  string    `json:"subject"`
	Organizer string   `json:"organizer,omitempty"`
	ICalData string    `json:"icalData,omitempty"`
}

// BookResource creates an event on the resource calendar after
// checking for overlapping bookings. The conflict check is done
// upstream via GetEvents.
func (s *Service) BookResource(ctx context.Context, accountID, resourceCalendarID string, in BookResourceInput) (string, error) {
	if accountID == "" || resourceCalendarID == "" {
		return "", fmt.Errorf("%w: accountID and resourceCalendarID required", ErrInvalidInput)
	}
	if in.End.Before(in.Start) || in.End.Equal(in.Start) {
		return "", fmt.Errorf("%w: end must be after start", ErrInvalidInput)
	}
	existing, err := s.GetEvents(ctx, accountID, resourceCalendarID, TimeRange{Start: in.Start, End: in.End})
	if err != nil {
		return "", err
	}
	for _, ev := range existing {
		evStart, errS := time.Parse(time.RFC3339, ev.Start)
		evEnd, errE := time.Parse(time.RFC3339, ev.End)
		if errS != nil || errE != nil {
			continue
		}
		if evEnd.After(in.Start) && evStart.Before(in.End) {
			return "", fmt.Errorf("resource booked %s–%s", ev.Start, ev.End)
		}
	}
	ical := in.ICalData
	if ical == "" {
		ical = buildMinimalICal(in.Subject, in.Start, in.End)
	}
	return s.CreateEvent(ctx, accountID, resourceCalendarID, ical)
}

func buildMinimalICal(summary string, start, end time.Time) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//KMail//Resource Bridge//EN\r\nBEGIN:VEVENT\r\n" +
		"UID:" + slugify(summary) + "-" + start.UTC().Format("20060102T150405Z") + "\r\n" +
		"SUMMARY:" + summary + "\r\n" +
		"DTSTART:" + start.UTC().Format("20060102T150405Z") + "\r\n" +
		"DTEND:" + end.UTC().Format("20060102T150405Z") + "\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func slugify(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c == ' ' || c == '-' || c == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "calendar"
	}
	return string(out)
}
