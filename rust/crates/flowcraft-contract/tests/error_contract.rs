//! Replays the committed error contract against the port.
//!
//! `rust/conformance/error-contract.json` is dumped from core by
//! `tools/conformance-oracle -error-contract` and checked in. The gate fails
//! if regenerating it produces anything else, so the file is core's answer;
//! this test is the other half, holding the port's answers to it row by row.
//! Between the two, a drift on either side is red.
//!
//! The samples carry their inputs, not just their answers, so this test
//! *reconstructs* each input (a chain, a classified error, a status) before
//! asking the port for its answer. Anything the file cannot express -- a
//! sample the port cannot build -- is a failure, not a skip.

use std::error::Error as StdError;
use std::fmt;

use flowcraft_contract::error::{Error, ErrorKind, http_status, kind_of};
use flowcraft_contract::provider::{
    ProviderCategory, classify_http_status, classify_provider, classify_provider_error,
    classify_status, provider_category_from_http_code,
};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/error-contract.json");

/// A chain element the samples build: its own text, and what is underneath.
///
/// Real chains are made of the port's errors, but a sample's *input* is only
/// ever its texts -- core's samples build them with `errors.New` and
/// `fmt.Errorf` -- so the harness uses the simplest thing that prints one.
#[derive(Debug)]
struct TextError {
    message: String,
    source: Option<Box<dyn StdError + Send + Sync + 'static>>,
}

impl fmt::Display for TextError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl StdError for TextError {
    fn source(&self) -> Option<&(dyn StdError + 'static)> {
        match &self.source {
            Some(source) => Some(&**source),
            None => None,
        }
    }
}

fn text(message: impl Into<String>) -> TextError {
    TextError {
        message: message.into(),
        source: None,
    }
}

fn text_over(
    message: impl Into<String>,
    source: Box<dyn StdError + Send + Sync + 'static>,
) -> TextError {
    TextError {
        message: message.into(),
        source: Some(source),
    }
}

/// The error a `classify_provider_error` sample hands in -- concrete, because
/// that function takes its input by value the way a caller's error arrives.
///
/// Two shapes, because the sample's classification has to sit *on the chain*
/// to be found: an unclassified element is a text element, a classified one
/// is the port's own [`Error`] (the type that carries a classification here).
/// A wrapper around either is a text element whose source is the classified
/// error, exactly as core's `fmt.Errorf("%s: %w", ...)` wraps one.
#[derive(Debug)]
enum SampleError {
    Text(TextError),
    Classified(Error),
}

/// The name the contract uses for a kind, both ways. A name this test does
/// not know is a failure: the file and the port have to move together.
fn kind_by_name(name: &str) -> ErrorKind {
    match name {
        "not_found" => ErrorKind::NotFound,
        "validation" => ErrorKind::Validation,
        "unauthorized" => ErrorKind::Unauthorized,
        "forbidden" => ErrorKind::Forbidden,
        "conflict" => ErrorKind::Conflict,
        "rate_limit" => ErrorKind::RateLimit,
        "budget_exceeded" => ErrorKind::BudgetExceeded,
        "policy_denied" => ErrorKind::PolicyDenied,
        "timeout" => ErrorKind::Timeout,
        "not_available" => ErrorKind::NotAvailable,
        "internal" => ErrorKind::Internal,
        "interrupted" => ErrorKind::Interrupted,
        "aborted" => ErrorKind::Aborted,
        other => panic!("the contract names a kind this test does not know: {other:?}"),
    }
}

fn kind_name(kind: ErrorKind) -> &'static str {
    match kind {
        ErrorKind::NotFound => "not_found",
        ErrorKind::Validation => "validation",
        ErrorKind::Unauthorized => "unauthorized",
        ErrorKind::Forbidden => "forbidden",
        ErrorKind::Conflict => "conflict",
        ErrorKind::RateLimit => "rate_limit",
        ErrorKind::BudgetExceeded => "budget_exceeded",
        ErrorKind::PolicyDenied => "policy_denied",
        ErrorKind::Timeout => "timeout",
        ErrorKind::NotAvailable => "not_available",
        ErrorKind::Internal => "internal",
        ErrorKind::Interrupted => "interrupted",
        ErrorKind::Aborted => "aborted",
        // Unreachable while every name above is known; the wildcard is what
        // `#[non_exhaustive]` asks for in a different crate.
        other => panic!("no contract name for {other:?}"),
    }
}

fn category_by_name(name: &str) -> ProviderCategory {
    match name {
        "transient" => ProviderCategory::Transient,
        "rate_limit" => ProviderCategory::RateLimit,
        "auth" => ProviderCategory::Auth,
        "billing" => ProviderCategory::Billing,
        "context_overflow" => ProviderCategory::ContextOverflow,
        "permanent" => ProviderCategory::Permanent,
        other => panic!("the contract names a category this test does not know: {other:?}"),
    }
}

