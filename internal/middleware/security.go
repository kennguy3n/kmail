package middleware

// Security headers + CORS middleware.
//
// Wired as the outermost handler wrapper in `cmd/kmail-api/main.go`
// so every response — health checks, JMAP, admin REST — picks up
// the same baseline. CSP defaults are restrictive but allow the
// React app's origin via `app-src` (configured through the
// `WebOrigins` field). CORS allowed origins come from
// `KMAIL_CORS_ORIGINS` (comma-separated).

import (
	"net/http"
	"strings"
)

// SecurityConfig configures the headers middleware.
type SecurityConfig struct {
	// WebOrigins is the list of trusted origins for the React app
	// (e.g. `["https://kmail.kchat.dev"]`). Used in the CSP and
	// CORS allow-list.
	WebOrigins []string

	// HSTSMaxAgeSeconds defaults to one year (31_536_000) when 0.
	HSTSMaxAgeSeconds int

	// FrameOptions defaults to "DENY". Override to e.g. "SAMEORIGIN"
	// only if a deliberate framing requirement exists.
	FrameOptions string

	// AllowAllOrigins bypasses the CORS allow-list (returns
	// `Access-Control-Allow-Origin: *`). Intended for dev only.
	AllowAllOrigins bool
}

// Security is the security-headers middleware.
type Security struct {
	cfg          SecurityConfig
	csp          string
	hsts         string
	allowOrigins map[string]struct{}
}

// NewSecurity returns a Security middleware. Always non-nil even
// when cfg is zero — defaults are safe for production.
func NewSecurity(cfg SecurityConfig) *Security {
	if cfg.HSTSMaxAgeSeconds <= 0 {
		cfg.HSTSMaxAgeSeconds = 31_536_000
	}
	if strings.TrimSpace(cfg.FrameOptions) == "" {
		cfg.FrameOptions = "DENY"
	}
	allow := map[string]struct{}{}
	for _, o := range cfg.WebOrigins {
		o = strings.TrimSpace(o)
		if o != "" {
			allow[o] = struct{}{}
		}
	}
	return &Security{
		cfg:          cfg,
		csp:          buildCSP(cfg.WebOrigins),
		hsts:         "max-age=" + itoa(cfg.HSTSMaxAgeSeconds) + "; includeSubDomains",
		allowOrigins: allow,
	}
}

// Wrap installs the headers around `next`.
func (s *Security) Wrap(next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", s.csp)
		hdr.Set("Strict-Transport-Security", s.hsts)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", s.cfg.FrameOptions)
		hdr.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		hdr.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")

		origin := r.Header.Get("Origin")
		if origin != "" {
			if s.cfg.AllowAllOrigins {
				hdr.Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := s.allowOrigins[origin]; ok {
				hdr.Set("Access-Control-Allow-Origin", origin)
				hdr.Set("Vary", "Origin")
			}
			hdr.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			hdr.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-KMail-Tenant-ID, X-KMail-User-ID")
			hdr.Set("Access-Control-Expose-Headers", "X-KMail-Degraded")
			hdr.Set("Access-Control-Max-Age", "600")
		}

		if r.Method == http.MethodOptions && origin != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func buildCSP(origins []string) string {
	clean := make([]string, 0, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(o)
		if o != "" {
			clean = append(clean, o)
		}
	}
	app := "'self'"
	if len(clean) > 0 {
		app = "'self' " + strings.Join(clean, " ")
	}
	// Restrictive defaults. `script-src` deliberately omits
	// `unsafe-inline` / `unsafe-eval` — the React build emits
	// hashed bundle entries.
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + app,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src " + app + " https:",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")
}

// itoa is a tiny strconv.Itoa replacement so this file stays
// `strconv`-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SplitOrigins parses a comma-separated origin list (e.g. from
// `KMAIL_CORS_ORIGINS`) into a deduped slice.
func SplitOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := map[string]struct{}{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
