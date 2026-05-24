// JMAP transport — Bearer-authenticated POSTs to the BFF with
// retry/backoff and connection keepalive.
//
// What's distinct about the JMAP wire protocol vs. plain HTTP:
//   - Every request and response is a single JSON document, even
//     when the document carries N method invocations.
//   - 200 OK with a JSON body containing a `methodResponses` array
//     is the canonical success shape. A 200 OK with the wrong
//     shape is a *protocol* failure, not a *transport* failure.
//   - 429 must carry a `Retry-After` header; the SDK respects it
//     to avoid hammering the BFF when rate-limited.
//
// The transport is intentionally protocol-agnostic — `post_json`
// takes any serde-encodable body and returns the raw JSON. The
// JMAP-shaped `JmapClient` layers on top of this and is responsible
// for parsing `methodResponses`.

use crate::error::{Error, Result};
use reqwest::{header, Client, StatusCode};
use serde::Serialize;
use std::sync::{Arc, RwLock};
use std::time::Duration;

/// Tunable knobs for the transport. Defaults mirror the React web
/// client's reqwest equivalents — same timeouts, same retry policy.
///
/// `Debug` is implemented by hand so that `bearer_token` is **never**
/// rendered verbatim, even via `tracing::debug!(?cfg, ...)`.
#[derive(Clone)]
pub struct TransportConfig {
    /// Absolute base URL of the BFF (e.g. `https://kmail.example.com`).
    pub base_url: String,
    /// OIDC bearer token presented on every request. Hot-swappable
    /// via [`JmapTransport::set_bearer_token`]; the SDK does not run
    /// OAuth itself.
    pub bearer_token: String,
    /// Per-request timeout. Default 30s, matches the BFF's
    /// 30s circuit-breaker open window.
    pub request_timeout: Duration,
    /// Total wall-clock budget for retries on a single logical
    /// call. Default 60s.
    pub retry_budget: Duration,
    /// Maximum retry attempts. Default 4 (so 3 retries after the
    /// first failure).
    pub max_attempts: u32,
    /// Optional user-agent override. Defaults to
    /// `"kmail-sdk/<crate-version> (<platform>)"`.
    pub user_agent: Option<String>,
}

impl TransportConfig {
    pub fn new(base_url: impl Into<String>, bearer_token: impl Into<String>) -> Self {
        Self {
            base_url: base_url.into(),
            bearer_token: bearer_token.into(),
            request_timeout: Duration::from_secs(30),
            retry_budget: Duration::from_secs(60),
            max_attempts: 4,
            user_agent: None,
        }
    }
}

impl std::fmt::Debug for TransportConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TransportConfig")
            .field("base_url", &self.base_url)
            .field("bearer_token", &"<redacted>")
            .field("request_timeout", &self.request_timeout)
            .field("retry_budget", &self.retry_budget)
            .field("max_attempts", &self.max_attempts)
            .field("user_agent", &self.user_agent)
            .finish()
    }
}

/// Reqwest-backed JMAP transport.
///
/// The bearer token lives in `Arc<RwLock<String>>` so that
/// [`set_bearer_token`] is visible to every clone of this transport
/// (and therefore to every clone of `JmapClient` / `KMailClient`).
/// Read-lock acquisition on the request path is uncontended in
/// steady state — the writer is the platform shell calling refresh
/// once every few minutes when the OIDC access token rotates.
#[derive(Clone)]
pub struct JmapTransport {
    base_url: String,
    bearer_token: Arc<RwLock<String>>,
    retry_budget: Duration,
    max_attempts: u32,
    client: Client,
}

