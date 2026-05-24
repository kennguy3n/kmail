// Top-level error taxonomy for the SDK.
//
// The variants are split so platform shells can map them to
// localised UI without re-parsing strings: `Auth` → reauthenticate
// flow, `RateLimit` → backoff banner, `Conflict` → resync prompt,
// etc. The JMAP error-code → variant mapping mirrors
// docs/JMAP-CONTRACT.md §6 so the BFF and the SDK agree on the
// wire contract for failures.

use thiserror::Error;

/// Public error type for every fallible `KMailClient` call.
#[derive(Debug, Error)]
pub enum Error {
    /// Local SQLite or filesystem I/O failure on the offline store.
    #[error("local store error: {0}")]
    Store(String),

    /// HTTP transport or TLS failure talking to the BFF.
    #[error("transport error: {0}")]
    Transport(String),

    /// BFF returned 401. Caller should refresh the OIDC token.
    #[error("authentication failed: {0}")]
    Auth(String),

    /// BFF returned 403. Caller's plan / tenant gating blocks this.
    #[error("forbidden: {0}")]
    Forbidden(String),

    /// BFF returned 404 for the targeted resource.
    #[error("not found: {0}")]
    NotFound(String),

    /// BFF returned 429. Caller should back off and retry.
    #[error("rate limited: retry after {retry_after_seconds}s")]
    RateLimit { retry_after_seconds: u64 },

    /// JMAP method-level error inside an otherwise-2xx batch
    /// response. `code` is the JMAP error type, e.g.
    /// `urn:ietf:params:jmap:error:invalidArguments`.
    #[error("jmap method error [{code}]: {description}")]
    JmapMethod { code: String, description: String },

    /// Wire-format parse failure — JSON didn't match the expected
    /// shape. Distinct from `Transport` because the remedy is
    /// usually "file a bug", not "retry".
    #[error("invalid jmap response: {0}")]
    Protocol(String),

    /// HTTP 4xx response that does NOT fall into one of the
    /// dedicated 4xx variants above (`Auth` for 401,
    /// `Forbidden` for 403, `NotFound` for 404, `RateLimit` for
    /// 429). Carries the raw status code and body so platform
    /// shells can surface the server's explanation verbatim
    /// (e.g. "413: attachment exceeds 25 MiB", "422: malformed
    /// `Email/set` patch").
    ///
    /// **Not retryable.** This is the load-bearing distinction
    /// from `Transport(_)`: a 400/405/409/413/422 means the
    /// server has examined the request and explicitly refused
    /// it. Retrying the same request will deterministically
    /// produce the same 4xx, so spinning through
    /// `with_retries` with exponential backoff only delays the
    /// error surfacing without changing the outcome.
    #[error("http client error [{status}]: {body}")]
    HttpClient { status: u16, body: String },

    /// Local state token diverged from the server; caller must
    /// re-bootstrap. The SDK raises this when JMAP returns
    /// `cannotCalculateChanges` per RFC 8620 §5.6.
    #[error("sync state diverged; full re-bootstrap required")]
    SyncStateDiverged,

    /// AEAD authentication failed: ciphertext, AAD, or key wrong.
    #[error("decryption failed: {0}")]
    Decryption(String),

    /// Key derivation parameters invalid (empty IKM, oversized L).
    #[error("key derivation error: {0}")]
    KeyDerivation(String),

    /// KeyStore (platform keychain) bridge failure.
    #[error("keystore error: {0}")]
    KeyStore(String),

    /// Caller passed arguments that fail SDK-side validation
    /// before any network round-trip.
    #[error("invalid argument: {0}")]
    InvalidArgument(String),

    /// Operation was cancelled by the caller (drop / abort).
    #[error("operation cancelled")]
    Cancelled,
}

/// Convenience alias used throughout the crate.
pub type Result<T> = std::result::Result<T, Error>;

impl From<rusqlite::Error> for Error {
    fn from(value: rusqlite::Error) -> Self {
        Error::Store(value.to_string())
    }
}

impl From<std::io::Error> for Error {
    fn from(value: std::io::Error) -> Self {
        Error::Store(value.to_string())
    }
}

impl From<serde_json::Error> for Error {
    fn from(value: serde_json::Error) -> Self {
        Error::Protocol(value.to_string())
    }
}

impl From<reqwest::Error> for Error {
    fn from(value: reqwest::Error) -> Self {
        if value.is_timeout() {
            Error::Transport(format!("request timed out: {value}"))
        } else if value.is_connect() {
            Error::Transport(format!("connect failed: {value}"))
        } else {
            Error::Transport(value.to_string())
        }
    }
}

impl From<reqwest::header::InvalidHeaderValue> for Error {
    fn from(value: reqwest::header::InvalidHeaderValue) -> Self {
        Error::InvalidArgument(format!("invalid header value: {value}"))
    }
}

impl Error {
    /// Returns `true` if the caller should attempt the operation
    /// again after waiting. Two call-sites consult this method and
    /// they apply **different semantics**:
    ///
    /// 1. The HTTP transport retry loop in `jmap::transport::with_retries`
    ///    treats "retryable" as *re-issue the HTTP request immediately
    ///    (after exponential backoff)*. Only error types the transport
    ///    itself produces are relevant here.
    /// 2. The pending-actions queue drainer in `client::flush_pending_actions`
    ///    treats "retryable" as *leave the action on the queue for the
    ///    next sync attempt* \u2014 a much weaker statement than "retry now".
    ///
    /// `Error::Cancelled` is classified as retryable because cancellation
    /// is observed only from the queue drainer side (it surfaces when a
    /// `tokio::task` is dropped, e.g. the FFI host process backgrounds the
    /// app mid-sync). A cancelled write must stay on the queue so the next
    /// sync replays it; dropping it would silently lose user actions. The
    /// `MAX_PENDING_ACTION_ATTEMPTS` ceiling (10) prevents an infinite
    /// retry loop if the cancellation is structurally persistent.
    ///
    /// The transport-loop semantic remains safe **only as long as
    /// `Cancelled` is never produced inside the transport layer**.
    /// `From<reqwest::Error>` maps to `Transport(_)`, not `Cancelled`, so
    /// the loop currently never observes a `Cancelled` to retry. Future
    /// contributors adding a cancellation-token path to the transport
    /// MUST either route it to a non-retryable variant (e.g. a new
    /// `Aborted` variant) or special-case `Cancelled` inside the loop \u2014
    /// otherwise an aborted request will retry until the `with_retries`
    /// attempt budget is exhausted, defeating the entire point of the
    /// cancellation signal.
    #[must_use]
    pub fn is_retryable(&self) -> bool {
        matches!(
            self,
            Error::Transport(_) | Error::RateLimit { .. } | Error::Cancelled
        )
    }
}
