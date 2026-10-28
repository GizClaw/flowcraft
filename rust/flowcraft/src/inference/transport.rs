//! The one place a provider call happens.
//!
//! Splitting the call from the lowering is what makes a compile testable as a JSON
//! snapshot and a decode testable as a canned payload: neither needs a socket. It also
//! keeps the "only one stage does I/O" rule checkable — the compiler is a pure
//! function, and this trait is the only thing that is not.
//!
//! Asynchronous and object-safe on purpose: the pipeline is generic over *which*
//! provider, not over *how* bytes get there, so a caller may plug in a client that
//! reuses one connection pool, or a recorder in a test.

use async_trait::async_trait;
use serde_json::Value;

use crate::error::Result;

/// Performs one provider request.
#[async_trait]
pub trait Transport: Send + Sync {
    /// POST `body` to `url` and return the decoded JSON answer.
    ///
    /// `headers` is what the deployment configured (an `authorization` and a
    /// content-type, usually). A non-2xx answer is the caller's error to classify: the
    /// transport reports what the provider said, and mapping a provider's error body
    /// into the taxonomy is the adapter's job, not the socket's.
    async fn post_json(&self, url: &str, headers: &[(&str, &str)], body: &Value) -> Result<Value>;
}