impl JmapTransport {
    pub fn new(config: TransportConfig) -> Result<Self> {
        let user_agent = config.user_agent.clone().unwrap_or_else(|| {
            format!(
                "kmail-sdk/{} ({})",
                env!("CARGO_PKG_VERSION"),
                std::env::consts::OS
            )
        });

        let client = Client::builder()
            .user_agent(user_agent)
            .timeout(config.request_timeout)
            .pool_idle_timeout(Duration::from_secs(90))
            .tcp_keepalive(Duration::from_secs(60))
            .build()
            .map_err(|e| Error::Transport(format!("client build failed: {e}")))?;
        Ok(Self {
            base_url: config.base_url,
            bearer_token: Arc::new(RwLock::new(config.bearer_token)),
            retry_budget: config.retry_budget,
            max_attempts: config.max_attempts,
            client,
        })
    }

    /// Hot-swap the OIDC bearer token. The new value is observed by
    /// every existing clone of this transport on the next request
    /// — no reconnect, no client rebuild, no in-flight request
    /// failure. Returns `Err(Error::Store("poisoned"))` if the
    /// internal lock has been poisoned by a panicking writer (a
    /// near-impossible failure mode, surfaced as a recoverable error
    /// rather than a process-wide panic).
    pub fn set_bearer_token(&self, token: impl Into<String>) -> Result<()> {
        let mut guard = self
            .bearer_token
            .write()
            .map_err(|_| Error::Store("transport bearer-token lock poisoned".into()))?;
        *guard = token.into();
        Ok(())
    }

    fn current_token(&self) -> Result<String> {
        let guard = self
            .bearer_token
            .read()
            .map_err(|_| Error::Store("transport bearer-token lock poisoned".into()))?;
        Ok(guard.clone())
    }

    /// Test-only accessor. Exposes the live bearer token without
    /// issuing a network request, so unit tests can assert that
    /// [`set_bearer_token`] is observed by cloned transports.
    #[doc(hidden)]
    pub fn current_bearer_token_for_test(&self) -> Result<String> {
        self.current_token()
    }

    /// GET a JSON resource. Used for `/jmap/session`.
    pub async fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T> {
        let url = self.absolute_url(path);
        self.with_retries(|| async {
            let token = self.current_token()?;
            let resp = self
                .client
                .get(&url)
                .bearer_auth(&token)
                .header(header::ACCEPT, "application/json")
                .send()
                .await?;
            classify_response(resp).await
        })
        .await
    }

    /// POST a JSON body and parse the response as JSON.
    pub async fn post_json<B: Serialize, T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        body: &B,
    ) -> Result<T> {
        let url = self.absolute_url(path);
        self.with_retries(|| async {
            let token = self.current_token()?;
            let resp = self
                .client
                .post(&url)
                .bearer_auth(&token)
                .header(header::CONTENT_TYPE, "application/json")
                .header(header::ACCEPT, "application/json")
                .json(body)
                .send()
                .await?;
            classify_response(resp).await
        })
        .await
    }

    fn absolute_url(&self, path: &str) -> String {
        if path.starts_with("http://") || path.starts_with("https://") {
            path.to_string()
        } else if path.starts_with('/') {
            format!("{}{}", self.base_url.trim_end_matches('/'), path)
        } else {
            format!("{}/{}", self.base_url.trim_end_matches('/'), path)
        }
    }

    /// Retry loop with exponential backoff respecting `Retry-After`.
    async fn with_retries<F, Fut, T>(&self, mut op: F) -> Result<T>
    where
        F: FnMut() -> Fut,
        Fut: std::future::Future<Output = Result<T>>,
    {
        let start = std::time::Instant::now();
        let mut attempt: u32 = 0;
        loop {
            attempt += 1;
            let result = op().await;
            match result {
                Ok(value) => return Ok(value),
                Err(e) => {
                    let elapsed = start.elapsed();
                    let last_attempt = attempt >= self.max_attempts || elapsed >= self.retry_budget;
                    if !e.is_retryable() || last_attempt {
                        return Err(e);
                    }
                    let delay = backoff_delay(attempt, &e);
                    tracing::debug!(
                        attempt,
                        delay_ms = delay.as_millis() as u64,
                        elapsed_ms = elapsed.as_millis() as u64,
                        "kmail-sdk: retrying after transient error: {e}"
                    );
                    tokio::time::sleep(delay).await;
                }
            }
        }
    }
}

