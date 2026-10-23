//! Trace context, from the OpenTelemetry SDK.
//!
//! The SDK owns ids, spans and the `traceparent` encoding; this module is the
//! adapter that keeps our vocabulary (`flowcraft.install`, `error.kind`, …) as
//! span attributes and hands the ids to the record renderer.
//!
//! # One thing to know about `tracing-opentelemetry`
//!
//! A tracing span becomes a real OpenTelemetry span lazily: the SDK assigns its
//! ids the first time somebody asks the span for its context. That is why
//! [`ids`] calls `Span::current().context()` rather than reading a thread-local:
//! the call is what materialises the span, and it is the only supported way to
//! get the id of the span a record was emitted in. The side effect is the point —
//! the span is then also exported on close when an exporter is configured.
//!
//! # Without an exporter
//!
//! A deployment with no collector still gets valid ids: the provider is built
//! with the default sampler and no span processor, so spans are created, given
//! ids, and dropped. That is what makes a record from a user's machine joinable
//! to the same request in the cloud later.

use opentelemetry::KeyValue;
use opentelemetry::trace::TracerProvider as _;
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::trace as sdktrace;
use tracing::Subscriber;
use tracing_subscriber::registry::LookupSpan;

use crate::attrs::attr;
use crate::resource::Resource;

/// The instrumentation scope every span and record is attributed to.
pub(crate) const SCOPE: &str = "flowcraft";

/// Build the tracer provider for `resource`.
///
/// With an exporter the provider batches; without one it drops the spans after
/// assigning their ids.
pub(crate) fn provider(
    resource: &Resource,
    otlp: Option<&crate::options::Otlp>,
) -> sdktrace::TracerProvider {
    // The propagator is global state in the SDK. Installing it next to the
    // provider means any process that has a provider also speaks W3C context,
    // and no caller has to remember a second call.
    opentelemetry::global::set_text_map_propagator(TraceContextPropagator::new());
    let builder = sdktrace::TracerProvider::builder().with_resource(sdk_resource(resource));
    wrap(builder, otlp).build()
}

/// Attach the exporter, when one is configured.
#[cfg(feature = "otlp")]
fn wrap(builder: sdktrace::Builder, otlp: Option<&crate::options::Otlp>) -> sdktrace::Builder {
    let Some(config) = otlp else {
        // No collector: ids are still generated, spans are dropped.
        return builder;
    };
    match crate::export::span_exporter(config) {
        Some(exporter) => builder.with_batch_exporter(exporter, opentelemetry_sdk::runtime::Tokio),
        None => builder,
    }
}

#[cfg(not(feature = "otlp"))]
fn wrap(builder: sdktrace::Builder, otlp: Option<&crate::options::Otlp>) -> sdktrace::Builder {
    let _ = otlp;
    builder
}

/// The span layer for `provider`.
pub(crate) fn layer<S>(
    provider: &sdktrace::TracerProvider,
) -> tracing_opentelemetry::OpenTelemetryLayer<S, sdktrace::Tracer>
where
    S: Subscriber + for<'a> LookupSpan<'a>,
{
    tracing_opentelemetry::layer().with_tracer(provider.tracer(SCOPE))
}

/// `trace_id` and `span_id` of the span the current thread is inside.
///
/// Returns `None` outside a span, and outside a *valid* one: a record with no
/// context must not pretend to have one.
///
/// Besides reading the ids this **materialises** the span: the SDK assigns them
/// on first use, and from then on the span is exported when it closes.
#[must_use]
pub fn ids() -> Option<(String, String)> {
    use opentelemetry::trace::TraceContextExt as _;
    use tracing_opentelemetry::OpenTelemetrySpanExt as _;

    let span = tracing::Span::current();
    if span.is_none() {
        return None;
    }
    let context = span.context();
    let span_context = context.span().span_context().clone();
    span_context.is_valid().then(|| {
        (
            format!("{:032x}", span_context.trace_id()),
            format!("{:016x}", span_context.span_id()),
        )
    })
}

/// Make `span` a child of whatever `headers` says.
///
/// Must be called before the span is first materialised, which is why the
/// server middleware sets it before instrumenting the request future. Headers
/// without a usable `traceparent` leave the span as a new root.
pub fn set_parent(span: &tracing::Span, headers: &propagation::Headers) {
    use tracing_opentelemetry::OpenTelemetrySpanExt as _;

    span.set_parent(propagation::extract(headers));
}

/// The identity every span carries, in the same spelling the records use.
pub(crate) fn sdk_resource(resource: &Resource) -> opentelemetry_sdk::Resource {
    let mut attributes = vec![
        KeyValue::new("service.name", resource.service.clone()),
        KeyValue::new("service.version", resource.version.clone()),
    ];
    for (key, value) in resource.fields() {
        attributes.push(KeyValue::new(key, value.as_text()));
    }
    attributes.push(KeyValue::new(attr::SESSION, resource.session.clone()));
    opentelemetry_sdk::Resource::new(attributes)
}

