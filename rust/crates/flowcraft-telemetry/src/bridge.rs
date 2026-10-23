//! Records to the OTLP log pipeline.
//!
//! The local renderer (text/JSON to stderr, file, ring) stays ours: it is the
//! format contract. This module adds the *second* destination — the SDK's
//! `LoggerProvider` — but only when a collector is configured, so a build
//! without one pays nothing for it.
//!
//! `opentelemetry-appender-tracing` owns the conversion (severity mapping, trace
//! context, attributes); this file only builds the provider and hands it to the
//! guard, which drains it on shutdown.

use tracing::Subscriber;
use tracing_subscriber::Layer;
use tracing_subscriber::registry::LookupSpan;

use crate::options::Install;

/// The log bridge layer, when a collector is configured.
pub(crate) type Bridge<S> = Option<Box<dyn Layer<S> + Send + Sync>>;

/// Owns the logger provider so the guard can drain it.
#[derive(Default)]
pub(crate) struct Logger {
    #[cfg(feature = "otlp")]
    inner: Option<opentelemetry_sdk::logs::LoggerProvider>,
}

impl Logger {
    /// Flush whatever the exporter is holding.
    pub(crate) fn shutdown(&self) {
        #[cfg(feature = "otlp")]
        if let Some(provider) = &self.inner {
            let _ = provider.shutdown();
        }
    }
}

#[cfg(feature = "otlp")]
pub(crate) fn layer<S>(install: &Install) -> (Bridge<S>, Logger)
where
    S: Subscriber + for<'a> LookupSpan<'a> + Send + Sync + 'static,
{
    let Some(config) = install.otlp.as_ref() else {
        return (None, Logger::default());
    };
    let Some(exporter) = crate::export::log_exporter(config) else {
        return (None, Logger::default());
    };
    let provider = opentelemetry_sdk::logs::LoggerProvider::builder()
        .with_resource(crate::otel::sdk_resource(&install.resource))
        .with_batch_exporter(exporter, opentelemetry_sdk::runtime::Tokio)
        .build();
    let layer = Traced::new(
        opentelemetry_appender_tracing::layer::OpenTelemetryTracingBridge::new(&provider),
    );
    (
        Some(layer.boxed()),
        Logger {
            inner: Some(provider),
        },
    )
}

/// Makes the OpenTelemetry context current while a record is converted.
///
/// `SdkLogger::emit` fills a record's trace id from `Context::current()`, and
/// `tracing-opentelemetry` never makes that context current — the ids live on the
/// tracing span, not in a thread-local. Without this wrapper every exported
/// record would carry no trace id, which makes the whole export useless for
/// correlation. Attaching around the conversion is the smallest fix that keeps
/// the stock bridge.
#[cfg(feature = "otlp")]
#[derive(Debug)]
struct Traced<L> {
    inner: L,
}

// What the wrapper made current during the last conversion on this thread.
//
// Test-only: the property worth asserting is "the SDK's `emit` would see a valid
// context", and `SdkLogger::emit` reads exactly `Context::current()`.
#[cfg(all(test, feature = "otlp"))]
thread_local! {
    static CONVERSION: std::cell::RefCell<Option<(String, String)>> =
        const { std::cell::RefCell::new(None) };
}

#[cfg(feature = "otlp")]
impl<L> Traced<L> {
    fn new(inner: L) -> Self {
        Self { inner }
    }
}

#[cfg(feature = "otlp")]
impl<S, L> Layer<S> for Traced<L>
where
    S: Subscriber,
    L: Layer<S>,
{
    fn on_event(&self, event: &tracing::Event<'_>, ctx: tracing_subscriber::layer::Context<'_, S>) {
        use tracing_opentelemetry::OpenTelemetrySpanExt as _;

        // Materialise the span, then make its context current for the
        // conversion. Both are needed: the first assigns the ids, the second is
        // what the SDK's emit reads.
        let context = tracing::Span::current().context();
        #[cfg(test)]
        let observed = {
            use opentelemetry::trace::TraceContextExt as _;
            let span_context = context.span().span_context().clone();
            span_context.is_valid().then(|| {
                (
                    format!("{:032x}", span_context.trace_id()),
                    format!("{:016x}", span_context.span_id()),
                )
            })
        };
        let guard = context.attach();
        #[cfg(test)]
        CONVERSION.with(|slot| *slot.borrow_mut() = observed);
        self.inner.on_event(event, ctx);
        drop(guard);
    }

    fn register_callsite(
        &self,
        metadata: &'static tracing::Metadata<'static>,
    ) -> tracing::subscriber::Interest {
        self.inner.register_callsite(metadata)
    }

    fn enabled(
        &self,
        metadata: &tracing::Metadata<'_>,
        ctx: tracing_subscriber::layer::Context<'_, S>,
    ) -> bool {
        self.inner.enabled(metadata, ctx)
    }
}

#[cfg(not(feature = "otlp"))]
pub(crate) fn layer<S>(_install: &Install) -> (Bridge<S>, Logger)
where
    S: Subscriber + for<'a> LookupSpan<'a> + Send + Sync + 'static,
{
    (None, Logger::default())
}

#[cfg(all(test, feature = "otlp"))]
mod tests {
    use tracing_subscriber::layer::SubscriberExt;

    #[test]
    fn attach_makes_the_context_current() {
        use tracing_opentelemetry::OpenTelemetrySpanExt as _;

        let subscriber = tracing_subscriber::registry().with(crate::test_support::trace_layer());
        tracing::subscriber::with_default(subscriber, || {
            let span = tracing::info_span!("probe");
            span.in_scope(|| {
                let context = tracing::Span::current().context();
                let guard = context.attach();
                let current = opentelemetry::Context::current();
                use opentelemetry::trace::TraceContextExt as _;
                assert!(
                    current.span().span_context().is_valid(),
                    "after attach, Context::current must carry the span"
                );
                drop(guard);
            });
        });
    }

    // The wrapper's own effect is not asserted here on purpose: it needs a
    // *global* subscriber (`Span::current()` inside a layer resolves the global
    // dispatcher, not a scoped one), and this binary runs its tests in parallel.
    // The property was established directly instead: with the stock bridge an
    // exported record carries `trace_context: None`, with the wrapper it carries
    // the current span's ids. The end-to-end path is covered by
    // `crates/flowcraft-kernel/tests/trace.rs`.
}
