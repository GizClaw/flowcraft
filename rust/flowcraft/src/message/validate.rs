//! What makes a `flowcraft.message.v1` value well formed.
//!
//! Every rule here is about the type, not about who is reading it: a part with no
//! kind is invalid in a tool result and in a turn payload alike. Refusing it
//! twice is how the two owners drifted into different words for the same
//! refusal — and, in one case, into accepting what the other rejected.

use crate::error::{Error, Result};
use crate::proto::message as msg;

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

/// A content value must carry at least one typed part.
///
/// The partlessness is refused here and nowhere else. A producer that has
/// nothing to say passes an empty text part ([`crate::message::text_content`]),
/// which is a different statement from "no parts": the caller can tell an empty
/// answer from a malformed one.
pub fn validate_content(content: &msg::Content) -> Result<()> {
    if content.parts.is_empty() {
        return Err(Error::validation("content has no parts"));
    }
    if content.parts.iter().any(|part| part.kind.is_none()) {
        return Err(Error::validation("content has a part with no kind"));
    }
    Ok(())
}

/// Arguments are a JSON object, or the empty string for "no arguments".
///
/// `message.v1.ToolCall.arguments` documents an object, and every provider (the
/// local runner, MCP, the remote client's server) assumes one. A scalar or an
/// array is a caller bug, and it is refused here rather than handed to a provider
/// that would have to guess.
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

/// A canonical message: known role and at least one typed part.
pub fn validate_message(message: &msg::Message) -> Result<()> {
    let role = msg::Role::try_from(message.role)
        .map_err(|_| Error::validation(format!("unknown message role {}", message.role)))?;
    if role == msg::Role::Unspecified {
        return Err(Error::validation("message role is unspecified"));
    }
    let Some(content) = &message.content else {
        return Err(Error::validation("message has no content"));
    };
    validate_content(content)
}

/// A streaming fragment: exactly one kind.
pub fn validate_delta(delta: &msg::Delta) -> Result<()> {
    if delta.kind.is_none() {
        return Err(Error::validation("delta has no kind"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::message::text_content;

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
    fn a_content_value_needs_a_typed_part() {
        assert!(validate_content(&msg::Content { parts: Vec::new() }).is_err());
        assert!(
            validate_content(&msg::Content {
                parts: vec![msg::Part { kind: None }],
            })
            .is_err()
        );
        validate_content(&text_content("ok")).unwrap();
    }

    #[test]
    fn a_message_and_a_tool_result_agree_on_what_content_is_valid() {
        // The reason this module exists: one type, one rule. A content value the
        // tool capability accepts must not be one the ABI rejects.
        for content in [
            msg::Content { parts: Vec::new() },
            msg::Content {
                parts: vec![msg::Part { kind: None }],
            },
            text_content(""),
            text_content("ok"),
        ] {
            let message = msg::Message {
                role: msg::Role::User as i32,
                content: Some(content.clone()),
            };
            assert_eq!(
                validate_content(&content).is_ok(),
                validate_message(&message).is_ok(),
                "content validation and message validation disagree on {content:?}"
            );
        }
    }

    #[test]
    fn a_message_needs_a_known_role() {
        let content = text_content("hi");
        for (role, expected) in [
            (msg::Role::User as i32, true),
            (msg::Role::Assistant as i32, true),
            (msg::Role::Unspecified as i32, false),
            (99, false),
        ] {
            let message = msg::Message {
                role,
                content: Some(content.clone()),
            };
            assert_eq!(validate_message(&message).is_ok(), expected, "role {role}");
        }
        assert!(
            validate_message(&msg::Message {
                role: msg::Role::User as i32,
                content: None,
            })
            .is_err()
        );
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

    #[test]
    fn a_delta_needs_a_kind() {
        assert!(validate_delta(&msg::Delta { kind: None }).is_err());
        validate_delta(&msg::Delta {
            kind: Some(msg::delta::Kind::Text("x".to_owned())),
        })
        .unwrap();
    }
}
