//! The secret grammar contract: every reference, resolution and wire form in
//! `rust/conformance/secret-contract.json`, replayed.
//!
//! Three tables, because the grammar has three seams and they refuse in
//! different sentences: the scheme (`expand`) prefixes `resource settings
//! expand: `, use-time resolution prefixes `resource secret: `, and the value's
//! decode either keeps the document as a literal or refuses it in one of two
//! sentences. A row here is a path or a document and the answer core gave it;
//! the stores are canned tables, so nothing depends on the machine.
//!
//! The `wire` field is compared as a decoded JSON value rather than as text:
//! core writes it with `json.Marshal`, which HTML-escapes `<`, `>` and `&`, and
//! `serde_json` does not. The document says so in `masked`; what the port has to
//! reproduce is the string `"<secret>"`, and a test that compared the bytes would
//! be measuring an encoder option.

use std::collections::BTreeMap;
use std::sync::Arc;

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_secret::store::SecretStoreFunc;
use flowcraft_secret::{Secret, SecretRef, SecretResolver, SecretStore};
use serde::Deserialize;

/// The committed document.
#[derive(Deserialize)]
struct Contract {
    /// The scheme's table.
    refs: Vec<RefsCase>,
    /// The use-time table.
    resolves: Vec<ResolveCase>,
    /// The wire table.
    values: Vec<ValueCase>,
}

/// One store in a scenario: an id and the answers it gives.
#[derive(Debug, Clone, Deserialize)]
struct StoreSpec {
    /// The id the store is addressable by.
    id: String,
    /// Whether it claims the default flag.
    #[serde(default)]
    default: bool,
    /// The canned answers.
    #[serde(default)]
    answers: Vec<Lookup>,
}

/// One canned answer.
#[derive(Debug, Clone, Deserialize)]
struct Lookup {
    /// The name asked about.
    name: String,
    /// The value a found secret answers.
    #[serde(default)]
    value: String,
    /// `false` for a store that has no such secret.
    #[serde(default)]
    found: Option<bool>,
    /// A backend failure's sentence.
    #[serde(default)]
    error: String,
}

/// One path list put to one resolver.
#[derive(Debug, Deserialize)]
struct RefsCase {
    /// Identifies the row in a mismatch.
    name: String,
    /// The configured stores.
    stores: Vec<StoreSpec>,
    /// The target of NAME-only references.
    #[serde(default)]
    default_store: String,
    /// The reference paths.
    paths: Vec<String>,
    /// The scheme's answers.
    answers: Vec<RefAnswer>,
}

/// One path's answer.
#[derive(Debug, Deserialize)]
struct RefAnswer {
    /// The path asked about.
    path: String,
    /// The marker's store id.
    #[serde(default)]
    store: String,
    /// The marker's secret name.
    #[serde(default)]
    name: String,
    /// The refusal's text.
    #[serde(default)]
    error: String,
}

/// One store asked for one name.
#[derive(Debug, Deserialize)]
struct ResolveCase {
    /// Identifies the row in a mismatch.
    name: String,
    /// The configured stores.
    stores: Vec<StoreSpec>,
    /// The resolver's default store.
    #[serde(default)]
    default_store: String,
    /// Whether the row resolves with no resolver at all.
    #[serde(default)]
    no_resolver: bool,
    /// The store to resolve through.
    store: String,
    /// The name to resolve.
    secret_name: String,
    /// The resolved value.
    #[serde(default)]
    value: String,
    /// The refusal's text.
    #[serde(default)]
    error: String,
}

/// One settings document and what it became.
#[derive(Debug, Deserialize)]
struct ValueCase {
    /// Identifies the row in a mismatch.
    name: String,
    /// The JSON text of the settings value.
    doc: String,
    /// The stores the resolution runs against.
    #[serde(default)]
    stores: Vec<StoreSpec>,
    /// The resolver's default store.
    #[serde(default)]
    default_store: String,
    /// Whether the row resolves with no resolver at all.
    #[serde(default)]
    no_resolver: bool,
    /// Whether the document decoded.
    ok: bool,
    /// The decode refusal's text.
    #[serde(default)]
    error: String,
    /// Whether the decoded value is a lazy reference.
    #[serde(default)]
    r#ref: bool,
    /// `json.Marshal`'s output for the decoded value.
    #[serde(default)]
    wire: String,
    /// `fmt.Sprint`'s output.
    #[serde(default)]
    display: String,
    /// `fmt.Sprintf("%#v")`'s output.
    #[serde(default)]
    go_syntax: String,
    /// The value `Secret.Resolve` answered.
    #[serde(default)]
    resolved: String,
    /// The resolution refusal's text.
    #[serde(default)]
    resolve_error: String,
}

