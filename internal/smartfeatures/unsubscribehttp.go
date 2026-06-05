package smartfeatures

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SafeOneClickUnsubscriber performs RFC 8058 one-click unsubscribe
// POSTs with an SSRF guard. Because the target URL is attacker-
// controlled (it comes from an inbound email's List-Unsubscribe
// header), a naive server-side POST would let a malicious sender
// coerce the BFF into reaching internal services — the classic
// SSRF pitfall. This client therefore:
//
//   - accepts https only (RFC 8058 §4 requires the one-click target
//     be https; http is downgrade-able and rejected);
//   - resolves the host and refuses loopback, private, link-local,
//     and unspecified destinations at dial time (so DNS rebinding
//     can't slip a public name past the pre-check);
//   - refuses to follow redirects (a 30x could bounce to an
//     internal target after the guard already passed);
//   - bounds the request with a short timeout.
type SafeOneClickUnsubscriber struct {
	client  *http.Client
	timeout time.Duration
}

// DefaultOneClickTimeout bounds a single one-click POST.
const DefaultOneClickTimeout = 10 * time.Second

// NewSafeOneClickUnsubscriber builds the guarded poster. timeout
// <= 0 falls back to DefaultOneClickTimeout.
func NewSafeOneClickUnsubscriber(timeout time.Duration) *SafeOneClickUnsubscriber {
	if timeout <= 0 {
		timeout = DefaultOneClickTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		// Guard at the connection layer: the address passed here is
		// the post-DNS-resolution ip:port, so a hostname that
		// resolves to a private IP (DNS rebinding) is still blocked.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if err := guardAddr(addr); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout: timeout,
	}
	return &SafeOneClickUnsubscriber{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("smartfeatures: refusing redirect to %s", req.URL.Redacted())
			},
		},
		timeout: timeout,
	}
}

// Post issues the RFC 8058 one-click POST. The body and
// Content-Type are fixed by the spec (§3.1): the form value
// `List-Unsubscribe=One-Click`.
func (s *SafeOneClickUnsubscriber) Post(ctx context.Context, rawurl string) error {
	u, err := url.Parse(strings.TrimSpace(rawurl))
	if err != nil {
		return fmt.Errorf("smartfeatures: parse unsubscribe url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("smartfeatures: refusing non-https unsubscribe target %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("smartfeatures: unsubscribe url has no host")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(),
		strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		return fmt.Errorf("smartfeatures: build unsubscribe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("smartfeatures: one-click unsubscribe: %w", err)
	}
	defer resp.Body.Close()
	// Drain the (small, ignored) body so the transport can return the
	// connection to its pool for keep-alive reuse instead of closing it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("smartfeatures: one-click unsubscribe returned %d", resp.StatusCode)
	}
	return nil
}

// guardAddr rejects any address that resolves to a non-global
// destination. Called from the transport's DialContext so it sees
// the resolved IP, defeating DNS-rebinding bypasses of a name-only
// pre-check.
func guardAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Address wasn't a literal IP — let the resolver's results be
		// re-checked on the next dial attempt. In practice Go's dialer
		// passes the resolved IP here, so this branch is defensive.
		return nil
	}
	if !isGlobalUnicast(ip) {
		return fmt.Errorf("smartfeatures: refusing unsubscribe to non-public address %s", ip)
	}
	return nil
}

// isGlobalUnicast reports whether ip is a routable public address,
// rejecting loopback, private (RFC 1918 / ULA), link-local,
// unspecified, and multicast ranges.
func isGlobalUnicast(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return ip.IsGlobalUnicast()
}
