//! Strict document decoding.
//!
//! A document is JSON when its first non-whitespace byte is `{`, YAML
//! otherwise — the same sniff the Go side uses. Both paths land on a
//! [`serde_json::Value`] and then on a `deny_unknown_fields` struct, so an
//! unknown field is rejected identically regardless of input syntax.
//!
//! YAML is converted by hand instead of through a serde YAML backend:
//!
//! * duplicate keys are rejected by the parser;
//! * keys that are not strings are stringified, and two keys that collapse
//!   onto the same JSON key are **rejected** rather than resolved by iteration
//!   order (the decision recorded for #655);
//! * unknown tags are rejected instead of silently becoming strings;
//! * `yes` / `on` stay strings, matching YAML 1.2 rather than 1.1.

use saphyr::{LoadableYamlNode, Scalar, Yaml};
use serde::de::DeserializeOwned;
use serde_json::{Map, Number, Value};

use crate::error::{Error, Result};
use crate::resource::document::Deployment;

/// Decode a document from bytes.
pub fn deployment(bytes: &[u8]) -> Result<Deployment> {
    let text = std::str::from_utf8(bytes)
        .map_err(|error| Error::validation(format!("document is not UTF-8: {error}")))?;
    deployment_str(text)
}

/// Decode a document from text.
pub fn deployment_str(text: &str) -> Result<Deployment> {
    let value = document_value(text)?;
    let deployment: Deployment =
        settings(&value).map_err(|error| Error::new(error.kind(), format!("document: {error}")))?;
    deployment.validate()?;
    Ok(deployment)
}

/// Decode a typed settings subtree, naming the failing field path.
pub fn settings<T: DeserializeOwned>(value: &Value) -> Result<T> {
    serde_path_to_error::deserialize::<_, T>(value).map_err(|error| {
        let path = error.path().to_string();
        if path.is_empty() || path == "." {
            Error::validation(error.inner().to_string())
        } else {
            Error::validation(format!("{path}: {}", error.inner()))
        }
    })
}

/// The generic JSON view of a document, after sniffing its syntax.
pub fn document_value(text: &str) -> Result<Value> {
    if text.trim_start().starts_with('{') {
        return serde_json::from_str(text)
            .map_err(|error| Error::validation(format!("invalid JSON: {error}")));
    }
    yaml_value(text)
}

fn yaml_value(text: &str) -> Result<Value> {
    let documents = Yaml::load_from_str(text)
        .map_err(|error| Error::validation(format!("invalid YAML: {error}")))?;
    match documents.len() {
        0 => Err(Error::validation("document is empty")),
        1 => convert(&documents[0]),
        count => Err(Error::validation(format!(
            "document holds {count} YAML documents; exactly one is required"
        ))),
    }
}

fn convert(node: &Yaml<'_>) -> Result<Value> {
    match node {
        // Resolved scalar, e.g. a value position.
        Yaml::Value(scalar) => convert_scalar(scalar),
        // Unresolved scalar, e.g. a mapping key.
        Yaml::Representation(text, _, _) => Ok(Value::String(text.to_string())),
        Yaml::Sequence(items) => {
            let mut out = Vec::with_capacity(items.len());
            for item in items {
                out.push(convert(item)?);
            }
            Ok(Value::Array(out))
        }
        Yaml::Mapping(mapping) => {
            let mut out = Map::new();
            for (key, value) in mapping {
                let key = key_string(key)?;
                let value = convert(value)?;
                if out.contains_key(&key) {
                    return Err(Error::validation(format!(
                        "YAML keys collapse onto the same JSON key {key:?}"
                    )));
                }
                out.insert(key, value);
            }
            Ok(Value::Object(out))
        }
        Yaml::Tagged(tag, _) => Err(Error::validation(format!(
            "YAML tag {tag:?} is not supported"
        ))),
        Yaml::Alias(_) => Err(Error::validation("unresolved YAML alias")),
        Yaml::BadValue => Err(Error::validation("YAML value could not be resolved")),
    }
}

fn convert_scalar(scalar: &Scalar<'_>) -> Result<Value> {
    match scalar {
        Scalar::Null => Ok(Value::Null),
        Scalar::Boolean(value) => Ok(Value::Bool(*value)),
        Scalar::Integer(value) => Ok(Value::Number(Number::from(*value))),
        Scalar::FloatingPoint(value) => Number::from_f64(value.into_inner())
            .map(Value::Number)
            .ok_or_else(|| {
                Error::validation("YAML float is not representable in JSON (NaN or infinity)")
            }),
        Scalar::String(value) => Ok(Value::String(value.to_string())),
    }
}