/// The document, parsed once.
fn contract() -> Contract {
    serde_json::from_str(include_str!("../../../conformance/secret-contract.json"))
        .unwrap_or_else(|err| panic!("secret-contract.json: {err}"))
}

/// Builds the stores a row names, each over its own canned answer table.
fn stores(specs: &[StoreSpec]) -> BTreeMap<String, Arc<dyn SecretStore>> {
    specs
        .iter()
        .map(|spec| (spec.id.clone(), canned(spec)))
        .collect()
}

/// One canned store, core's `resource.SecretStoreFunc` over a table.
fn canned(spec: &StoreSpec) -> Arc<dyn SecretStore> {
    let answers: BTreeMap<String, Lookup> = spec
        .answers
        .iter()
        .map(|answer| (answer.name.clone(), answer.clone()))
        .collect();
    let store: Arc<dyn SecretStore> = Arc::new(
        SecretStoreFunc::new(
            move |name: &str| -> flowcraft_secret::Result<Option<String>> {
                match answers.get(name) {
                    None => Ok(None),
                    Some(answer) if !answer.error.is_empty() => {
                        Err(Error::new(ErrorKind::Validation, answer.error.clone()))
                    }
                    Some(answer) if answer.found == Some(false) => Ok(None),
                    Some(answer) => Ok(Some(answer.value.clone())),
                }
            },
        )
        .with_default(spec.default),
    );
    store
}

/// Builds the resolver a row asks with, or `None` for a row that asks a factory
/// with no `Input.Secrets`.
fn resolver(specs: &[StoreSpec], default_store: &str, no_resolver: bool) -> Option<SecretResolver> {
    if no_resolver {
        return None;
    }
    Some(SecretResolver::new(
        stores(specs),
        default_store.to_string(),
    ))
}

/// Names the row and the field in a mismatch.
fn mismatch(
    row: &str,
    field: &str,
    got: impl std::fmt::Display,
    want: impl std::fmt::Display,
) -> ! {
    panic!("{row}: {field} = {got}, want {want}");
}

#[test]
fn refs_agree_with_the_contract() {
    for case in contract().refs {
        let resolver = SecretResolver::new(stores(&case.stores), case.default_store.clone());
        if case.answers.len() != case.paths.len() {
            mismatch(&case.name, "answers", case.answers.len(), case.paths.len());
        }
        for (path, answer) in case.paths.iter().zip(&case.answers) {
            if *path != answer.path {
                mismatch(&case.name, "path", path, &answer.path);
            }
            match (resolver.expand(path), answer.error.is_empty()) {
                (Ok(marker), true) => {
                    let want = SecretRef {
                        store: answer.store.clone(),
                        name: answer.name.clone(),
                    };
                    if marker != want {
                        mismatch(&case.name, path, format!("{marker:?}"), format!("{want:?}"));
                    }
                }
                (Ok(marker), false) => mismatch(
                    &case.name,
                    path,
                    format!("the marker {marker:?}"),
                    format!("the refusal {}", answer.error),
                ),
                (Err(err), _) => {
                    if answer.error.is_empty() {
                        mismatch(&case.name, path, &err, "a marker");
                    }
                    if err.to_string() != answer.error {
                        mismatch(&case.name, path, &err, &answer.error);
                    }
                }
            }
        }
    }
}

#[test]
fn resolves_agree_with_the_contract() {
    for case in contract().resolves {
        // core asks a *nil* `*SecretResolver` in the no-resolver row, and a nil
        // resolver is what a factory with no `Input.Secrets` holds. The port has
        // no nil resolver -- the same answer comes from `Secret.resolve(None)`,
        // which is where the sentence lives.
        let answer = match resolver(&case.stores, &case.default_store, case.no_resolver) {
            Some(resolver) => resolver.resolve(&case.store, &case.secret_name),
            None => Secret::reference(SecretRef {
                store: case.store.clone(),
                name: case.secret_name.clone(),
            })
            .resolve(None),
        };
        match (answer, case.error.is_empty()) {
            (Ok(value), true) => {
                if value != case.value {
                    mismatch(&case.name, "value", value, &case.value);
                }
            }
            (Ok(value), false) => mismatch(
                &case.name,
                "value",
                format!("the value {value:?}"),
                format!("the refusal {}", case.error),
            ),
            (Err(err), _) => {
                if case.error.is_empty() {
                    mismatch(&case.name, "value", &err, "a value");
                }
                if err.to_string() != case.error {
                    mismatch(&case.name, "value", &err, &case.error);
                }
            }
        }
    }
}

