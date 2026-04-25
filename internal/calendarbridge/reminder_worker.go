// Package calendarbridge — pre-event reminder worker.
//
// ReminderWorker is the background goroutine that fires KChat
// reminders 15 / 5 minutes before an event start. It polls the
// CalDAV store every 60s for events in the upcoming 30-minute
// window, deduplicates already-sent reminders against Valkey, and
// dispatches via the Notifier.
//
// The worker is intentionally per-tenant-naive: it iterates every
// tenant in `tenants` whose status is active. A future Phase 5
// optimization will replace the polling loop with a JMAP push
// channel as soon as Stalwart's calendar JMAP capability ships.
package calendarbridge

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ReminderWindows are the pre-event windows the worker fires for.
// The 5-min window is below the 60s polling interval so we
// over-fetch slightly to avoid missing reminders that fall between
// ticks.
var ReminderWindows = []int{15, 5}

// ReminderWorker is the background poller. Construct via
// NewReminderWorker; call Run with a cancellable context.
type ReminderWorker struct {
	pool         *pgxpool.Pool
	cal          *Service
	notifier     *Notifier
	valkey       *redis.Client
	logger       *log.Logger
	pollInterval time.Duration
	now          func() time.Time
}

// NewReminderWorker wires the dependencies.
func NewReminderWorker(pool *pgxpool.Pool, cal *Service, notifier *Notifier, valkey *redis.Client, logger *log.Logger) *ReminderWorker {
	if logger == nil {
		logger = log.Default()
	}
	return &ReminderWorker{
		pool:         pool,
		cal:          cal,
		notifier:     notifier,
		valkey:       valkey,
		logger:       logger,
		pollInterval: 60 * time.Second,
		now:          time.Now,
	}
}

// WithPollInterval is a test-only override.
func (w *ReminderWorker) WithPollInterval(d time.Duration) *ReminderWorker {
	w.pollInterval = d
	return w
}

// Run loops until ctx is cancelled.
func (w *ReminderWorker) Run(ctx context.Context) {
	if w == nil || w.cal == nil || w.notifier == nil {
		return
	}
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	// Tick immediately on startup so a freshly-restarted BFF doesn't
	// miss a reminder that should fire in the first 60s.
	if err := w.tick(ctx); err != nil {
		w.logger.Printf("calendarbridge.reminder: initial tick: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.tick(ctx); err != nil {
				w.logger.Printf("calendarbridge.reminder: tick: %v", err)
			}
		}
	}
}

func (w *ReminderWorker) tick(ctx context.Context) error {
	if w.pool == nil {
		return nil
	}
	rows, err := w.pool.Query(ctx, `
		SELECT t.id::text, u.stalwart_account_id
		FROM tenants t
		JOIN users u ON u.tenant_id = t.id
		WHERE t.status = 'active' AND u.status = 'active'
	`)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	type pair struct {
		tenant, account string
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.tenant, &p.account); err != nil {
			return err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pairs {
		if err := w.tickTenant(ctx, p.tenant, p.account); err != nil {
			w.logger.Printf("calendarbridge.reminder: tenant %s: %v", p.tenant, err)
		}
	}
	return nil
}

func (w *ReminderWorker) tickTenant(ctx context.Context, tenantID, accountID string) error {
	cals, err := w.cal.ListCalendars(ctx, accountID)
	if err != nil {
		return err
	}
	now := w.now().UTC()
	r := TimeRange{Start: now, End: now.Add(30 * time.Minute)}
	for _, c := range cals {
		events, err := w.cal.GetEvents(ctx, accountID, c.ID, r)
		if err != nil {
			w.logger.Printf("calendarbridge.reminder: GetEvents %s/%s: %v", accountID, c.ID, err)
			continue
		}
		for _, ev := range events {
			info := summaryFromICal(ev.ICalData)
			info.UID = ev.UID
			info.CalendarID = c.ID
			if info.Start == "" {
				info.Start = ev.Start
			}
			start, err := parseEventStart(info.Start)
			if err != nil {
				continue
			}
			delta := start.Sub(now)
			for _, win := range ReminderWindows {
				lo := time.Duration(win)*time.Minute - w.pollInterval
				hi := time.Duration(win) * time.Minute
				if delta < lo || delta > hi {
					continue
				}
				if w.alreadySent(ctx, tenantID, ev.UID, win) {
					continue
				}
				if err := w.notifier.NotifyReminder(ctx, tenantID, info, win); err != nil {
					w.logger.Printf("calendarbridge.reminder: notify %s/%s: %v", tenantID, ev.UID, err)
					continue
				}
				w.markSent(ctx, tenantID, ev.UID, win)
			}
		}
	}
	return nil
}

func reminderKey(tenantID, eventID string, minutesBefore int) string {
	return fmt.Sprintf("reminder:%s:%s:%d", tenantID, eventID, minutesBefore)
}

func (w *ReminderWorker) alreadySent(ctx context.Context, tenantID, eventID string, minutesBefore int) bool {
	if w.valkey == nil {
		return false
	}
	v, err := w.valkey.Get(ctx, reminderKey(tenantID, eventID, minutesBefore)).Result()
	if err != nil || v == "" {
		return false
	}
	return true
}

func (w *ReminderWorker) markSent(ctx context.Context, tenantID, eventID string, minutesBefore int) {
	if w.valkey == nil {
		return
	}
	_ = w.valkey.Set(ctx, reminderKey(tenantID, eventID, minutesBefore), strconv.Itoa(minutesBefore), 24*time.Hour).Err()
}

// summaryFromICal pulls the SUMMARY / DTSTART / LOCATION / ORGANIZER
// fields out of an iCalendar payload. Sufficient for the reminder
// card; the full VEVENT lives in CalDAV.
func summaryFromICal(ical string) EventInfo {
	out := EventInfo{}
	for _, line := range strings.Split(ical, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "SUMMARY:"):
			out.Summary = strings.TrimPrefix(line, "SUMMARY:")
		case strings.HasPrefix(line, "DTSTART"):
			if i := strings.Index(line, ":"); i >= 0 {
				out.Start = line[i+1:]
			}
		case strings.HasPrefix(line, "DTEND"):
			if i := strings.Index(line, ":"); i >= 0 {
				out.End = line[i+1:]
			}
		case strings.HasPrefix(line, "LOCATION:"):
			out.Location = strings.TrimPrefix(line, "LOCATION:")
		case strings.HasPrefix(line, "ORGANIZER"):
			if i := strings.Index(line, ":"); i >= 0 {
				out.Organizer = line[i+1:]
			}
		}
	}
	return out
}

// parseEventStart accepts either RFC 3339 or the iCalendar
// "YYYYMMDDThhmmssZ" format.
func parseEventStart(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339,
		"20060102T150405Z",
		"20060102T150405",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("calendarbridge: unrecognized DTSTART %q", s)
}
