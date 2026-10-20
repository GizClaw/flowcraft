//! [`Secret`]: a settings value that never renders its contents.

use serde::de::{Deserializer, Error as _};
use serde::ser::Serializer;
use serde::{Deserialize, Serialize};
use serde_json::value::RawValue;
use serde_json::{Map, Value};

use crate::resolver::{SecretRef, SecretResolver};
use crate::{Result, validation};

/// What every verb prints instead of the value.
const REDACTED: &str = "<secret>";

/// A settings value that never renders its contents: either a literal string or
/// a lazy reference resolved through a [`SecretResolver`] at use time.
///
/// [`Display`](std::fmt::Display), [`Debug`], and `Serialize` all answer
/// `<secret>` (the wire form is the JSON string `"<secret>"`), so a secret does
/// not leak through logs, errors, or snapshots. Rust has no fourth verb to close
/// the way core closes `%#v` with `GoString`; `Debug` covers `{:?}` and `{:#?}`
/// both.
///
/// The redaction makes a failed comparison unreadable, which is the point: a
/// test that diffs two `Secret`s sees two `<secret>`s and has to compare
/// [`Secret::reference`] or a resolved value instead.
#[derive(Clone, PartialEq, Eq)]
pub struct Secret {
    kind: Kind,
}

/// The two shapes a settings value can have.
#[derive(Clone, PartialEq, Eq)]
enum Kind {
    /// A plain value, carried as written.
    Literal(String),
    /// A marker: which store, which name.
    Reference(SecretRef),
}

impl Secret {
    /// Wraps a plain value as a non-referencing secret.
    pub fn literal(value: impl Into<String>) -> Self {
        Self {
            kind: Kind::Literal(value.into()),
        }
    }

    /// Wraps a marker as a lazy reference.
    pub fn reference(reference: SecretRef) -> Self {
        Self {
            kind: Kind::Reference(reference),
        }
    }

    /// Reports whether this is a lazy reference rather than a literal.
    pub fn is_ref(&self) -> bool {
        matches!(self.kind, Kind::Reference(_))
    }

    /// Returns the marker, when this is a reference.
    pub fn reference_parts(&self) -> Option<&SecretRef> {
        match &self.kind {
            Kind::Reference(reference) => Some(reference),
            Kind::Literal(_) => None,
        }
    }

    /// Returns the secret's value. A literal returns itself; a reference is
    /// looked up through `resolver` now.
    ///
    /// `resolver` is `None` when the factory was handed no
    /// [`Input.Secrets`](crate::SecretResolver) -- a deployment with no secret
    /// store. A literal still resolves; a reference is refused, naming the
    /// secret it could not reach.
    pub fn resolve(&self, resolver: Option<&SecretResolver>) -> Result<String> {
        match &self.kind {
            Kind::Literal(value) => Ok(value.clone()),
            Kind::Reference(reference) => match resolver {
                Some(resolver) => resolver.resolve(&reference.store, &reference.name),
                None => Err(validation(format!(
                    "resource secret: no resolver for {}",
                    flowcraft_utils::decode::go_quote(&reference.name)
                ))),
            },
        }
    }

    /// Decodes the wire form: a JSON string is a literal, a
    /// `{"store":...,"name":...}` object is a reference.
    ///
    /// The rules are `encoding/json`'s, because core's `UnmarshalJSON` runs
    /// through it, and three of them are not guessable:
    ///
    /// - `null` decodes as the **empty literal**. Unmarshalling JSON null into a
    ///   string target is a no-op that returns no error, so the string branch
    ///   wins before the reference branch is tried. It is not the blank-reference
    ///   refusal, and it is not a bad shape.
    /// - Field names match **case-insensitively** (`{"Store":...}` decodes), with
    ///   an exact match preferred.
    /// - A field that is neither a string nor null fails the struct decode, so
    ///   the document gets the catch-all refusal quoting it, not the
    ///   blank-reference one. A null field is the no-op it is elsewhere, which
    ///   leaves that half blank and lands on the blank-reference refusal.
    ///
    /// Refusals are core's sentences, and the catch-all quotes the document *as
    /// written*: the text handed in, not a re-rendering of the parsed value.
    pub fn from_json(text: &str) -> Result<Self> {
        let value: Value = serde_json::from_str(text)
            .map_err(|err| validation(format!("resource secret: {err}")))?;
        match value {
            Value::String(literal) => Ok(Self::literal(literal)),
            Value::Null => Ok(Self::literal("")),
            Value::Object(fields) => Self::from_fields(&fields, text),
            _ => Err(wrong_shape(text)),
        }
    }

