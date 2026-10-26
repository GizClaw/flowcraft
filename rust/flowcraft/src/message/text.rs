//! The small shapes inside a message: text parts, and the readings of them.
//!
//! Two readings are kept on purpose, because they answer different questions and
//! the difference used to be silent — one caller concatenated every text part,
//! another took the first one and dropped the rest, both calling it "the text":
//!
//! - [`text_of`] is what a reader of the message sees: every text part, in order.
//! - [`first_text`] is a one-line summary — an error, a log field, an audit
//!   record — where the first part is the whole point and the others are not
//!   wanted.

use crate::proto::message as msg;

/// A content value that is one text part.
///
/// This is also the answer for "nothing to say": see
/// [`crate::message::validate_content`] for why an empty text part is not the
/// same statement as no parts at all.
pub fn text_content(text: impl Into<String>) -> msg::Content {
    msg::Content {
        parts: vec![text_part(text)],
    }
}

/// A message that is one text part.
#[must_use]
pub fn text_message(role: msg::Role, text: &str) -> msg::Message {
    msg::Message {
        role: role as i32,
        content: Some(text_content(text)),
    }
}

/// Every text part, concatenated: what a reader of the content sees.
#[must_use]
pub fn text_of(content: &msg::Content) -> String {
    content
        .parts
        .iter()
        .filter_map(|part| match part.kind.as_ref() {
            Some(msg::part::Kind::Text(text)) => Some(text.text.as_str()),
            _ => None,
        })
        .collect()
}

/// The first text part, for a one-line summary of a value that may hold several.
#[must_use]
pub fn first_text(content: &msg::Content) -> Option<&str> {
    content
        .parts
        .iter()
        .find_map(|part| match part.kind.as_ref() {
            Some(msg::part::Kind::Text(text)) => Some(text.text.as_str()),
            _ => None,
        })
}

/// Mutably visit every text part, skipping the typed ones.
pub fn for_each_text(content: &mut msg::Content, mut visit: impl FnMut(&mut msg::TextPart)) {
    for part in &mut content.parts {
        if let Some(msg::part::Kind::Text(text)) = part.kind.as_mut() {
            visit(text);
        }
    }
}

/// Replace the text parts with one text part, keeping the typed ones.
///
/// Used by truncation, which rewrites the textual body and must not touch an
/// image or an audio part it did not cut.
pub fn replace_text(content: &mut msg::Content, text: String) {
    let mut parts = Vec::with_capacity(content.parts.len() + 1);
    parts.push(text_part(text));
    for part in content.parts.drain(..) {
        if !matches!(part.kind, Some(msg::part::Kind::Text(_))) {
            parts.push(part);
        }
    }
    content.parts = parts;
}

/// Approximate serialized size of a definition, for budget pruning.
///
/// An estimate, not a measurement: the two short strings and the schema are what
/// a model sees, and 32 bytes covers the protobuf framing of the message that
/// carries them. A definition that fits is not sent; one that does not is pruned
/// before anything is serialized, so the constant only has to be stable.
#[must_use]
pub fn definition_bytes(definition: &msg::ToolDefinition) -> i64 {
    (definition.name.len() + definition.description.len() + definition.input_schema.len() + 32)
        as i64
}

fn text_part(text: impl Into<String>) -> msg::Part {
    msg::Part {
        kind: Some(msg::part::Kind::Text(msg::TextPart { text: text.into() })),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn content_with(parts: Vec<msg::Part>) -> msg::Content {
        msg::Content { parts }
    }

    #[test]
    fn text_content_is_one_text_part() {
        let content = text_content("hello");
        assert_eq!(content.parts.len(), 1);
        assert_eq!(text_of(&content), "hello");
    }

    #[test]
    fn text_of_joins_every_text_part_and_skips_the_rest() {
        let content = content_with(vec![
            text_part("a"),
            msg::Part {
                kind: Some(msg::part::Kind::Image(msg::ImagePart {
                    source: Some(msg::MediaSource {
                        media_type: "image/png".to_owned(),
                        from: Some(msg::media_source::From::Data(vec![1, 2, 3])),
                    }),
                })),
            },
            text_part("b"),
        ]);
        assert_eq!(text_of(&content), "ab");
        assert_eq!(first_text(&content), Some("a"));
    }

    #[test]
    fn first_text_is_none_without_a_text_part() {
        let content = content_with(vec![msg::Part { kind: None }]);
        assert_eq!(first_text(&content), None);
        assert_eq!(text_of(&content), "");
    }

    #[test]
    fn replace_text_keeps_the_typed_parts() {
        let mut content = content_with(vec![
            text_part("before"),
            msg::Part {
                kind: Some(msg::part::Kind::Text(msg::TextPart {
                    text: "second".to_owned(),
                })),
            },
        ]);
        replace_text(&mut content, "after".to_owned());
        assert_eq!(text_of(&content), "after");
        assert_eq!(
            content.parts.len(),
            1,
            "both text parts collapse into the new one"
        );

        let mut with_image = content_with(vec![text_part("x")]);
        with_image.parts.push(msg::Part {
            kind: Some(msg::part::Kind::Image(msg::ImagePart {
                source: Some(msg::MediaSource {
                    media_type: "image/png".to_owned(),
                    from: Some(msg::media_source::From::Data(vec![0])),
                }),
            })),
        });
        replace_text(&mut with_image, "y".to_owned());
        assert_eq!(with_image.parts.len(), 2);
        assert!(matches!(
            with_image.parts[1].kind,
            Some(msg::part::Kind::Image(_))
        ));
    }

    #[test]
    fn for_each_text_visits_only_text() {
        let mut content = content_with(vec![text_part("a"), msg::Part { kind: None }]);
        let mut visited = 0;
        for_each_text(&mut content, |text| {
            visited += 1;
            text.text.push('!');
        });
        assert_eq!(visited, 1);
        assert_eq!(text_of(&content), "a!");
    }

    #[test]
    fn definition_bytes_grows_with_the_schema() {
        let mut definition = msg::ToolDefinition {
            name: "search".to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        };
        let bare = definition_bytes(&definition);
        definition.input_schema = vec![b'x'; 100];
        assert_eq!(definition_bytes(&definition), bare + 100);
    }
}
