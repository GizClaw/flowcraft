//! Provider error normalisation: the reverse mapping.
//!
//! [`crate::error`] is the forward direction -- an error that knows its kind
//! answers with an HTTP status. This module is the other one, for the SDK
//! boundary: a status a transport saw, or an arbitrary error coming back from
//! an upstream, turned into a classification the rest of the port can act on.
//! Every driver needs the same table, so it lives here rather than in each of
//! them.
//!
//! Two entry points, as in core:
//!
//! - [`classify_status`] / [`classify_http_status`] -- the caller drove the
//!   HTTP exchange itself and holds the status code;
//! - [`classify_provider_error`] -- the caller holds an error and nothing
//!   else, and the classification has to be read out of it.
//!
//! [`ProviderCategory`] is the finer-grained axis underneath both. It carries
//! what the kinds fold away -- 401 and 403 both surface as
//! Unauthorized/Forbidden on the wire, 402 and 403 both as Forbidden,
//! context overflow and a plain 400 both as Validation -- so a caller
//! choosing a cooldown can still tell them apart.
//!
//! Not ported: core's `HTTPStatusCoder` interface. `ClassifyProvider` tests
//! for it first, because an SDK error type that carries its status is the
//! most reliable signal. Rust cannot discover a foreign subtrait through
//! `&dyn Error` (no `downcast_ref::<dyn Trait>`), and it does not have to:
//! every transport in this repo drives its own HTTP and calls
//! [`classify_status`] with the status in hand -- nothing outside core's own
//! tests implements the interface today. A caller that does hold a status and
//! wants the coarse axis asks [`provider_category_from_http_code`] directly.

use std::error::Error as StdError;

use crate::error::{Error, ErrorKind, kind_of};

/// The classification axis underneath the kinds.
///
/// Declaration order is core's iota order; `#[non_exhaustive]` because core
/// can grow a category without that being a breaking change here.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum ProviderCategory {
    /// A network flake or a 5xx: the attempt is void, retrying is reasonable.
    Transient,
    /// The upstream is rate-limiting this caller (429).
    RateLimit,
    /// 401 or 403: the credential, or the right to use it, was refused.
    Auth,
    /// 402: the account cannot pay, which retrying does not fix.
    Billing,
    /// The input no longer fits: shrink it and send it again.
    ContextOverflow,
    /// 400, 404, 405, 422: the request itself is wrong in a way that repeats.
    Permanent,
}

/// core's `providerKeywordClassifiers`, in its order.
///
/// The order is the contract, not an implementation detail: the scan stops at
/// the first hit, so context-overflow keywords are listed before the plain
/// "http 400" they would otherwise be read as, and "unauthorized" before a
/// generic rate-limit match.
const KEYWORD_CLASSIFIERS: &[(ProviderCategory, &[&str])] = &[
    (
        ProviderCategory::ContextOverflow,
        &[
            "context length exceeded",
            "maximum context length",
            "too many tokens",
            "content too large",
            "request too large",
            "payload too large",
            "max_tokens",
        ],
    ),
    (
        ProviderCategory::Auth,
        &[
            "invalid api key",
            "incorrect api key",
            "unauthorized",
            "authentication",
            "permission denied",
            "api key not found",
            "invalid_api_key",
            "invalid x-api-key",
        ],
    ),
    (
        ProviderCategory::Billing,
        &[
            "insufficient credits",
            "insufficient credit",
            "billing",
            "payment required",
            "quota exceeded",
            "insufficient_quota",
            "no credit",
        ],
    ),
    (
        ProviderCategory::RateLimit,
        &[
            "rate limit",
            "rate_limit",
            "too many requests",
            "overloaded",
            "resource_exhausted",
            "at capacity",
            "capacity exceeded",
            "server is busy",
            "currently overloaded",
        ],
    ),
];

