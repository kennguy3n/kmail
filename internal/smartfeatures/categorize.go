package smartfeatures

import "strings"

// Category is a Gmail-style inbox category. The string values are
// the canonical wire form returned by the categorization API and
// also embedded in the JMAP keyword (see Keyword).
type Category string

const (
	CategoryPrimary    Category = "primary"
	CategorySocial     Category = "social"
	CategoryPromotions Category = "promotions"
	CategoryUpdates    Category = "updates"
	CategoryForums     Category = "forums"
)

// AllCategories is the stable display order used by the frontend
// tab bar. Primary is first because it is the default landing tab.
var AllCategories = []Category{
	CategoryPrimary,
	CategorySocial,
	CategoryPromotions,
	CategoryUpdates,
	CategoryForums,
}

// Keyword returns the JMAP keyword used to persist a category on a
// message (RFC 8621 keywords are lower-cased tokens). Storing the
// category as a `$category_*` keyword keeps it queryable through
// the normal Email/query filter path and survives client reloads,
// matching how Stalwart already exposes `$phishing` / `$junk`.
func (c Category) Keyword() string {
	return "$category_" + string(c)
}

// Valid reports whether c is one of the known categories.
func (c Category) Valid() bool {
	switch c {
	case CategoryPrimary, CategorySocial, CategoryPromotions, CategoryUpdates, CategoryForums:
		return true
	default:
		return false
	}
}

// socialDomains are well-known social-network senders. Matched as
// a domain suffix so "mail.notifications.facebook.com" still maps
// to Social. Kept small and high-precision on purpose — the goal
// is to avoid false Social hits, since a misfiled personal email
// is more annoying than an under-categorized promo.
var socialDomains = []string{
	"facebook.com", "facebookmail.com",
	"twitter.com", "x.com",
	"linkedin.com",
	"instagram.com",
	"pinterest.com",
	"tiktok.com",
	"reddit.com", "redditmail.com",
	"youtube.com",
	"snapchat.com",
	"discord.com",
	"meetup.com",
}

// Categorize assigns a message to a Gmail-style category using a
// deterministic precedence ladder. The order is chosen so the most
// specific, least-ambiguous signal wins:
//
//  1. Forums  — mailing-list machinery (List-Id / List-Post). A
//     message routed through a list is a forum/group post first,
//     even if it also carries an unsubscribe link.
//  2. Social  — a known social-network sender domain.
//  3. Promotions — bulk/marketing markers (List-Unsubscribe,
//     Precedence: bulk, marketing campaign headers).
//  4. Updates — automated transactional/notification senders
//     (auto-submitted, no-reply senders, receipts/alerts).
//  5. Primary — everything else (person-to-person mail).
//
// Forums is checked before Promotions because virtually every
// mailing list also sets List-Unsubscribe; without the ordering
// every list message would be miscategorized as Promotions.
func Categorize(m Message) Category {
	if isForum(m) {
		return CategoryForums
	}
	if isSocial(m) {
		return CategorySocial
	}
	if isPromotion(m) {
		return CategoryPromotions
	}
	if isUpdate(m) {
		return CategoryUpdates
	}
	return CategoryPrimary
}

func isForum(m Message) bool {
	// RFC 2919 (List-Id) and RFC 2369 (List-Post) are the canonical
	// "this came through a mailing list" markers.
	return m.HasHeader("List-Id") || m.HasHeader("List-Post") ||
		m.HasHeader("Mailing-List")
}

func isSocial(m Message) bool {
	from, ok := m.FirstFrom()
	if !ok {
		return false
	}
	d := from.Domain()
	if d == "" {
		return false
	}
	for _, sd := range socialDomains {
		if d == sd || strings.HasSuffix(d, "."+sd) {
			return true
		}
	}
	return false
}

func isPromotion(m Message) bool {
	// List-Unsubscribe is the single strongest bulk-mail signal.
	if m.HasHeader("List-Unsubscribe") {
		return true
	}
	if prec := strings.ToLower(m.Header("Precedence")); prec == "bulk" || prec == "list" {
		return true
	}
	// ESP campaign headers emitted by the major bulk senders
	// (Mailchimp, SendGrid, Marketo, etc.).
	for _, h := range []string{
		"X-Campaign", "X-Campaignid", "X-Mailchimp-Campaign",
		"X-Marketing-Email", "X-SG-EID", "X-Marketo-Campaign",
	} {
		if m.HasHeader(h) {
			return true
		}
	}
	return false
}

func isUpdate(m Message) bool {
	// Auto-generated transactional mail per RFC 3834.
	if as := strings.ToLower(m.Header("Auto-Submitted")); as != "" && as != "no" {
		return true
	}
	if strings.EqualFold(m.Header("X-Auto-Response-Suppress"), "All") {
		return true
	}
	// no-reply style senders are almost always automated
	// notifications (receipts, alerts, password resets).
	if from, ok := m.FirstFrom(); ok {
		local := from.Email
		if at := strings.Index(local, "@"); at >= 0 {
			local = local[:at]
		}
		local = strings.ToLower(local)
		for _, marker := range []string{"no-reply", "noreply", "donotreply", "do-not-reply", "notification", "notifications", "alerts"} {
			if strings.Contains(local, marker) {
				return true
			}
		}
	}
	return false
}
