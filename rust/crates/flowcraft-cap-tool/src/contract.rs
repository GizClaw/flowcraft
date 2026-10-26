//! The tool-source contract.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_proto::message as msg;
use flowcraft_resource::Resource;

/// The registry kind.
pub const KIND: &str = "tool.Source";
/// The cross-process contract identity.
pub const CONTRACT: &str = "tool.ToolSource/v1";

/// A named set of tools: list what is available, run one call.
#[async_trait]
pub trait ToolSource: Send + Sync {
    /// Every tool this source exposes.
    ///
    /// Order is not significant: the endpoint sorts by name before a peer sees
    /// the catalog, so a source does not have to promise an order.
    async fn list(&self) -> Result<Vec<msg::ToolDefinition>>;

    /// Run `name` with `arguments` (a JSON object, empty for no arguments).
    async fn execute(&self, name: &str, arguments: &str) -> Result<msg::Content>;
}

/// The value a factory of this capability produces.
#[derive(Clone)]
pub struct Handle(pub Arc<dyn ToolSource>);

impl Handle {
    /// Wrap a provider.
    #[must_use]
    pub fn new(source: Arc<dyn ToolSource>) -> Self {
        Self(source)
    }

    /// The provider.
    #[must_use]
    pub fn source(&self) -> Arc<dyn ToolSource> {
        self.0.clone()
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("tool.Handle")
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn ToolSource>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl Resource for Handle {}

/// A tool definition must be addressable by the model.
pub fn validate_definition(definition: &msg::ToolDefinition) -> Result<()> {
    let name = definition.name.trim();
    if name.is_empty() {
        return Err(Error::validation("tool definition has an empty name"));
    }
    if name != definition.name {
        return Err(Error::validation(format!(
            "tool name {:?} has surrounding whitespace",
            definition.name
        )));
    }
    if name.len() > 128 {
        return Err(Error::validation(format!(
            "tool name {name:?} is longer than 128 bytes"
        )));
    }
    // The same alphabet the Go SDK accepts, minus whitespace: a name travels
    // through model output, grants, and logs, so it stays boring.
    if !name
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.'))
    {
        return Err(Error::validation(format!(
            "tool name {name:?} may only contain letters, digits, '.', '-', '_'"
        )));
    }
    // The schema is a JSON Schema object (the conversation model says so). An
    // empty schema is the historical shorthand for "no declared input", used by
    // code-only tools, so it stays accepted.
    if !definition.input_schema.is_empty()
        && !matches!(
            serde_json::from_slice::<serde_json::Value>(&definition.input_schema),
            Ok(serde_json::Value::Object(_))
        )
    {
        return Err(Error::validation(format!(
            "tool {name:?} has an input_schema that is not a JSON object"
        )));
    }
    Ok(())
}

/// A result must carry at least one typed part, or the caller sees an empty
/// turn it cannot explain.
pub fn validate_content(content: &msg::Content) -> Result<()> {
    if content.parts.is_empty() {
        return Err(Error::validation("tool result has no content parts"));
    }
    if content.parts.iter().any(|part| part.kind.is_none()) {
        return Err(Error::validation("tool result has a part with no kind"));
    }
    Ok(())
}

/// Arguments are a JSON object, or the empty string for "no arguments".
///
/// `message.v1.ToolCall.arguments` documents an object, and every provider in
/// this crate (local, MCP, the remote client's server) assumes one. A scalar or
/// an array is a caller bug, and it is refused here rather than handed to a
/// provider that would have to guess.
pub fn validate_arguments(arguments: &str) -> Result<()> {
    if arguments.trim().is_empty() {
        return Ok(());
    }
    match serde_json::from_str::<serde_json::Value>(arguments) {
        Ok(serde_json::Value::Object(_)) => Ok(()),
        Ok(_) => Err(Error::validation("tool arguments must be a JSON object")),
        Err(error) => Err(Error::validation(format!(
            "tool arguments must be JSON: {error}"
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    #[test]
    fn names_are_checked() {
        validate_definition(&definition("search")).unwrap();
        validate_definition(&definition("team.search_v2")).unwrap();
        for bad in ["", " ", "two words", "a/b", "héllo", " search", "search "] {
            assert!(validate_definition(&definition(bad)).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn an_input_schema_must_be_a_json_object() {
        let mut valid = definition("search");
        valid.input_schema = br#"{"type":"object"}"#.to_vec();
        validate_definition(&valid).unwrap();

        for bad in [&b"[1]"[..], b"\"text\"", b"{oops", b"null"] {
            let mut invalid = definition("search");
            invalid.input_schema = bad.to_vec();
            assert!(validate_definition(&invalid).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn a_result_needs_a_typed_part() {
        assert!(validate_content(&msg::Content { parts: Vec::new() }).is_err());
        assert!(
            validate_content(&msg::Content {
                parts: vec![msg::Part { kind: None }],
            })
            .is_err()
        );
        validate_content(&msg::Content {
            parts: vec![msg::Part {
                kind: Some(msg::part::Kind::Text(msg::TextPart {
                    text: "ok".to_owned(),
                })),
            }],
        })
        .unwrap();
    }

    #[test]
    fn arguments_must_be_json_object_or_empty() {
        validate_arguments("").unwrap();
        validate_arguments("  ").unwrap();
        validate_arguments("{}").unwrap();
        validate_arguments(r#"{"query":"x"}"#).unwrap();
        for bad in ["{not json", "[1]", "\"text\"", "1", "null"] {
            assert!(validate_arguments(bad).is_err(), "{bad:?}");
        }
    }
}