#[test]
fn values_agree_with_the_contract() {
    for case in contract().values {
        let secret = match (Secret::from_json(&case.doc), case.ok) {
            (Ok(secret), true) => secret,
            (Ok(secret), false) => mismatch(
                &case.name,
                "doc",
                format!("a {secret}"),
                format!("the refusal {}", case.error),
            ),
            (Err(err), _) => {
                if case.ok {
                    mismatch(&case.name, "doc", &err, "a decoded value");
                }
                if err.to_string() != case.error {
                    mismatch(&case.name, "doc", &err, &case.error);
                }
                continue;
            }
        };
        if secret.is_ref() != case.r#ref {
            mismatch(&case.name, "ref", secret.is_ref(), case.r#ref);
        }
        // The wire form is JSON text; compare what it decodes to, because the
        // escaping is the encoder's (`masked` says so).
        let wire = serde_json::to_string(&secret)
            .unwrap_or_else(|err| panic!("{}: marshal: {err}", case.name));
        let wire: serde_json::Value = serde_json::from_str(&wire)
            .unwrap_or_else(|err| panic!("{}: the wire form is not JSON: {err}", case.name));
        let want: serde_json::Value = serde_json::from_str(&case.wire)
            .unwrap_or_else(|err| panic!("{}: the contract's wire is not JSON: {err}", case.name));
        if wire != want {
            mismatch(&case.name, "wire", wire, want);
        }
        if secret.to_string() != case.display {
            mismatch(&case.name, "display", secret, &case.display);
        }
        if format!("{secret:?}") != case.go_syntax {
            mismatch(
                &case.name,
                "go_syntax",
                format!("{secret:?}"),
                &case.go_syntax,
            );
        }
        let resolver = resolver(&case.stores, &case.default_store, case.no_resolver);
        match (
            secret.resolve(resolver.as_ref()),
            case.resolve_error.is_empty(),
        ) {
            (Ok(value), true) => {
                if value != case.resolved {
                    mismatch(&case.name, "resolved", value, &case.resolved);
                }
            }
            (Ok(value), false) => mismatch(
                &case.name,
                "resolved",
                format!("the value {value:?}"),
                format!("the refusal {}", case.resolve_error),
            ),
            (Err(err), _) => {
                if case.resolve_error.is_empty() {
                    mismatch(&case.name, "resolved", &err, "a value");
                }
                if err.to_string() != case.resolve_error {
                    mismatch(&case.name, "resolved", &err, &case.resolve_error);
                }
            }
        }
    }
}

#[test]
fn every_refusal_in_the_contract_is_a_validation_error() {
    // The kind is not in the document (core has predicates, not a kind
    // accessor); it is pinned here, where the port can answer it. A backend
    // failure is the one that looks like it should be a not_available.
    for case in contract().resolves {
        // A row with no resolver is a `Secret.resolve(None)` row, not a
        // `SecretResolver::resolve` one; the kind table above covers it.
        if case.no_resolver {
            continue;
        }
        let resolver = SecretResolver::new(stores(&case.stores), case.default_store);
        match resolver.resolve(&case.store, &case.secret_name) {
            Ok(value) => {
                if !case.error.is_empty() {
                    panic!("{}: expected a refusal, got {value}", case.name);
                }
            }
            Err(err) => {
                if case.error.is_empty() {
                    panic!("{}: expected a value, got {err}", case.name);
                }
                assert_eq!(err.kind(), ErrorKind::Validation, "{}: {err}", case.name);
            }
        }
    }
    for case in contract().values {
        if let Err(err) = Secret::from_json(&case.doc) {
            assert_eq!(err.kind(), ErrorKind::Validation, "{}: {err}", case.name);
        }
    }
}