/// The category a bare HTTP status implies, or `None` when it implies
/// nothing -- the caller then applies its own default, the same way core's
/// `(ProviderTransient, false)` tells it to.
///
/// Note what is *not* here, because it is not in core either: 409, 408 and
/// 504 have no category. Going the other way, `ClassifyStatus` reads 409 as a
/// Conflict and 408/504 as Timeouts -- the two directions disagree on those
/// statuses on purpose, and `error-contract.json` pins both answers.
pub fn provider_category_from_http_code(code: u16) -> Option<ProviderCategory> {
    match code {
        401 | 403 => Some(ProviderCategory::Auth),
        402 => Some(ProviderCategory::Billing),
        429 => Some(ProviderCategory::RateLimit),
        400 | 404 | 405 | 422 => Some(ProviderCategory::Permanent),
        _ => None,
    }
}

/// Classify an HTTP status the caller's own transport observed, keeping
/// `cause` on the chain.
///
/// core's `ClassifyStatus`, with its mapping: 400/404/405/422 are validation,
/// 401 unauthorized, 402/403 forbidden (a billing or authorization refusal is
/// permanent, not a retryable auth failure), 409 conflict, 429 rate limit,
/// 408/504 timeout, and everything else -- including 5xx -- not-available,
/// which the router may retry.
pub fn classify_status<E>(code: u16, cause: E) -> Error
where
    E: StdError + Send + Sync + 'static,
{
    let kind = match code {
        400 | 404 | 405 | 422 => ErrorKind::Validation,
        401 => ErrorKind::Unauthorized,
        402 | 403 => ErrorKind::Forbidden,
        409 => ErrorKind::Conflict,
        429 => ErrorKind::RateLimit,
        408 | 504 => ErrorKind::Timeout,
        _ => ErrorKind::NotAvailable,
    };
    Error::with_source(kind, cause.to_string(), Box::new(cause))
}

/// Classify a raw status plus body, building the message core builds:
/// `"<provider>: http <code>"`, and `": <body>"` appended when there is a
/// body.
///
/// core's `ClassifyHTTPStatus`, for callers driving HTTP themselves. The
/// status mapping is [`classify_status`]'s, except where core's is: below 400
/// the helper assumes the caller already gated on a non-2xx response and
/// answers Internal, 403 is Forbidden rather than a retryable auth failure,
/// and a 4xx with no category (409 included) is Internal while a 5xx is
/// NotAvailable.
pub fn classify_http_status(provider: &str, code: u16, body: &str) -> Error {
    let mut message = format!("{provider}: http {code}");
    if !body.is_empty() {
        message = format!("{message}: {body}");
    }
    let kind = if code < 400 {
        ErrorKind::Internal
    } else if code == 403 {
        // Ahead of the category axis, which folds 403 onto Auth and would
        // make a permanent authorization refusal look retryable.
        ErrorKind::Forbidden
    } else {
        match provider_category_from_http_code(code) {
            Some(ProviderCategory::Auth) => ErrorKind::Unauthorized,
            Some(ProviderCategory::Billing) => ErrorKind::Forbidden,
            Some(ProviderCategory::RateLimit) => ErrorKind::RateLimit,
            Some(ProviderCategory::Permanent) => ErrorKind::Validation,
            // A code never implies these two; when one arrives anyway it
            // carries no opinion, so the 5xx rule decides -- the same
            // fall-through core's switch takes.
            None | Some(ProviderCategory::Transient | ProviderCategory::ContextOverflow) => {
                if code >= 500 {
                    ErrorKind::NotAvailable
                } else {
                    ErrorKind::Internal
                }
            }
        }
    };
    Error::new(kind, message)
}

