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
use bytes::Bytes;
use futures_util::Stream;
use serde_json::Value;

use crate::error::{Error, Result};

/// The bytes of a streaming answer, as they arrive.
pub type ByteStream = std::pin::Pin<Box<dyn Stream<Item = Result<Bytes>> + Send>>;

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

    /// POST `body` to `url` and return the answer's bytes as they arrive.
    ///
    /// The default refuses: a transport that cannot stream must say so rather than
    /// buffer the whole answer and pretend. Nothing in the taxonomy is lost by that —
    /// the caller gets `NotAvailable` and can fall back to [`Self::post_json`], which
    /// is the honest difference between the two shapes.
    async fn post_stream(
        &self,
        _url: &str,
        _headers: &[(&str, &str)],
        _body: &Value,
    ) -> Result<ByteStream> {
        Err(Error::not_available(
            "inference: this transport cannot stream; use post_json",
        ))
    }
}
