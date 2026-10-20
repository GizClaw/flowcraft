//! `core/secret` and the reference grammar behind it: a `${secret:...}` span is
//! a *lazy* reference, resolved at use time, and it never renders its value.
//!
//! The crate has four pieces, and they fail differently on purpose:
//!
//! - the **scheme** ([`SecretResolver::expand`]) runs at build time. It reads a
//!   reference path and emits a [`SecretRef`] marker rather than a value, so a
//!   deployment gets build-time validation of the store and name with
//!   request-time resolution. Its refusals are prefixed
//!   `resource settings expand: `.
//! - **resolution** ([`SecretResolver::resolve`]) runs at use time, asks a
//!   [`SecretStore`], and prefixes its refusals `resource secret: `. The two
//!   prefixes are different families because the two seams are reached from
//!   different places; `rust/conformance/secret-contract.json` keeps them apart.
//! - the **value** ([`Secret`]) is what a settings field holds: a literal or a
//!   reference, [`Display`](std::fmt::Display)-ing and `Debug`-ing as
//!   `<secret>`, and serializing as `"<secret>"` -- every verb redacts, because
//!   a secret leaks through the one spelling nobody compared.
//! - the **stores** core ships, [`env`] and [`file`], behind the strict settings
//!   decode each performs and the [`loader`] a file store reads through. Both
//!   answer "no such secret" as `Ok(None)` and a store failure as `Err`, so the
//!   resolver can tell the two apart.
//!
//! # What the port changed
//!
//! - **A `lookup` answers `Result<Option<String>>`.** core returns
//!   `(value, found, error)`; "the store has no such secret" and "the store
//!   failed" are different answers, and `Option` says which without a third
//!   return value that can contradict the first.
//! - **Optional interfaces became default methods.** core's `SecretStoreID` is a
//!   type assertion beside `SecretStore`; here it is
//!   [`SecretStore::secret_store_id`], defaulting to `None`, and the default
//!   flag is [`SecretStore::default_secret_store`].
//! - **No `context.Context`.** The trait is synchronous: core threads a context
//!   through `Lookup` for telemetry only, and the stores core ships read an
//!   environment variable or a file.
//! - **`Secret` decodes with [`Secret::from_json`].** core gets there through
//!   `encoding/json`'s `UnmarshalJSON`, and the rules that fall out of it are
//!   not obvious: `null` is the *empty literal* rather than a bad shape, field
//!   names match case-insensitively, and a field of the wrong type is the shape
//!   refusal rather than the blank one. The contract has a row per rule. The
//!   `Deserialize` impl is the same code, for the deploy documents that will hold
//!   a `Secret` field.
//!
//! # What is deliberately not here yet
//!
//! The resource registry the two stores are registered in (core's
//! `secret.Register`), the TTL cache `core/deploy` wraps around every store, and
//! the wiring rules the deploy builder applies (id fallback to the resource name,
//! duplicate ids, one default). The contract's fourth table covers the stores
//! themselves; the wiring arrives with the deploy builder.

pub mod env;
pub mod file;
pub mod loader;
pub mod resolver;
pub mod store;
pub mod value;

use flowcraft_contract::error::{Error, ErrorKind};
use serde::Deserialize;

pub use env::EnvSettings;
pub use file::FileSettings;
pub use loader::{DEFAULT_MAX_BYTES, Loader, Source};
pub use resolver::{SecretRef, SecretResolver};
pub use store::{RESOURCE_KIND, SecretStore, SecretStoreFunc, Store};
pub use value::Secret;

/// The crate's result type: a [`flowcraft_contract::Error`], whose text is
/// core's sentence.
pub type Result<T> = std::result::Result<T, Error>;

/// `resource secret: `'s constant: every refusal in this crate is a validation
/// error, including the one that wraps a backend failure -- core does not
/// reclassify a store outage, and neither does the port.
fn validation(message: impl Into<String>) -> Error {
    Error::new(ErrorKind::Validation, message.into())
}

/// `encoding/json`'s rule for `null`: it leaves the field untouched, so a `null`
/// is that field's zero value rather than a shape error. serde refuses it, so
/// every scalar in a settings subtree core decodes with `resource.DecodeTyped`
/// takes its `Deserialize` through here.
///
/// Unknown *fields* stay strict (`deny_unknown_fields`, core's
/// `DisallowUnknownFields`); this is only about a field that is present and
/// null.
pub(crate) fn go_null<'de, D, T>(deserializer: D) -> std::result::Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}
