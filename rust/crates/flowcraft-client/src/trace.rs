//! Carrying trace context across the wire.
//!
//! Two calls in two processes are one trace only if the `traceparent` header
//! travels with them. The propagation logic itself lives in
//! `flowcraft_telemetry::propagation`; this module is the thin gRPC adapter, so the
//! telemetry crate keeps no dependency on tonic.
//!
//! The header is written by the same middleware that opens the client span, so
//! the server's span is a child of it: caller → client span → server span. A
//! header stamped earlier, before the client span existed, would put the two
//! spans side by side instead.

use http::HeaderValue;

use flowcraft_telemetry::propagation;

/// A span around every outgoing RPC.
///
/// The span is what makes a trace readable end to end: the server's span is its
/// child (via `traceparent`), so a capability call stops being an orphan branch
/// in the collector. It is applied to the channel, not to the request builder,
/// because the interesting part is the round trip, not the construction.
#[derive(Debug, Clone, Copy, Default)]
pub struct TraceLayer;

impl<S> tower::Layer<S> for TraceLayer {
    type Service = TraceService<S>;

    fn layer(&self, inner: S) -> Self::Service {
        TraceService { inner }
    }
}

/// The service [`TraceLayer`] builds.
#[derive(Debug, Clone)]
pub struct TraceService<S> {
    inner: S,
}

impl<S, B> tower::Service<http::Request<B>> for TraceService<S>
where
    S: tower::Service<http::Request<B>>,
{
    type Response = S::Response;
    type Error = S::Error;
    type Future = tracing::instrument::Instrumented<S::Future>;

    fn poll_ready(
        &mut self,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::result::Result<(), Self::Error>> {
        self.inner.poll_ready(cx)
    }

    fn call(&mut self, mut request: http::Request<B>) -> Self::Future {
        use tracing::Instrument as _;

        // `otel.name` and `otel.kind` are read by tracing-opentelemetry itself,
        // which is why they are not part of our attribute vocabulary.
        let span = tracing::info_span!(
            "rpc",
            rpc.method = %request.uri().path(),
            otel.name = %request.uri().path(),
            otel.kind = "client",
        );
        stamp(&span, request.headers_mut());
        self.inner.call(request).instrument(span)
    }
}

/// The W3C header the SDK's propagator writes.
const TRACEPARENT: &str = "traceparent";

/// Write `span`'s `traceparent` into the outgoing headers.
fn stamp(span: &tracing::Span, headers: &mut http::HeaderMap) {
    let mut values = propagation::Headers::new();
    propagation::inject_span(span, &mut values);
    let Some(value) = values.get(TRACEPARENT) else {
        return;
    };
    // A malformed value is unreachable for a well-formed context, and not worth
    // failing a call over: a request without a trace still works.
    if let Ok(value) = HeaderValue::try_from(value.as_str()) {
        headers.insert(TRACEPARENT, value);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    use tower::{Layer as _, Service as _, ServiceExt as _};
    use tracing_subscriber::layer::SubscriberExt;

    /// The traceparent the inner service received, and the ids it ran with.
    type Seen = Arc<Mutex<Option<(String, String)>>>;

    fn call_with_caller_span<R>(body: impl FnOnce() -> R) -> (R, Seen) {
        let seen: Seen = Arc::new(Mutex::new(None));
        let captured = seen.clone();
        // The layer opens the span; the inner service (user code) records what
        // it received. `with_default` is enough because the assertions are made
        // from user code, not from a layer callback.
        let outcome = tracing::subscriber::with_default(
            tracing_subscriber::registry().with(flowcraft_telemetry::test_support::trace_layer()),
            || {
                let runtime = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("runtime");
                runtime.block_on(async move {
                    let service = tower::service_fn(move |request: http::Request<()>| {
                        let captured = captured.clone();
                        async move {
                            let ids = flowcraft_telemetry::trace_ids().expect("a span");
                            let header = request
                                .headers()
                                .get(TRACEPARENT)
                                .and_then(|value| value.to_str().ok())
                                .unwrap_or_default()
                                .to_owned();
                            *captured.lock().expect("lock") = Some((ids.1, header));
                            Ok::<_, std::convert::Infallible>(http::Response::new(()))
                        }
                    });
                    let mut layered = TraceLayer.layer(service);
                    let request: http::Request<()> = http::Request::new(());
                    let _ = layered.ready().await.expect("ready").call(request).await;
                });
                body()
            },
        );
        (outcome, seen)
    }

    #[test]
    fn a_call_runs_inside_its_own_span() {
        let ((), seen) = call_with_caller_span(|| ());
        let (_, header) = seen.lock().expect("lock").clone().expect("one call");
        assert!(!header.is_empty(), "the call must carry a traceparent");
    }

    #[test]
    fn the_header_names_the_client_span_not_the_caller() {
        let ((), seen) = call_with_caller_span(|| ());
        let (client_span_id, header) = seen.lock().expect("lock").clone().expect("one call");
        // `00-<32 hex trace>-<16 hex span>-01`
        let span_part = header.split('-').nth(2).expect("a traceparent");
        assert_eq!(
            span_part, client_span_id,
            "the server must see the client span as its parent, not the caller: {header}"
        );
    }
}
