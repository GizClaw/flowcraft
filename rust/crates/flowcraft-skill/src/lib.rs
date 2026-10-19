//! `core/skill`: one `SKILL.md`, from bytes to metadata.
//!
//! A skill is a directory whose `SKILL.md` carries YAML frontmatter
//! (agentskills.io): a name, a description, and whatever else a third party
//! wrote. This module is the parse half — [`parse_file`] and [`parse_bytes`] —
//! and it exists on its own because that half is the part with opinions.
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
//! - Not ported, with where each belongs: discovery, ranking and the snapshot
//!   service (`discover.go`, `rank.go`, `render.go`, `service.go`) are the next
//!   slice; they read this module's metadata and add the filesystem and the BM25
//!   index on top.

mod parse;

pub use parse::{
    MAX_DESCRIPTION_LEN, MAX_NAME_LEN, MAX_SKILL_FILE_BYTES, Metadata, ParseResult, parse_bytes,
    parse_file,
};
