//! `core/utils/httpprobe`: which round trips are worth recording, and what the
//! records say.
//!
//! core wraps an `http.RoundTripper` and emits two records per probed round trip
//! -- "request dispatched" and "response headers received" -- so a caller can
//! tell local assembly time from network plus provider time to first byte. The
//! question it answers is the one per-turn latency reports cannot: how much of a
//! slow inference step is ours.
//!
//! # What is ported, and what is waiting
//!
//! The *policy* is here, because it is the part with rules and it is pure:
//!
//! - [`enabled`] reads the environment (`FLOWCRAFT_HTTP_PROBE`), and only the
//!   values Go's `strconv.ParseBool` accepts turn it on;
//! - [`probed`] decides which round trips are recorded: a body-carrying write,
//!   which is what every inference call is, minus the collector's own
//!   `/v1/{logs,traces,metrics}` exports;
//! - [`request_bytes`] reports the declared body size, and never reads the body:
//!   a request body holds conversation content, and a driver may stream it.
//!
//! The wrapper itself is waiting on two slices it cannot be written without: the
//! telemetry sink (the records are OTel events with the caller's trace id) and
//! the HTTP client (Rust has no process-wide `RoundTripper` to wrap, so the port
//! will decorate a tower layer instead of `http.DefaultTransport`). Both are
//! later slices in `docs/plans/2026-10-15-rust-core-port.md`; until they land,
//! this module answers the questions they will ask it.

use std::time::Duration;

/// Enables the probe: any value `strconv.ParseBool` accepts turns it on.
pub const ENV: &str = "FLOWCRAFT_HTTP_PROBE";

/// Whether the environment asks for the probe.
///
/// `strconv.ParseBool` accepts exactly `1`, `t`, `T`, `TRUE`, `true`, `True`,
/// `0`, `f`, `F`, `FALSE`, `false`, `False`; the value is trimmed first, and
/// anything else is an error, which reads as "off".
pub fn enabled() -> bool {
    enabled_from(std::env::var(ENV).ok().as_deref())
}

/// [`enabled`] with the variable's value passed in, so the rule can be tested
/// without writing to the process environment (which edition 2024 made
/// `unsafe`, and this crate forbids).
pub fn enabled_from(value: Option<&str>) -> bool {
    match value {
        Some(value) => parse_bool(value.trim()).unwrap_or(false),
        None => false,
    }
}

/// `strconv.ParseBool`, spelled out: the accepted set is smaller than a reader
/// expects, and `yes`/`on` are *not* in it.
pub fn parse_bool(value: &str) -> Option<bool> {
    match value {
        "1" | "t" | "T" | "TRUE" | "true" | "True" => Some(true),
        "0" | "f" | "F" | "FALSE" | "false" | "False" => Some(false),
        _ => None,
    }
}

/// One round trip's identifying facts, as far as the policy cares.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Request<'a> {
    /// The method, spelled the way HTTP spells it (`POST`, not `post`).
    pub method: &'a str,
    /// The URL path, without the query.
    pub path: &'a str,
}

impl<'a> Request<'a> {
    /// A request with this method and path.
    pub fn new(method: &'a str, path: &'a str) -> Request<'a> {
        Request { method, path }
    }
}

/// Whether one request is worth recording.
///
/// Only a body-carrying write is, which is what every inference call is. GETs
/// (model lists, MCP discovery) are cheap and would only add noise, and the
/// application's own OTLP export is a POST too, so the collector paths stay out
/// of the record.
pub fn probed(request: &Request<'_>) -> bool {
    if !matches!(request.method, "POST" | "PUT" | "PATCH") {
        return false;
    }
    if request.path.contains("/v1/") {
        // `path.Base` of a path ending in a slash is the element before it, so
        // trailing separators are trimmed first.
        let base = request
            .path
            .trim_end_matches('/')
            .rsplit('/')
            .next()
            .unwrap_or_default();
        if matches!(base, "logs" | "traces" | "metrics") {
            return false;
        }
    }
    true
}

/// The declared body size, or `-1` when the request has no measurable body.
///
/// The body itself is never read or logged: the driver may stream it, and a
/// request body holds conversation content.
pub fn request_bytes(content_length: Option<u64>, has_body: bool) -> i64 {
    match content_length {
        Some(length) if length > 0 => i64::try_from(length).unwrap_or(i64::MAX),
        _ if has_body => -1,
        _ => 0,
    }
}

/// The wait a record reports, in whole milliseconds the way core's
/// `time.Duration.Milliseconds` truncates.
pub fn waited_millis(waited: Duration) -> i64 {
    i64::try_from(waited.as_millis()).unwrap_or(i64::MAX)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_a_write_is_probed() {
        for (method, expected) in [
            ("POST", true),
            ("PUT", true),
            ("PATCH", true),
            ("GET", false),
            ("HEAD", false),
            ("DELETE", false),
            ("OPTIONS", false),
            ("post", false),
        ] {
            assert_eq!(
                probed(&Request::new(method, "/v1/chat/completions")),
                expected,
                "{method}"
            );
        }
    }

    #[test]
    fn the_collectors_own_exports_are_not_probed() {
        for path in [
            "/v1/logs",
            "/v1/traces",
            "/v1/metrics",
            // The trailing-separator spelling, which `path.Base` also reads as
            // the element before it.
            "/v1/logs/",
            "/otel/v1/metrics",
        ] {
            assert!(!probed(&Request::new("POST", path)), "{path}");
        }
        for path in [
            "/v1/chat/completions",
            "/v2/logs",
            "/v1/logs/archive",
            "/v10/metrics",
        ] {
            assert!(probed(&Request::new("POST", path)), "{path}");
        }
    }

    #[test]
    fn the_environment_is_parse_bools_set() {
        for (value, expected) in [
            ("1", Some(true)),
            ("t", Some(true)),
            ("T", Some(true)),
            ("TRUE", Some(true)),
            ("true", Some(true)),
            ("True", Some(true)),
            ("0", Some(false)),
            ("f", Some(false)),
            ("F", Some(false)),
            ("FALSE", Some(false)),
            ("false", Some(false)),
            ("False", Some(false)),
            // What a reader might expect to work and does not.
            ("yes", None),
            ("on", None),
            ("", None),
            ("2", None),
            ("tRuE", None),
        ] {
            assert_eq!(parse_bool(value), expected, "{value:?}");
        }
    }

    #[test]
    fn the_environment_is_read_trimmed() {
        assert!(enabled_from(Some(" true ")));
        assert!(
            !enabled_from(Some("yes")),
            "a value ParseBool refuses is off"
        );
        assert!(!enabled_from(Some("")), "an empty value is an error");
        assert!(!enabled_from(None), "unset is off");
    }

    #[test]
    fn request_bytes_declares_and_never_reads() {
        assert_eq!(request_bytes(Some(1024), true), 1024);
        assert_eq!(request_bytes(Some(0), true), -1, "a streamed body");
        assert_eq!(request_bytes(None, true), -1);
        assert_eq!(request_bytes(Some(0), false), 0);
        assert_eq!(request_bytes(None, false), 0);
    }

    #[test]
    fn waits_are_truncated_to_whole_milliseconds() {
        assert_eq!(waited_millis(Duration::from_micros(1999)), 1);
        assert_eq!(waited_millis(Duration::from_millis(1500)), 1500);
        assert_eq!(waited_millis(Duration::ZERO), 0);
    }
}
