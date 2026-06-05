package smartfeatures

import (
	"sort"
	"time"
)

// Analytics is the aggregated email-activity report rendered by the
// Email Analytics admin page. Every field is derived from the
// caller's Sent + Inbox message windows (see AnalyticsService);
// the aggregation itself (Aggregate) is a pure function so the math
// is unit-tested without any JMAP/Valkey dependency.
type Analytics struct {
	RangeStart         string       `json:"range_start"`
	RangeEnd           string       `json:"range_end"`
	TotalSent          int          `json:"total_sent"`
	TotalReceived      int          `json:"total_received"`
	Daily              []DailyCount `json:"daily"`
	TopRecipients      []NamedCount `json:"top_recipients"`
	TopSenders         []NamedCount `json:"top_senders"`
	BusiestHours       []HourCount  `json:"busiest_hours"`
	AvgResponseSeconds float64      `json:"avg_response_seconds"`
	ResponseSampleSize int          `json:"response_sample_size"`
}

// DailyCount is one day's sent/received tally (YYYY-MM-DD in the
// report's timezone).
type DailyCount struct {
	Date     string `json:"date"`
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
}

// NamedCount is a correspondent and how many messages involved them.
type NamedCount struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Count int    `json:"count"`
}

// HourCount is the message volume for one hour-of-day bucket (0..23).
type HourCount struct {
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

const (
	topN              = 10
	maxResponseWindow = 14 * 24 * time.Hour
)

// Aggregate computes the analytics report from the sent and
// received message windows. loc is the timezone used to bucket
// days and hours (so "busiest hour" reflects the tenant's local
// time, not UTC). A nil loc defaults to UTC.
//
// Average response time is approximated per-thread: for any thread
// that has both an inbound message and a later sent message, the
// gap between the inbound receipt and the first subsequent reply is
// one sample. Threads with no reply, or replies that precede the
// inbound (e.g. the user started the thread), contribute nothing.
func Aggregate(sent, received []Message, loc *time.Location, now time.Time) Analytics {
	if loc == nil {
		loc = time.UTC
	}

	daily := map[string]*DailyCount{}
	hours := make([]int, 24)
	recipients := map[string]*NamedCount{}
	senders := map[string]*NamedCount{}

	bump := func(m map[string]*DailyCount, key string) *DailyCount {
		if dc, ok := m[key]; ok {
			return dc
		}
		dc := &DailyCount{Date: key}
		m[key] = dc
		return dc
	}

	// Totals count only messages with a usable timestamp, matching
	// the daily breakdown below (which skips zero-time messages). Using
	// len(sent)/len(received) here would let the KPI totals exceed the
	// sum of the daily bars whenever Stalwart hands back a message with
	// an unparseable receivedAt.
	var totalSent, totalReceived int

	for _, msg := range sent {
		if msg.ReceivedAt.IsZero() {
			continue
		}
		totalSent++
		local := msg.ReceivedAt.In(loc)
		bump(daily, local.Format("2006-01-02")).Sent++
		for _, addr := range append(append([]Address{}, msg.To...), msg.Cc...) {
			tally(recipients, addr)
		}
	}

	for _, msg := range received {
		if msg.ReceivedAt.IsZero() {
			continue
		}
		totalReceived++
		local := msg.ReceivedAt.In(loc)
		bump(daily, local.Format("2006-01-02")).Received++
		hours[local.Hour()]++
		if from, ok := msg.FirstFrom(); ok {
			tally(senders, from)
		}
	}

	avg, sample := avgResponseSeconds(sent, received)

	return Analytics{
		RangeStart:         rangeStart(sent, received, loc),
		RangeEnd:           now.In(loc).Format("2006-01-02"),
		TotalSent:          totalSent,
		TotalReceived:      totalReceived,
		Daily:              sortedDaily(daily),
		TopRecipients:      topNamed(recipients),
		TopSenders:         topNamed(senders),
		BusiestHours:       hourCounts(hours),
		AvgResponseSeconds: avg,
		ResponseSampleSize: sample,
	}
}

func tally(m map[string]*NamedCount, a Address) {
	key := a.Normalized()
	if key == "" {
		return
	}
	if nc, ok := m[key]; ok {
		nc.Count++
		if nc.Name == "" && a.Name != "" {
			nc.Name = a.Name
		}
		return
	}
	m[key] = &NamedCount{Email: key, Name: a.Name, Count: 1}
}

// avgResponseSeconds matches inbound messages to the user's first
// later reply within the same thread and averages the gaps.
func avgResponseSeconds(sent, received []Message) (float64, int) {
	// Earliest inbound per thread.
	firstInbound := map[string]time.Time{}
	for _, m := range received {
		if m.ThreadID == "" || m.ReceivedAt.IsZero() {
			continue
		}
		if t, ok := firstInbound[m.ThreadID]; !ok || m.ReceivedAt.Before(t) {
			firstInbound[m.ThreadID] = m.ReceivedAt
		}
	}
	var total float64
	var n int
	// Earliest reply strictly after the inbound, per thread.
	bestGap := map[string]time.Duration{}
	for _, m := range sent {
		if m.ThreadID == "" || m.ReceivedAt.IsZero() {
			continue
		}
		in, ok := firstInbound[m.ThreadID]
		if !ok || !m.ReceivedAt.After(in) {
			continue
		}
		gap := m.ReceivedAt.Sub(in)
		if gap > maxResponseWindow {
			continue
		}
		if cur, seen := bestGap[m.ThreadID]; !seen || gap < cur {
			bestGap[m.ThreadID] = gap
		}
	}
	for _, gap := range bestGap {
		total += gap.Seconds()
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return total / float64(n), n
}

func sortedDaily(m map[string]*DailyCount) []DailyCount {
	out := make([]DailyCount, 0, len(m))
	for _, dc := range m {
		out = append(out, *dc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

func topNamed(m map[string]*NamedCount) []NamedCount {
	out := make([]NamedCount, 0, len(m))
	for _, nc := range m {
		out = append(out, *nc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Email < out[j].Email
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

func hourCounts(hours []int) []HourCount {
	out := make([]HourCount, 24)
	for h := 0; h < 24; h++ {
		out[h] = HourCount{Hour: h, Count: hours[h]}
	}
	return out
}

func rangeStart(sent, received []Message, loc *time.Location) string {
	var earliest time.Time
	consider := func(msgs []Message) {
		for _, m := range msgs {
			if m.ReceivedAt.IsZero() {
				continue
			}
			if earliest.IsZero() || m.ReceivedAt.Before(earliest) {
				earliest = m.ReceivedAt
			}
		}
	}
	consider(sent)
	consider(received)
	if earliest.IsZero() {
		return ""
	}
	return earliest.In(loc).Format("2006-01-02")
}