/// Map a reqwest response to a typed `Result<T>`. JMAP error
/// codes are surfaced in the `JmapClient` layer — this function
/// only handles HTTP-level outcomes.
async fn classify_response<T: serde::de::DeserializeOwned>(resp: reqwest::Response) -> Result<T> {
    let status = resp.status();
    if status.is_success() {
        let bytes = resp.bytes().await?;
        if bytes.is_empty() {
            return Err(Error::Protocol("empty response body".into()));
        }
        let parsed: T = serde_json::from_slice(&bytes)
            .map_err(|e| Error::Protocol(format!("response parse failed: {e}")))?;
        return Ok(parsed);
    }
    match status {
        StatusCode::UNAUTHORIZED => Err(Error::Auth(read_text(resp).await)),
        StatusCode::FORBIDDEN => Err(Error::Forbidden(read_text(resp).await)),
        StatusCode::NOT_FOUND => Err(Error::NotFound(read_text(resp).await)),
        StatusCode::TOO_MANY_REQUESTS => {
            let retry_after = resp
                .headers()
                .get(header::RETRY_AFTER)
                .and_then(|h| h.to_str().ok())
                .and_then(|s| s.parse::<u64>().ok())
                .unwrap_or(1);
            Err(Error::RateLimit {
                retry_after_seconds: retry_after,
            })
        }
        s if s.is_server_error() => Err(Error::Transport(format!(
            "upstream {s}: {}",
            read_text(resp).await
        ))),
        s => Err(Error::Transport(format!(
            "unexpected {s}: {}",
            read_text(resp).await
        ))),
    }
}

async fn read_text(resp: reqwest::Response) -> String {
    resp.text().await.unwrap_or_default()
}

/// Exponential backoff with a Retry-After-aware floor.
fn backoff_delay(attempt: u32, last_error: &Error) -> Duration {
    if let Error::RateLimit {
        retry_after_seconds,
    } = last_error
    {
        return Duration::from_secs(*retry_after_seconds);
    }
    // 250ms * 2^(n-1), capped at 8s.
    let base = 250u64;
    let exp = attempt.saturating_sub(1).min(6);
    let ms = base.saturating_mul(1u64 << exp).min(8_000);
    Duration::from_millis(ms)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Backoff schedule must double until the cap, and must obey
    /// `Retry-After` on 429s.
    #[test]
    fn backoff_schedule() {
        // attempt 1 -> 250ms, attempt 2 -> 500ms, ..., capped at 8s.
        let err = Error::Transport("x".into());
        assert_eq!(backoff_delay(1, &err), Duration::from_millis(250));
        assert_eq!(backoff_delay(2, &err), Duration::from_millis(500));
        assert_eq!(backoff_delay(3, &err), Duration::from_millis(1000));
        assert_eq!(backoff_delay(7, &err), Duration::from_millis(8_000));
        assert_eq!(backoff_delay(100, &err), Duration::from_millis(8_000));

        // 429 overrides the exponential schedule.
        let rate_limited = Error::RateLimit {
            retry_after_seconds: 5,
        };
        assert_eq!(backoff_delay(1, &rate_limited), Duration::from_secs(5));
    }

    /// Relative paths join against the base URL; absolute URLs
    /// pass through verbatim. Important for `/.well-known/jmap`
    /// returning a different host's session URL.
    #[test]
    fn absolute_url_normalizes_paths() {
        let cfg = TransportConfig::new("https://kmail.example.com/", "tok");
        let t = JmapTransport::new(cfg).unwrap();
        assert_eq!(
            t.absolute_url("/jmap/session"),
            "https://kmail.example.com/jmap/session"
        );
        assert_eq!(
            t.absolute_url("jmap/api"),
            "https://kmail.example.com/jmap/api"
        );
        assert_eq!(
            t.absolute_url("https://other.example.com/jmap/api"),
            "https://other.example.com/jmap/api"
        );
    }
}
