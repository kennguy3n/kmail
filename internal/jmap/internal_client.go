// Package jmap — BFF-initiated JMAP client.
//
// Where `Proxy` proxies a single client HTTP request to Stalwart,
// `InternalClient` constructs JMAP request bodies *inside* the
// BFF and dispatches them to Stalwart on behalf of an
// authenticated user. The first consumer is the
// `/api/v1/sync/bootstrap` handler in `internal/sync` — it needs
// to issue a composed `Mailbox/get` + `Email/query` +
// `Email/get` batch in a single Stalwart round-trip without going
// back out through the client.
//
// The client deliberately reuses the proxy's transport (mTLS
// certificate material, dialer tuning) and account-resolution
// cache so a cold-start pod doesn't double the upstream
// handshake or Postgres-query rate. Shard failover happens here
// at the URL level rather than via the proxy's
// `shardFailoverTransport` — that wrapper is bonded to a
// `httputil.ReverseProxy` which fixes the upstream URL on
// `SetURL`, so failover has to happen inside the RoundTripper.
// BFF-initiated callers construct the request URL themselves so a
// simple loop over the shard list is the correct shape.
package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// InternalClient is the BFF-initiated JMAP client.
//
// Construct via `NewInternalClient(proxy)`. The client is
// safe for concurrent use across handlers.
type InternalClient struct {
	proxy   *Proxy
	httpc   *http.Client
	timeout time.Duration
}

// internalClientDefaultTimeout bounds the BFF→Stalwart request.
// Mailbox/get + Email/query + Email/get for the bootstrap window
// should comfortably fit under a second on a warm shard; the
// 30 s bound is the long-tail cap for cold caches + first-page
// blob hydration. Matches the JMAP proxy's
// `http.DefaultTransport` write deadline so a slow Stalwart
// fails the BFF handler the same way it fails a proxied request.
const internalClientDefaultTimeout = 30 * time.Second

// NewInternalClient returns a client backed by the proxy's
// transport. `proxy` must be non-nil and fully constructed
// (`NewProxy` returned successfully).
func NewInternalClient(proxy *Proxy) (*InternalClient, error) {
	if proxy == nil {
		return nil, errors.New("jmap.NewInternalClient: proxy is required")
	}
	tr := proxy.BaseTransport()
	if tr == nil {
		return nil, errors.New("jmap.NewInternalClient: proxy has no transport (uninitialised?)")
	}
	return &InternalClient{
		proxy:   proxy,
		httpc:   &http.Client{Transport: tr, Timeout: internalClientDefaultTimeout},
		timeout: internalClientDefaultTimeout,
	}, nil
}

// ResolveAccountID surfaces the proxy's shared cache so colocated
// callers (the sync service) can resolve `(tenant, user) → account_id`
// without having to depend on the proxy directly. The cache /
// Postgres-fallback semantics match the proxy on every hit.
func (c *InternalClient) ResolveAccountID(ctx context.Context, tenantID, kchatUserID string) (string, error) {
	return c.proxy.ResolveAccountID(ctx, tenantID, kchatUserID)
}

// SetTimeout overrides the per-request timeout. Pass 0 to reset to
// the default. Used by tests; production callers stick with the
// default.
func (c *InternalClient) SetTimeout(d time.Duration) {
	if d <= 0 {
		d = internalClientDefaultTimeout
	}
	c.timeout = d
	c.httpc.Timeout = d
}

// JmapRequest is the wire shape of a JMAP-over-HTTP request body
// (RFC 8620 §3.3). Modelled with `any` field values so a single
// type can carry every method call shape (`Mailbox/get`,
// `Email/query`, `Email/get`, `EmailSubmission/set`, …) without
// per-method structs in the BFF — those live SDK-side.
type JmapRequest struct {
	Using       []string          `json:"using"`
	MethodCalls [][]any           `json:"methodCalls"`
	CreatedIds  map[string]string `json:"createdIds,omitempty"`
}

// JmapResponse mirrors the response envelope. Errors surface
// through individual method-call entries in the JMAP spec, so
// the BFF parses the outer shape and the caller inspects
// `MethodResponses[i][0]` for `error` vs the typed method name.
type JmapResponse struct {
	MethodResponses [][]any           `json:"methodResponses"`
	SessionState    string            `json:"sessionState"`
	CreatedIds      map[string]string `json:"createdIds,omitempty"`
}

// CallByID returns the response arguments for the method
// invocation whose client-supplied ID matches `id`. JMAP servers
// preserve method-call ID across the request/response boundary
// (RFC 8620 §3.2). Returns `(name, args, true)` on match,
// `("", nil, false)` otherwise.
func (r *JmapResponse) CallByID(id string) (string, map[string]any, bool) {
	for _, entry := range r.MethodResponses {
		if len(entry) != 3 {
			continue
		}
		name, _ := entry[0].(string)
		args, _ := entry[1].(map[string]any)
		gotID, _ := entry[2].(string)
		if gotID == id {
			return name, args, true
		}
	}
	return "", nil, false
}

