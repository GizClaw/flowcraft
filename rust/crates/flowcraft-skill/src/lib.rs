//! `core/skill`: the Agent Skills engine -- one `SKILL.md` from bytes to
//! metadata, and the registry that discovers, ranks and renders them.
//!
//! A skill is a directory whose `SKILL.md` carries YAML frontmatter
//! (agentskills.io): a name, a description, and whatever else a third party
//! wrote. The crate has two layers. The parse layer ([`parse_file`],
//! [`parse_bytes`]) is where the opinions are, because that is where
//! third-party input meets a prompt. The registry layer ([`discover`],
//! [`Service`], [`render_section`]) is rules about *which* files are read and
//! *how* they reach a turn.
//!
//! # The opinions, and whose they are
//!
//! The frontmatter is **third-party input**, and the parser is lenient exactly
//! where a real skill needs it to be: extension fields are ignored rather than
//! refused, a BOM is trimmed, a CRLF file splits like an LF one, a name that
//! disagrees with its directory is accepted with a warning, and a name that is
//! not a valid slug falls back to the directory's slugified name.
//!
//! It is strict where a model reads the result: the name is one closed shape
//! (`[a-z0-9]+(-[a-z0-9]+)*`, at most [`MAX_NAME_LEN`] characters), the
//! description is required and capped at [`MAX_DESCRIPTION_LEN`] characters, both
//! are folded to a single line, and no file past [`MAX_SKILL_FILE_BYTES`] is read
//! at all. Those are the values that end up in a prompt, so they are the values
//! that get a limit.
//!
//! # What the port changed
//!
//! - The typed decode is `sigs.k8s.io/yaml`'s, and the port reproduces its rule
//!   rather than Rust's: a YAML scalar landing in a string field is *coerced to
//!   its spelling* — an integer in decimal after YAML 1.1 resolution (`007` is
//!   `7`), a float at float32 precision (`0.000001` is `1e-06`), a boolean as
//!   `true`/`false`, a null as the empty string — and a collection is refused.
//!   Field names match case-insensitively, which is `encoding/json`'s rule.
//!   `rust/conformance/skill-contract.json` has a row per spelling.
//! - The conversion is [`flowcraft_utils::decode::to_json_lenient`] rather than
//!   `to_json`: a repeated key keeps its last value and says nothing, and two
//!   keys that spell one JSON key load with a note instead of refusing the skill.
//!   That is what the library underneath does, and dropping a usable skill over a
//!   collapse inside an extension block would be the port's own decision.
//! - Core returns these refusals unclassified (`fmt.Errorf`); the port calls them
//!   [`ErrorKind::Validation`], because a `SKILL.md` that does not parse is
//!   exactly that. The sentences are core's, so a caller's log does not change.
//! # The registry, and why its rules are separate
//!
//! Discovery is a filesystem walk over third-party content, so it is where the
//! port's safety rules live rather than in the application on top: hidden
//! entries are skipped, a symlink is followed only while it stays inside a
//! configured root, a directory is walked once by canonical path so an alias
//! cannot loop the walk, and a broken `SKILL.md` is a diagnostic rather than a
//! failed pass. Ranking is BM25 over names and descriptions (see
//! `flowcraft-utils`), and rendering is one line per skill -- a body is never
//! inlined.
//!
//! # What the registry slice changed
//!
//! - Cancellation is a `Fn() -> bool` argument, not a carried `context.Context`;
//!   the runtime that will own one is a later slice. [`discover`] never cancels,
//!   [`discover_with`] and [`Service::reload_with`] take the check.
//! - The snapshot is an `RwLock<Arc<..>>` rather than an `atomic.Pointer`, so the
//!   crate gains no dependency for the same read-mostly guarantee.
//! - Refusals are named (`NotFound` for a missing skill, `Validation` for a
//!   filesystem one) where core returns them unclassified, and the sentences are
//!   core's. The index-build warning core logs has no sink yet; the registry
//!   still degrades to unranked rather than to empty.
//! - Not ported, with where each belongs: `telemetry.WarnErr` (the telemetry
//!   slice), and nothing else -- `discover.go`, `rank.go`, `render.go` and
//!   `service.go` are all here.

mod discover;
mod parse;
mod rank;
mod render;
mod service;

pub use discover::{Diagnostic, Outcome, discover, discover_with};
pub use parse::{
    MAX_DESCRIPTION_LEN, MAX_NAME_LEN, MAX_SKILL_FILE_BYTES, Metadata, ParseResult, parse_bytes,
    parse_file,
};
pub use rank::{Scored, mention_names};
pub use render::{RENDER_HEADER, render_section};
pub use service::{DEFAULT_TOP_N, Options, Service};
