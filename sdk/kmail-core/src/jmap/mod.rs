// JMAP client used by the SDK.
//
// Sub-modules:
//
//   transport — reqwest-backed HTTP client with Bearer auth,
//               retry-with-backoff for 429 / 5xx / transport
//               failures, and connection keepalive.
//   request   — `JmapRequest` builder (`using` capabilities +
//               `methodCalls` triplets).
//   response  — Strongly typed batch response parser; surfaces
//               method-level errors via `Error::JmapMethod`.
//   ops       — Higher-level wrappers around individual JMAP
//               methods (`Mailbox/get`, `Email/changes`, etc.).
//
// The public surface is `JmapClient` — exposed via this module.

pub mod ops;
pub mod request;
pub mod response;
pub mod transport;

pub use ops::JmapClient;
pub use request::{
    EmailChangesArgs, EmailGetArgs, EmailQueryArgs, JmapInvocation, JmapRequest, MailboxGetArgs,
};
pub use response::{JmapInvocationResponse, JmapResponse, MethodErrorPayload};
pub use transport::{JmapTransport, TransportConfig};