fn key_string(node: &Yaml<'_>) -> Result<String> {
    match node {
        Yaml::Representation(text, _, _) => Ok(text.to_string()),
        Yaml::Value(Scalar::String(text)) => Ok(text.to_string()),
        Yaml::Value(Scalar::Null) => Ok("null".to_owned()),
        Yaml::Value(Scalar::Boolean(value)) => Ok(value.to_string()),
        Yaml::Value(Scalar::Integer(value)) => Ok(value.to_string()),
        Yaml::Value(Scalar::FloatingPoint(value)) => Ok(value.into_inner().to_string()),
        Yaml::Sequence(_) | Yaml::Mapping(_) => {
            Err(Error::validation("mapping keys must be scalars"))
        }
        Yaml::Tagged(tag, _) => Err(Error::validation(format!(
            "YAML tag {tag:?} on a mapping key is not supported"
        ))),
        Yaml::Alias(_) => Err(Error::validation("unresolved YAML alias in a mapping key")),
        Yaml::BadValue => Err(Error::validation("YAML mapping key could not be resolved")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const JSON: &str = r#"{
        "version": "v1",
        "resources": {
            "ws": {"kind": "workspace.Workspace", "impl": "local",
                   "settings": {"root": "./data"}}
        },
        "entries": ["ws"]
    }"#;

    const YAML: &str = r#"
version: v1
resources:
  ws:
    kind: workspace.Workspace
    impl: local
    settings:
      root: ./data
entries: [ws]
"#;

    #[test]
    fn json_and_yaml_reach_the_same_document() {
        let from_json = deployment_str(JSON).unwrap();
        let from_yaml = deployment_str(YAML).unwrap();
        assert_eq!(
            from_json.resources["ws"].kind.as_str(),
            from_yaml.resources["ws"].kind.as_str()
        );
        assert_eq!(from_json.entries, from_yaml.entries);
        assert_eq!(
            from_json.resources["ws"].settings,
            from_yaml.resources["ws"].settings
        );
    }

    #[test]
    fn unknown_fields_are_rejected() {
        let text = r#"{"resources": {"ws": {"kind": "k", "unknown": 1}}}"#;
        let error = deployment_str(text).unwrap_err();
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
        assert!(error.to_string().contains("unknown field"), "{error}");
    }

    #[test]
    fn missing_kind_is_rejected() {
        let error = deployment_str(r#"{"resources": {"ws": {}}}"#).unwrap_err();
        assert!(error.to_string().contains("missing field"), "{error}");
    }

    #[test]
    fn settings_are_object_only() {
        let text = r#"{"resources": {"ws": {"kind": "k", "settings": 3}}}"#;
        assert_eq!(
            deployment_str(text).unwrap_err().kind(),
            crate::error::ErrorKind::Validation
        );
    }

    #[test]
    fn multiple_yaml_documents_are_rejected() {
        let error = deployment_str("a: 1\n---\nb: 2\n").unwrap_err();
        assert!(error.to_string().contains("exactly one"), "{error}");
    }

    #[test]
    fn duplicate_yaml_keys_are_rejected() {
        let error = deployment_str("version: v1\nversion: v1\n").unwrap_err();
        assert!(error.to_string().contains("duplicated"), "{error}");
    }

    #[test]
    fn keys_that_collapse_onto_one_json_key_are_rejected() {
        // `1` and `"1"` are distinct YAML keys, one JSON key.
        let error = deployment_str("1: a\n\"1\": b\n").unwrap_err();
        assert!(error.to_string().contains("collapse"), "{error}");
    }

    #[test]
    fn yaml_one_one_booleans_stay_strings() {
        let value = document_value("a: yes\nb: on\nc: true\n").unwrap();
        assert_eq!(value["a"], Value::String("yes".to_owned()));
        assert_eq!(value["b"], Value::String("on".to_owned()));
        assert_eq!(value["c"], Value::Bool(true));
    }

    #[test]
    fn settings_errors_name_the_field_path() {
        #[derive(serde::Deserialize, Debug)]
        #[serde(deny_unknown_fields)]
        struct Settings {
            #[allow(dead_code)]
            root: String,
        }

        #[derive(serde::Deserialize, Debug)]
        #[serde(deny_unknown_fields)]
        struct Wrapper {
            #[allow(dead_code)]
            settings: Settings,
        }

        let value: Value = serde_json::json!({"settings": {"root": 1}});
        let error = settings::<Wrapper>(&value).unwrap_err();
        assert!(error.to_string().starts_with("settings.root:"), "{error}");
    }
}