/// The finer-grained category of an arbitrary error, read from the text of
/// every error on its chain.
///
/// core's `ClassifyProvider`, minus its first step (see the module docs). What
/// remains is the part the samples in `error-contract.json` pin: the keyword
/// scan over the lowercased chain text, then the embedded
/// `"http NNN"` / `"status NNN"` code, then Transient.
pub fn classify_provider(error: &(dyn StdError + 'static)) -> ProviderCategory {
    let message = chain_message(error);
    for &(category, keywords) in KEYWORD_CLASSIFIERS {
        for &keyword in keywords {
            if message.contains(keyword) {
                return category;
            }
        }
    }
    if let Some(code) = find_status_code(&message)
        && let Some(category) = provider_category_from_http_code(code)
    {
        return category;
    }
    ProviderCategory::Transient
}

/// Classify a provider error into a kind, prefixing the message with
/// `provider` the way core's `fmt.Errorf("%s: %w", ...)` does.
///
/// core's `ClassifyProviderError`: the error must not already carry a
/// classification, and when it does, the kind and the text come through
/// unchanged -- no provider prefix, no re-reading. That guard is the reason a
/// `Timeout` raised earlier cannot be downgraded to `NotAvailable` here; it is
/// checked against the whole chain, so a classification underneath somebody
/// else's wrapper counts too.
pub fn classify_provider_error<E>(provider: &str, error: E) -> Error
where
    E: StdError + Send + Sync + 'static,
{
    if let Some(kind) = kind_of(&error) {
        return Error::with_source(kind, error.to_string(), Box::new(error));
    }
    let kind = match classify_provider(&error) {
        ProviderCategory::Auth => ErrorKind::Unauthorized,
        ProviderCategory::Billing => ErrorKind::Forbidden,
        ProviderCategory::RateLimit => ErrorKind::RateLimit,
        ProviderCategory::ContextOverflow | ProviderCategory::Permanent => ErrorKind::Validation,
        ProviderCategory::Transient => ErrorKind::NotAvailable,
    };
    Error::with_source(kind, format!("{provider}: {error}"), Box::new(error))
}

/// The lowercased text of every error on the chain, joined with a space --
/// core's `collectErrorMessages`.
///
/// The join is visible in the classification: core concatenates the texts of
/// a chain, so a keyword can straddle a boundary ("context length" +
/// "exceeded" reads as the overflow keyword), and the same input classified
/// element by element would not match. `error-contract.json` has a sample for
/// exactly that.
fn chain_message(error: &(dyn StdError + 'static)) -> String {
    let mut message = String::new();
    let mut current = Some(error);
    while let Some(err) = current {
        if !message.is_empty() {
            message.push(' ');
        }
        // core lowercases with Go's simple case mapping, this uses Rust's
        // full one; they part ways only where a full mapping expands a
        // character (U+0130), and every keyword above is ASCII.
        message.push_str(&err.to_string().to_lowercase());
        current = err.source();
    }
    message
}

/// The first `\b(?:http|status)\s*(\d{3})\b` in the lowercased text.
///
/// Hand-written rather than pulled in as a regex, because Go's `\s`, `\d` and
/// `\b` are ASCII and a Rust regex would need those same flags spelled out --
/// and then the care would have gone into the flags instead of the rules.
/// The rules, one for one with core's pattern:
///
/// - a word boundary before the keyword: no preceding byte, or a preceding
///   byte that is not `[0-9A-Za-z_]`;
/// - then `http` or `status`;
/// - then any run of `[\t\n\f\r ]` -- Go's `\s`, which is *not* `\v` too;
/// - then exactly three ASCII digits;
/// - then a word boundary: the text ends, or the next byte is not
///   `[0-9A-Za-z_]`.
fn find_status_code(message: &str) -> Option<u16> {
    const KEYS: [&str; 2] = ["http", "status"];
    let bytes = message.as_bytes();
    let mut start = 0;
    while start < bytes.len() {
        if start > 0 && is_word_byte(bytes[start - 1]) {
            start += 1;
            continue;
        }
        let Some(key) = KEYS
            .iter()
            .find(|key| bytes[start..].starts_with(key.as_bytes()))
        else {
            start += 1;
            continue;
        };
        let mut digits = start + key.len();
        while digits < bytes.len() && matches!(bytes[digits], b'\t' | b'\n' | 0x0c | b'\r' | b' ') {
            digits += 1;
        }
        if digits + 3 <= bytes.len()
            && bytes[digits..digits + 3].iter().all(u8::is_ascii_digit)
            && (digits + 3 == bytes.len() || !is_word_byte(bytes[digits + 3]))
        {
            let code = u16::from(bytes[digits] - b'0') * 100
                + u16::from(bytes[digits + 1] - b'0') * 10
                + u16::from(bytes[digits + 2] - b'0');
            return Some(code);
        }
        start += 1;
    }
    None
}

/// `\b`'s notion of a word byte, which is ASCII in Go's regexp.
fn is_word_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || byte == b'_'
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fmt;

    /// A foreign error with a message and an optional cause.
    #[derive(Debug)]
    struct Plain {
        message: String,
        source: Option<Box<dyn StdError + Send + Sync + 'static>>,
    }

    impl Plain {
        fn new(message: impl Into<String>) -> Self {
            Plain {
                message: message.into(),
                source: None,
            }
        }

        fn wrapping(
            message: impl Into<String>,
            source: impl StdError + Send + Sync + 'static,
        ) -> Self {
            Plain {
                message: message.into(),
                source: Some(Box::new(source)),
            }
        }
    }

    impl fmt::Display for Plain {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.write_str(&self.message)
        }
    }

    impl StdError for Plain {
        fn source(&self) -> Option<&(dyn StdError + 'static)> {
            match &self.source {
                Some(source) => Some(&**source),
                None => None,
            }
        }
    }

    #[test]
    fn the_code_axis_maps_what_core_maps() {
        for (code, want) in [
            (401, Some(ProviderCategory::Auth)),
            (403, Some(ProviderCategory::Auth)),
            (402, Some(ProviderCategory::Billing)),
            (429, Some(ProviderCategory::RateLimit)),
            (400, Some(ProviderCategory::Permanent)),
            (404, Some(ProviderCategory::Permanent)),
            (405, Some(ProviderCategory::Permanent)),
            (422, Some(ProviderCategory::Permanent)),
            // The statuses the other direction knows and this one does not.
            (408, None),
            (409, None),
            (418, None),
            (500, None),
            (503, None),
            (504, None),
            (200, None),
        ] {
            assert_eq!(provider_category_from_http_code(code), want, "code {code}");
        }
    }

    #[test]
    fn classify_status_keeps_the_cause_and_its_text() {
        let err = classify_status(429, Plain::new("provider said no"));
        assert_eq!(err.kind(), ErrorKind::RateLimit);
        assert_eq!(err.to_string(), "provider said no");
        let Some(cause) = err.source() else {
            panic!("the cause should stay on the chain");
        };
        assert!(cause.downcast_ref::<Plain>().is_some());
    }

    #[test]
    fn classify_status_maps_every_branch() {
        for (code, want) in [
            (400, ErrorKind::Validation),
            (404, ErrorKind::Validation),
            (405, ErrorKind::Validation),
            (422, ErrorKind::Validation),
            (401, ErrorKind::Unauthorized),
            (402, ErrorKind::Forbidden),
            (403, ErrorKind::Forbidden),
            (409, ErrorKind::Conflict),
            (429, ErrorKind::RateLimit),
            (408, ErrorKind::Timeout),
            (504, ErrorKind::Timeout),
            (500, ErrorKind::NotAvailable),
            (503, ErrorKind::NotAvailable),
            // What the switch does not name is transient, 2xx included.
            (418, ErrorKind::NotAvailable),
            (200, ErrorKind::NotAvailable),
        ] {
            assert_eq!(
                classify_status(code, Plain::new("x")).kind(),
                want,
                "code {code}"
            );
        }
    }

    #[test]
    fn classify_http_status_builds_core_s_message() {
        let bare = classify_http_status("openai", 429, "");
        assert_eq!(bare.kind(), ErrorKind::RateLimit);
        assert_eq!(bare.to_string(), "openai: http 429");

        let with_body = classify_http_status("openai", 429, "slow down");
        assert_eq!(with_body.to_string(), "openai: http 429: slow down");
    }

    #[test]
    fn classify_http_status_keeps_core_s_two_readings_apart() {
        // 403 stays a permanent refusal rather than a retryable auth failure.
        assert_eq!(
            classify_http_status("p", 403, "").kind(),
            ErrorKind::Forbidden
        );
        assert_eq!(
            classify_http_status("p", 401, "").kind(),
            ErrorKind::Unauthorized
        );
        // 409 has no category, so it is not a Conflict through this door.
        assert_eq!(
            classify_http_status("p", 409, "").kind(),
            ErrorKind::Internal
        );
        // 408 and 504 are not Timeouts through it either...
        assert_eq!(
            classify_http_status("p", 408, "").kind(),
            ErrorKind::Internal
        );
        assert_eq!(
            classify_http_status("p", 504, "").kind(),
            ErrorKind::NotAvailable
        );
        // ...while below 400 nothing is classified, caller having gated.
        assert_eq!(
            classify_http_status("p", 301, "").kind(),
            ErrorKind::Internal
        );
    }

    #[test]
    fn keywords_are_scanned_in_core_s_order() {
        // The overflow keyword sits inside an "http 400" that would otherwise
        // read as permanent.
        assert_eq!(
            classify_provider(&Plain::new("http 400: maximum context length exceeded")),
            ProviderCategory::ContextOverflow
        );
        assert_eq!(
            classify_provider(&Plain::new("http 400: unauthorized access")),
            ProviderCategory::Auth
        );
        assert_eq!(
            classify_provider(&Plain::new("api error: http 400")),
            ProviderCategory::Permanent
        );
    }

    #[test]
    fn keywords_are_folded_and_matched_anywhere() {
        assert_eq!(
            classify_provider(&Plain::new("Invalid API Key")),
            ProviderCategory::Auth
        );
        assert_eq!(
            classify_provider(&Plain::new("resource_exhausted")),
            ProviderCategory::RateLimit
        );
        assert_eq!(
            classify_provider(&Plain::new("insufficient_quota")),
            ProviderCategory::Billing
        );
        assert_eq!(
            classify_provider(&Plain::new("max_tokens must be at most 4096")),
            ProviderCategory::ContextOverflow
        );
    }

    #[test]
    fn the_chain_text_is_joined_before_scanning() {
        // Neither element carries the keyword on its own.
        let split = Plain::wrapping("context length", Plain::new("exceeded"));
        assert_eq!(classify_provider(&split), ProviderCategory::ContextOverflow);

        let nested = Plain::wrapping("outer", Plain::new("rate limit exceeded"));
        assert_eq!(classify_provider(&nested), ProviderCategory::RateLimit);
    }

    #[test]
    fn embedded_codes_are_scanned_the_way_core_scans_them() {
        for (text, want) in [
            ("got http 429", ProviderCategory::RateLimit),
            ("status 401 from upstream", ProviderCategory::Auth),
            ("http\t429", ProviderCategory::RateLimit),
            ("http\n429", ProviderCategory::RateLimit),
            ("http429", ProviderCategory::RateLimit),
            // No boundary, wrong length, wrong word, wrong whitespace.
            ("httpx 429", ProviderCategory::Transient),
            ("http 4291", ProviderCategory::Transient),
            ("estatus 429", ProviderCategory::Transient),
            ("http\u{b}429", ProviderCategory::Transient),
            // A code with no category says nothing.
            ("status 503", ProviderCategory::Transient),
            ("connection refused", ProviderCategory::Transient),
            ("", ProviderCategory::Transient),
        ] {
            assert_eq!(classify_provider(&Plain::new(text)), want, "{text:?}");
        }
    }

    #[test]
    fn classify_provider_error_prefixes_like_core() {
        let err = classify_provider_error("openai", Plain::new("connection refused"));
        assert_eq!(err.kind(), ErrorKind::NotAvailable);
        assert_eq!(err.to_string(), "openai: connection refused");

        let err = classify_provider_error("openai", Plain::new("insufficient credits"));
        assert_eq!(err.kind(), ErrorKind::Forbidden);
        assert_eq!(err.to_string(), "openai: insufficient credits");
    }

    #[test]
    fn an_existing_classification_survives_untouched() {
        // Timeout must not be downgraded to NotAvailable on the way out.
        let err = classify_provider_error(
            "openai",
            Error::new(ErrorKind::Timeout, "openai.generate: 30s"),
        );
        assert_eq!(err.kind(), ErrorKind::Timeout);
        assert_eq!(err.to_string(), "openai.generate: 30s");

        // Even under somebody else's wrapper, and with no provider prefix:
        // the wrapper says what a formatter would have made it print.
        let wrapped = Plain::wrapping(
            "wrapped: rate limit hit",
            Error::new(ErrorKind::RateLimit, "rate limit hit"),
        );
        let err = classify_provider_error("openai", wrapped);
        assert_eq!(err.kind(), ErrorKind::RateLimit);
        assert_eq!(err.to_string(), "wrapped: rate limit hit");
    }
}