fn category_name(category: ProviderCategory) -> &'static str {
    match category {
        ProviderCategory::Transient => "transient",
        ProviderCategory::RateLimit => "rate_limit",
        ProviderCategory::Auth => "auth",
        ProviderCategory::Billing => "billing",
        ProviderCategory::ContextOverflow => "context_overflow",
        ProviderCategory::Permanent => "permanent",
        other => panic!("no contract name for {other:?}"),
    }
}

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(doc) => doc,
        Err(err) => panic!("the committed contract does not parse: {err}"),
    }
}

fn field<'a>(value: &'a Value, name: &str) -> &'a Value {
    match value.get(name) {
        Some(found) => found,
        None => panic!("{value} has no field {name:?}"),
    }
}

fn rows<'a>(doc: &'a Value, section: &str) -> &'a [Value] {
    match field(doc, section).as_array() {
        Some(rows) => rows,
        None => panic!("section {section:?} is not an array"),
    }
}

fn str_field(value: &Value, name: &str) -> String {
    match field(value, name).as_str() {
        Some(found) => found.to_owned(),
        None => panic!("field {name:?} of {value} is not a string"),
    }
}

fn u16_field(value: &Value, name: &str) -> u16 {
    match field(value, name)
        .as_u64()
        .and_then(|code| u16::try_from(code).ok())
    {
        Some(found) => found,
        None => panic!("field {name:?} of {value} is not a 16-bit code"),
    }
}

fn label_of(row: &Value) -> String {
    str_field(row, "label")
}

/// The chain a sample names: `parts[0]` is outermost, and an empty list is a
/// plain unclassified error.
fn build_kind_chain(parts: &[String]) -> Box<dyn StdError + Send + Sync + 'static> {
    let mut chain: Box<dyn StdError + Send + Sync> = Box::new(text("probe"));
    for part in parts.iter().rev() {
        let message = chain.to_string();
        chain = Box::new(Error::with_source(kind_by_name(part), message, chain));
    }
    chain
}

/// The chain a `classify_provider` sample names: `parts[0]` is outermost.
fn build_text_chain(parts: &[String]) -> Box<dyn StdError + Send + Sync + 'static> {
    let mut chain: Option<Box<dyn StdError + Send + Sync>> = None;
    for part in parts.iter().rev() {
        chain = Some(match chain {
            Some(inner) => Box::new(text_over(part.clone(), inner)),
            None => Box::new(text(part.clone())),
        });
    }
    match chain {
        Some(built) => built,
        None => Box::new(text("")),
    }
}

/// `<text>`, optionally classified, optionally under a wrapper that prints
/// `"<prefix>: <text>"`.
fn build_inner(row: &Value) -> SampleError {
    let inner = field(row, "inner");
    let message = str_field(inner, "text");
    let classified = field(inner, "kind")
        .as_str()
        .map(|name| Error::new(kind_by_name(name), message.clone()));
    match (field(inner, "prefix").as_str(), classified) {
        (None, None) => SampleError::Text(text(message)),
        (None, Some(class)) => SampleError::Classified(class),
        (Some(prefix), None) => SampleError::Text(text_over(
            format!("{prefix}: {message}"),
            Box::new(text(message)),
        )),
        (Some(prefix), Some(class)) => {
            SampleError::Text(text_over(format!("{prefix}: {message}"), Box::new(class)))
        }
    }
}

/// Hands the sample in, in the two shapes above.
fn classify_inner(provider: &str, row: &Value) -> Error {
    match build_inner(row) {
        SampleError::Text(element) => classify_provider_error(provider, element),
        SampleError::Classified(error) => classify_provider_error(provider, error),
    }
}

/// Collects every mismatch before failing, so one run reports the whole
/// distance rather than the first row of it.
struct Failures {
    mismatches: Vec<String>,
}

impl Failures {
    fn new() -> Self {
        Failures {
            mismatches: Vec::new(),
        }
    }

    fn check<T: PartialEq + fmt::Debug>(&mut self, what: &str, expected: T, actual: T) {
        if expected != actual {
            self.mismatches.push(format!(
                "{what}: core says {expected:?}, the port says {actual:?}"
            ));
        }
    }

    fn finish(self) {
        assert!(
            self.mismatches.is_empty(),
            "the port does not answer what core answers:\n{}",
            self.mismatches.join("\n")
        );
    }
}