    /// The reference branch of [`Secret::from_json`].
    fn from_fields(fields: &Map<String, Value>, text: &str) -> Result<Self> {
        let store = field(fields, "store");
        let name = field(fields, "name");
        // The struct decode fails on a field of the wrong type before core ever
        // looks at the blanks, so the shape refusal comes first.
        if matches!(store, Field::Wrong) || matches!(name, Field::Wrong) {
            return Err(wrong_shape(text));
        }
        let Field::Text(store) = store else {
            return Err(validation(
                "resource secret: reference must carry store and name",
            ));
        };
        let Field::Text(name) = name else {
            return Err(validation(
                "resource secret: reference must carry store and name",
            ));
        };
        if store.trim().is_empty() || name.trim().is_empty() {
            return Err(validation(
                "resource secret: reference must carry store and name",
            ));
        }
        Ok(Self::reference(SecretRef {
            store: store.to_string(),
            name: name.to_string(),
        }))
    }
}

/// One reference field, as `encoding/json` would decode it.
enum Field<'a> {
    /// Absent, or JSON null: `encoding/json` leaves the destination alone.
    Blank,
    /// The field's text.
    Text(&'a str),
    /// A value a string cannot take, so the whole struct decode fails.
    Wrong,
}

/// Reads one field: an exact key wins over a case-folded one.
fn field<'a>(fields: &'a Map<String, Value>, name: &str) -> Field<'a> {
    let Some(value) = fields.get(name).or_else(|| {
        fields
            .iter()
            .find(|(key, _)| key.eq_ignore_ascii_case(name))
            .map(|(_, value)| value)
    }) else {
        return Field::Blank;
    };
    match value {
        Value::Null => Field::Blank,
        Value::String(text) => Field::Text(text),
        _ => Field::Wrong,
    }
}

/// `resource secret: want string or {...}, got <the document as written>`.
fn wrong_shape(text: &str) -> crate::Error {
    validation(format!(
        "resource secret: want string or {{\"store\":...,\"name\":...}}, got {text}"
    ))
}

impl std::fmt::Display for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(REDACTED)
    }
}

impl std::fmt::Debug for Secret {
    /// Redacted for `{:?}` and `{:#?}`: without this a derived `Debug` would
    /// print the marker -- or the literal -- field by field.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(REDACTED)
    }
}

impl Serialize for Secret {
    fn serialize<S: Serializer>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error> {
        serializer.serialize_str(REDACTED)
    }
}

impl<'de> Deserialize<'de> for Secret {
    /// The same rules as [`Secret::from_json`], for the deploy documents that
    /// will hold a `Secret` field.
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> std::result::Result<Self, D::Error> {
        let raw = Box::<RawValue>::deserialize(deserializer)?;
        Self::from_json(raw.get()).map_err(D::Error::custom)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flowcraft_contract::error::ErrorKind;

    #[test]
    fn unparseable_text_is_a_validation_error() {
        let refusal = Secret::from_json("{").err().unwrap_or_else(|| {
            panic!("an unterminated document decoded");
        });
        assert_eq!(refusal.kind(), ErrorKind::Validation);
    }

    #[test]
    fn a_reference_keeps_its_parts_as_written() {
        let secret = Secret::from_json(r#"{"store":" env ","name":" TOKEN "}"#)
            .unwrap_or_else(|err| panic!("{err}"));
        assert!(secret.is_ref());
        assert_eq!(
            secret.reference_parts(),
            Some(&SecretRef {
                store: " env ".to_string(),
                name: " TOKEN ".to_string(),
            }),
            "the decode keeps space; expand is what trims the name"
        );
    }
}
