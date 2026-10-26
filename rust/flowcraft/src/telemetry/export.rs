//! The OTLP exporter.
//!
//! Everything protocol-shaped lives in `opentelemetry-otlp`: encoding, the HTTP
//! request, retries. This module only turns our configuration into the builder
//! calls, so there is no wire format anywhere in this repository.
//!
//! Exporting needs a runtime because the SDK's batch processors spawn their
//! worker on one. That is why the feature also enables `rt-tokio`: a deployment
//! that ships telemetry is a long-running process that already has a runtime,
//! and a build that does not ship it never compiles this file.

use opentelemetry_otlp::{WithExportConfig as _, WithHttpConfig as _};

/// A span exporter, when OTLP is configured.
pub(crate) fn span_exporter(
    config: &crate::telemetry::options::Otlp,
) -> Option<opentelemetry_otlp::SpanExporter> {
    let mut builder = opentelemetry_otlp::SpanExporter::builder()
        .with_http()
        .with_endpoint(config.signal_endpoint("/v1/traces"));
    if !config.headers.is_empty() {
        builder = builder.with_headers(config.headers.clone());
    }
    match builder.build() {
        Ok(exporter) => Some(exporter),
        Err(error) => {
            eprintln!("flowcraft-telemetry: cannot build the OTLP span exporter: {error}");
            None
        }
    }
}

/// A log exporter, when OTLP is configured.
pub(crate) fn log_exporter(
    config: &crate::telemetry::options::Otlp,
) -> Option<opentelemetry_otlp::LogExporter> {
    let mut builder = opentelemetry_otlp::LogExporter::builder()
        .with_http()
        .with_endpoint(config.signal_endpoint("/v1/logs"));
    if !config.headers.is_empty() {
        builder = builder.with_headers(config.headers.clone());
    }
    match builder.build() {
        Ok(exporter) => Some(exporter),
        Err(error) => {
            eprintln!("flowcraft-telemetry: cannot build the OTLP log exporter: {error}");
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use crate::telemetry::options::Otlp;

    #[test]
    fn a_bare_endpoint_gets_the_signal_path() {
        let config = Otlp {
            endpoint: "http://collector:4318".to_owned(),
            headers: HashMap::new(),
            insecure: true,
        };
        assert_eq!(
            config.signal_endpoint("/v1/logs"),
            "http://collector:4318/v1/logs"
        );
    }

    #[test]
    fn an_endpoint_that_already_names_the_signal_is_left_alone() {
        let config = Otlp {
            endpoint: "https://api.example/otlp/v1/traces".to_owned(),
            headers: HashMap::new(),
            insecure: false,
        };
        assert_eq!(
            config.signal_endpoint("/v1/traces"),
            "https://api.example/otlp/v1/traces"
        );
    }
}
