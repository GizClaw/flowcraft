#![allow(clippy::unwrap_used, clippy::expect_used)]

//! The server side of trace context.
//!
//! One test per binary: the pipeline is process-global, and the assertion is
//! about what a *record* ends up containing, so it has to go through the real
//! subscriber rather than a capture layer.

use std::convert::Infallible;
use std::sync::{Arc, Mutex};

use flowcraft::kernel::TraceLayer;
use flowcraft::telemetry::{Install, Profile, install};
use tower::{Layer as _, Service as _, ServiceExt as _};

#[test]
fn a_request_span_inherits_the_remote_trace() {
    // The ring is the observable: whatever the pipeline wrote is what a support
    // bundle would contain.
    let guard = install(
        Install::new(Profile::Service)
            .with_directives("info")
            .with_ring(64 * 1024),
    )
    .expect("install");

    // A trace started elsewhere: 32 hex characters, sampled.
    let remote_trace = "4bf92f3577b34da6a3ce929d0e0e4736";
    let header = format!("00-{remote_trace}-00f067aa0ba902b7-01");

    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("runtime");
    runtime.block_on(async {
        let seen: Arc<Mutex<Option<(String, String)>>> = Arc::new(Mutex::new(None));
        let handler_seen = seen.clone();
        let service = tower::service_fn(move |_request: http::Request<()>| {
            let seen = handler_seen.clone();
            async move {
                // A record emitted inside the handler, like a capability endpoint
                // logging what it did.
                tracing::info!(flowcraft.capability = "workspace.Workspace/v1", "read");
                *seen.lock().expect("lock") =
                    Some(flowcraft::telemetry::trace_ids().expect("a span around the handler"));
                Ok::<_, Infallible>(http::Response::new(()))
            }
        });
        let mut layered = TraceLayer.layer(service);
        let request: http::Request<()> = http::Request::builder()
            .uri("/workspace.Workspace/v1/Read")
            .header("traceparent", header.as_str())
            .body(())
            .expect("request");
        let run: Result<http::Response<()>, Infallible> =
            layered.ready().await.expect("ready").call(request).await;
        run.expect("call");
        let ids = seen.lock().expect("lock").clone().expect("handler ids");
        assert_eq!(
            ids.0, remote_trace,
            "the handler runs in the caller's trace"
        );
        assert_ne!(ids.1, "00f067aa0ba902b7", "but as its own span");
    });

    let dump = guard.ring_dump().expect("ring");
    assert!(
        dump.contains(remote_trace),
        "the handler's record must carry the caller's trace id:\n{dump}"
    );
    assert!(dump.contains("flowcraft.capability=workspace.Workspace/v1"));
}