// FirstCallError extracts the first JMAP method-level error from
// the response, if any. The JMAP envelope itself is HTTP 200 even
// when individual method calls fail (`["error", {...}, "c0"]` is
// the canonical shape per RFC 8620 §3.5.1), so the bootstrap
// handler must surface those to the SDK as a 502/5xx rather than
// returning a partially-empty response.
func (r *JmapResponse) FirstCallError() error {
	for _, entry := range r.MethodResponses {
		if len(entry) != 3 {
			continue
		}
		name, _ := entry[0].(string)
		if name != "error" {
			continue
		}
		args, _ := entry[1].(map[string]any)
		typ, _ := args["type"].(string)
		desc, _ := args["description"].(string)
		if desc != "" {
			return fmt.Errorf("jmap method error: %s: %s", typ, desc)
		}
		return fmt.Errorf("jmap method error: %s", typ)
	}
	return nil
}

// Dispatch sends `req` to Stalwart as the given user. The user is
// identified by `(tenantID, kchatUserID)`; the client resolves
// the Stalwart account ID through the proxy's cache and stamps
// the same `X-KMail-*` identity headers as the proxy (Stalwart
// trusts them only because the mTLS handshake authenticated the
// BFF).
//
// On primary-shard 5xx or transport failure, the client retries
// against each secondary shard in order. The breaker / Postgres
// account cache that backs `Proxy.ResolveAccountID` is shared
// across the proxy and internal client, so a hot account is
// resolved once and reused on both paths.
func (c *InternalClient) Dispatch(
	ctx context.Context,
	tenantID, kchatUserID string,
	req JmapRequest,
) (*JmapResponse, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("jmap.InternalClient.Dispatch: tenantID is required")
	}
	if strings.TrimSpace(kchatUserID) == "" {
		return nil, errors.New("jmap.InternalClient.Dispatch: kchatUserID is required")
	}
	accountID, err := c.proxy.ResolveAccountID(ctx, tenantID, kchatUserID)
	if err != nil {
		return nil, fmt.Errorf("resolve stalwart account: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal jmap request: %w", err)
	}

	urls := c.proxy.ResolveShardURLs(ctx, tenantID)
	if len(urls) == 0 {
		urls = []string{c.proxy.Target().String()}
	}

	var lastErr error
	for _, base := range urls {
		endpoint, err := joinPath(base, "/jmap/api")
		if err != nil {
			lastErr = err
			continue
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("X-KMail-Tenant-Id", tenantID)
		httpReq.Header.Set("X-KMail-Kchat-User-Id", kchatUserID)
		httpReq.Header.Set("X-KMail-Stalwart-Account-Id", accountID)

		resp, err := c.httpc.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("jmap dispatch %s: %w", base, err)
			c.proxy.logger.Printf("jmap internal client transport error shard=%s err=%v", base, err)
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, internalClientMaxResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("jmap dispatch %s: read body: %w", base, readErr)
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("jmap dispatch %s: upstream %d", base, resp.StatusCode)
			c.proxy.logger.Printf("jmap internal client 5xx shard=%s status=%d", base, resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 400 {
			// 4xx is not retryable across shards — every shard
			// gets the same body and would return the same
			// 4xx. Surface immediately so the caller can map to
			// the corresponding handler response.
			return nil, fmt.Errorf("jmap dispatch %s: upstream %d: %s", base, resp.StatusCode, truncate(respBody, 512))
		}
		var out JmapResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, fmt.Errorf("jmap dispatch %s: unmarshal response: %w", base, err)
		}
		if err := out.FirstCallError(); err != nil {
			// Method-level error inside a 2xx envelope — surface
			// directly. Per RFC 8620 §3.5.1 the response is HTTP
			// 200 even when a method invocation fails, so the
			// transport-level retry loop never saw it.
			return nil, err
		}
		return &out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("jmap dispatch: no shard URL configured")
	}
	return nil, lastErr
}

// internalClientMaxResponseBytes bounds the response body the
// internal client will accept from Stalwart. JMAP responses are
// bounded by the request's `maxObjectsInGet` plus per-object
// metadata. The bootstrap handler caps Email/get at 1000 objects
// of metadata; at ~4 KiB per object that's ~4 MiB. 16 MiB gives
// a 4x safety margin for headers + future schema growth without
// letting a hostile / misconfigured Stalwart wedge the BFF on
// `io.ReadAll`.
const internalClientMaxResponseBytes = 16 << 20

// truncate clips a byte slice to at most `n` bytes, returning a
// printable string. Used for error messages where the upstream
// 4xx body might be hostile or just unhelpfully large.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// joinPath concatenates a base URL with an absolute path,
// preserving the base's scheme + host + any base path. Used to
// produce per-shard request URLs.
func joinPath(base, relPath string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse shard url %q: %w", base, err)
	}
	if !strings.HasPrefix(relPath, "/") {
		relPath = "/" + relPath
	}
	// Normalise: avoid double slashes when base path ends with "/".
	u.Path = strings.TrimSuffix(u.Path, "/") + relPath
	return u.String(), nil
}