/// Every failure mode above is exercised by the two tests that follow, so the
/// harness itself is held to being able to fail.
#[test]
fn the_harness_reports_a_mismatch() {
    let mut failures = Failures::new();
    failures.check("a deliberate mismatch", 404u16, 400u16);
    assert_eq!(
        failures.mismatches.len(),
        1,
        "a wrong answer went unreported"
    );

    let mut failures = Failures::new();
    failures.check("an agreeing pair", "same", "same");
    failures.finish();
}

#[test]
fn the_contract_is_not_vacuous() {
    let doc = contract();
    for section in [
        "kinds",
        "http_status_priority",
        "provider_category_from_http_code",
        "classify_status",
        "classify_http_status",
        "classify_provider_error",
        "classify_provider",
    ] {
        assert!(
            rows(&doc, section).len() >= 5,
            "section {section:?} is too small to prove anything"
        );
    }
    assert_eq!(rows(&doc, "kinds").len(), 13, "one row per kind");
}

#[test]
fn kinds_answer_core_s_statuses() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "kinds") {
        let kind = kind_by_name(&str_field(row, "name"));
        failures.check(
            &str_field(row, "name"),
            u16_field(row, "http_status"),
            kind.http_status(),
        );
    }
    failures.finish();
}

#[test]
fn http_status_scans_the_way_core_scans() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "http_status_priority") {
        let parts: Vec<String> = match field(row, "chain").as_array() {
            Some(parts) => parts
                .iter()
                .map(|part| part.as_str().unwrap_or_default().to_owned())
                .collect(),
            None => panic!("{}: chain is not an array", label_of(row)),
        };
        let error = build_kind_chain(&parts);
        failures.check(
            &label_of(row),
            u16_field(row, "status"),
            http_status(&*error),
        );
    }
    failures.finish();
}

#[test]
fn the_code_axis_answers_core_s_table() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "provider_category_from_http_code") {
        let code = u16_field(row, "code");
        let expected = match field(row, "mapped").as_bool() {
            Some(true) => Some(category_by_name(&str_field(row, "category"))),
            Some(false) => None,
            None => panic!("{}: mapped is not a bool", code),
        };
        failures.check(
            &format!("code {code}"),
            expected,
            provider_category_from_http_code(code),
        );
    }
    failures.finish();
}

#[test]
fn classify_status_answers_core() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "classify_status") {
        let label = label_of(row);
        let classified = classify_status(u16_field(row, "code"), text("provider said no"));
        failures.check::<String>(
            &format!("{label} kind"),
            kind_name(kind_by_name(&str_field(row, "kind"))).to_owned(),
            kind_name(classified.kind()).to_owned(),
        );
        failures.check(
            &format!("{label} message"),
            str_field(row, "message"),
            classified.to_string(),
        );
    }
    failures.finish();
}

#[test]
fn classify_http_status_answers_core() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "classify_http_status") {
        let label = label_of(row);
        let classified = classify_http_status(
            &str_field(row, "provider"),
            u16_field(row, "code"),
            &str_field(row, "body"),
        );
        failures.check::<String>(
            &format!("{label} kind"),
            str_field(row, "kind"),
            kind_name(classified.kind()).to_owned(),
        );
        failures.check(
            &format!("{label} message"),
            str_field(row, "message"),
            classified.to_string(),
        );
    }
    failures.finish();
}

#[test]
fn classify_provider_error_answers_core() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "classify_provider_error") {
        let label = label_of(row);
        let classified = classify_inner(&str_field(row, "provider"), row);
        failures.check::<String>(
            &format!("{label} kind"),
            str_field(row, "kind"),
            kind_name(classified.kind()).to_owned(),
        );
        failures.check(
            &format!("{label} message"),
            str_field(row, "message"),
            classified.to_string(),
        );
    }
    failures.finish();
}

#[test]
fn classify_provider_answers_core() {
    let doc = contract();
    let mut failures = Failures::new();
    for row in rows(&doc, "classify_provider") {
        let label = label_of(row);
        let parts: Vec<String> = match field(row, "chain").as_array() {
            Some(parts) => parts
                .iter()
                .map(|part| part.as_str().unwrap_or_default().to_owned())
                .collect(),
            None => panic!("{label}: chain is not an array"),
        };
        let error = build_text_chain(&parts);
        failures.check::<String>(
            &label,
            str_field(row, "category"),
            category_name(classify_provider(&*error)).to_owned(),
        );
    }
    failures.finish();
}

/// The last thing the file pins, and the easiest to lose sight of: the ports'
/// answers are about chains, so a classification core would find anywhere on
/// one has to be findable here too. `kind_of` is what the harness leans on.
#[test]
fn classifications_are_found_through_foreign_wrappers() {
    let classified = text_over("wrapped", Box::new(Error::new(ErrorKind::Timeout, "slow")));
    assert_eq!(kind_of(&classified), Some(ErrorKind::Timeout));
}
