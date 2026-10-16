//! The provider-neutral wire DTOs shared by every chat turn: a role-tagged
//! [`Message`] carrying an ordered [`Content`] of [`Part`]s, the tool-call
//! DTOs, and the JSON Schema helpers behind a tool definition.
//!
//! core keeps this in `core/message`, and with the same rule: no runtime, no
//! providers, no executors. It is the canonical shape of what travels on the
//! wire, so the layers above it (inference for LLM calls, tool for execution,
//! memory for stored turns) operate on these types without owning them, and
//! two consumers can share a message log without pulling in each other's
//! runtime.
//!
//! What lands when, and why the split is where it is:
//!
//! - the media DTOs are here (see [`media`]): they ride inside multimodal
//!   parts, so nothing above can be shaped without them;
//! - the part union, the message/content types and the wire codec follow, and
//!   they are the reason this module exists: the wire format is JSON with each
//!   part tagged by its kind, and the port has to write the same bytes core
//!   writes and refuse the same documents core refuses;
//! - `message.Stream` (the delta transport inference streams through) and the
//!   schema *builders* land with the slices that use them: a stream is a
//!   runtime, and a builder is not a shape.

pub mod media;