/// Reading and writing `traceparent`.
pub mod propagation {
    use std::collections::HashMap;

    use opentelemetry::propagation::{Extractor, Injector};
    use tracing_opentelemetry::OpenTelemetrySpanExt as _;

    /// Header values, keyed by the lowercase header name.
    ///
    /// A plain map, so a transport only has to convert its own header type into
    /// this and back; the telemetry crate stays free of tonic and `http`. The
    /// encoding and validation are the SDK's.
    pub type Headers = HashMap<String, String>;

    struct Carrier(Headers);

    impl Injector for Carrier {
        fn set(&mut self, key: &str, value: String) {
            self.0.insert(key.to_owned(), value);
        }
    }

    impl Extractor for Carrier {
        fn get(&self, key: &str) -> Option<&str> {
            self.0.get(key).map(String::as_str)
        }

        fn keys(&self) -> Vec<&str> {
            self.0.keys().map(String::as_str).collect()
        }
    }

    /// Write the current span's context into `into`.
    ///
    /// A no-op outside a span: there is nothing to propagate, and an invented
    /// parent would link two unrelated requests.
    pub fn inject_current(into: &mut Headers) {
        let context = tracing::Span::current().context();
        let mut carrier = Carrier(std::mem::take(into));
        opentelemetry::global::get_text_map_propagator(|propagator| {
            propagator.inject_context(&context, &mut carrier)
        });
        *into = carrier.0;
    }

    /// Write `span`'s context into `into`.
    ///
    /// The client middleware injects from the span it just opened, not from
    /// whatever happens to be current: that is what puts the client span in the
    /// server span's ancestry instead of beside it.
    pub fn inject_span(span: &tracing::Span, into: &mut Headers) {
        use tracing_opentelemetry::OpenTelemetrySpanExt as _;

        let context = span.context();
        let mut carrier = Carrier(std::mem::take(into));
        opentelemetry::global::get_text_map_propagator(|propagator| {
            propagator.inject_context(&context, &mut carrier)
        });
        *into = carrier.0;
    }

    /// The context `from` describes, or the empty one.
    #[must_use]
    pub fn extract(from: &Headers) -> opentelemetry::Context {
        opentelemetry::global::get_text_map_propagator(|propagator| {
            propagator.extract(&Carrier(from.clone()))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tracing_subscriber::layer::SubscriberExt;

    fn subscriber() -> impl Subscriber {
        let provider = provider(
            &Resource {
                service: "flowcraftd".to_owned(),
                version: "0.0.0".to_owned(),
                ..Resource::default()
            },
            None,
        );
        tracing_subscriber::registry().with(layer::<tracing_subscriber::Registry>(&provider))
    }

    #[test]
    fn a_record_inside_a_span_gets_the_sdks_ids() {
        tracing::subscriber::with_default(subscriber(), || {
            assert!(ids().is_none(), "no span, no ids");
            let outer = tracing::info_span!("outer");
            outer.in_scope(|| {
                let outer_ids = ids().expect("outer ids");
                assert_eq!(outer_ids.0.len(), 32);
                assert_eq!(outer_ids.1.len(), 16);

                let inner = tracing::info_span!("inner");
                inner.in_scope(|| {
                    let inner_ids = ids().expect("inner ids");
                    assert_eq!(inner_ids.0, outer_ids.0, "same trace");
                    assert_ne!(inner_ids.1, outer_ids.1, "a different span");
                });
                assert_eq!(ids().expect("outer again"), outer_ids);
            });
        });
    }

    #[test]
    fn a_remote_traceparent_becomes_the_parent() {
        let remote = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
        let headers = propagation::Headers::from([("traceparent".to_owned(), remote.to_owned())]);

        tracing::subscriber::with_default(subscriber(), || {
            use tracing_opentelemetry::OpenTelemetrySpanExt as _;
            // Extracted here: the propagator is installed when the provider is
            // built, which is inside `subscriber()`.
            let span = tracing::info_span!("handle");
            span.set_parent(propagation::extract(&headers));
            span.in_scope(|| {
                let ids = ids().expect("ids");
                assert_eq!(ids.0, "4bf92f3577b34da6a3ce929d0e0e4736");
                assert_ne!(ids.1, "00f067aa0ba902b7", "a new span, same trace");
            });
        });
    }

    #[test]
    fn injecting_outside_a_span_writes_nothing() {
        let mut headers = propagation::Headers::new();
        propagation::inject_current(&mut headers);
        assert!(headers.is_empty(), "{headers:?}");
    }

    #[test]
    fn injection_round_trips_through_the_propagator() {
        tracing::subscriber::with_default(subscriber(), || {
            let span = tracing::info_span!("call");
            span.in_scope(|| {
                let mut headers = propagation::Headers::new();
                propagation::inject_current(&mut headers);
                let header = headers.get("traceparent").expect("a traceparent").clone();
                let own = ids().expect("own ids");
                assert!(header.contains(&own.0), "{header}");
                assert!(header.contains(&own.1), "{header}");
            });
        });
    }
}
