//! The `flowcraft.message.v1` model: its shape rules, and the constructors and
//! accessors every producer of a message needs.
//!
//! The types are generated ([`crate::proto::message`]); this module is the
//! hand-written half. It exists because the model has three owners — the tool
//! capability for results, the craft ABI for turn payloads, and the MCP adapter
//! for tool output — and each of them used to spell the same rules its own way:
//! a partless content value was refused by two validators with two different
//! messages and normalised by a third, and "the text of a content value" meant
//! "every text part joined" to one caller and "the first text part" to another.
//!
//! So the model's rules live here, once. A capability keeps its *policy* — what a
//! `ToolResult` means, how a truncation budget spends parts, which refusal it
//! reports — and no longer keeps its own copy of the model.

#![cfg_attr(test, allow(clippy::unwrap_used, clippy::expect_used))]

mod text;
mod validate;

pub use text::{
    definition_bytes, first_text, for_each_text, replace_text, text_content, text_message, text_of,
};
pub use validate::{
    validate_arguments, validate_content, validate_definition, validate_delta, validate_message,
};
