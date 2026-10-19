//! `core/skill/parse.go`: one `SKILL.md`, from bytes to metadata.

use std::fs;
use std::path::Path;

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_utils::decode::{self, LenientRefusal};
use flowcraft_utils::pathsafe;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

/// The longest name a directive may carry, in runes.
pub const MAX_NAME_LEN: usize = 64;

/// The longest description kept, in runes.
pub const MAX_DESCRIPTION_LEN: usize = 1024;

/// The largest `SKILL.md` read at all, in bytes. Discovery and the body readers
/// apply the same cap: a third-party skill must not be able to put an unbounded
/// document into the model's context.
pub const MAX_SKILL_FILE_BYTES: u64 = 256 << 10;

/// One skill parsed from a `SKILL.md`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Metadata {
    /// The skill's name: `[a-z0-9]+(-[a-z0-9]+)*`, at most [`MAX_NAME_LEN`] runes.
    pub name: String,
    /// The one-line description a per-turn ranking ranks on.
    pub description: String,
    /// The non-standard `metadata.short-description`, when a skill carries one.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub short_description: String,
    /// The canonical path of the `SKILL.md` this came from.
    pub path: String,
}

/// The parsed metadata plus what was accepted with a note.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ParseResult {
    /// What a caller reads.
    pub metadata: Metadata,
    /// The shape issues the parse accepted, in the order it met them.
    pub warnings: Vec<String>,
}

/// Parses one `SKILL.md`: frontmatter split, YAML decode, validation.
///
/// A missing or invalid name falls back to the parent directory's slug with a
/// warning; only a malformed file is refused. The sentences are core's.
pub fn parse_file(path: impl AsRef<Path>) -> Result<ParseResult, Error> {
    let path = path.as_ref();
    let info = fs::metadata(path).map_err(|err| os_failure("stat", path, &err))?;
    if info.len() > MAX_SKILL_FILE_BYTES {
        // core says `skill: SKILL.md exceeds the 262144-byte limit` here and
        // names the path at the second check in `parse_bytes`; the difference is
        // core's, and a caller reads either sentence the same way.
        return Err(refused(format!(
            "skill: SKILL.md exceeds the {MAX_SKILL_FILE_BYTES}-byte limit"
        )));
    }
    let data = fs::read(path).map_err(|err| os_failure("read", path, &err))?;
    parse_bytes(path, &data)
}

/// Parses `SKILL.md` content somebody already read.
pub fn parse_bytes(path: &Path, data: &[u8]) -> Result<ParseResult, Error> {
    if data.len() as u64 > MAX_SKILL_FILE_BYTES {
        return Err(refused(format!(
            "skill: {} exceeds the {MAX_SKILL_FILE_BYTES}-byte SKILL.md limit",
            path.display()
        )));
    }
    let parts = split_frontmatter(data)?;
    if String::from_utf8_lossy(parts.body).trim().is_empty() {
        return Err(refused("skill: SKILL.md body is empty".into()));
    }
    // The lenient conversion is the typed decode's: it keeps the last value of a
    // repeated key silently, loads a collapse with a note, and refuses what no
    // target could read. See `flowcraft_utils::decode::to_json_lenient`.
    let frontmatter = decode::to_json_lenient(parts.front).map_err(|refusal| {
        refused(format!(
            "skill: parse frontmatter: {}",
            refusal_sentence(&refusal)
        ))
    })?;
    let root: Value = serde_json::from_str(&frontmatter.json)
        .map_err(|err| refused(format!("skill: parse frontmatter: {err}")))?;
    let mut fields = Frontmatter::read(&root)
        .map_err(|sentence| refused(format!("skill: parse frontmatter: {sentence}")))?;

    fields.name = single_line(&fields.name);
    fields.description = single_line(&fields.description);
    let directory = fallback_directory(path);
    let fallback = slugify(&directory);
    let mut warnings: Vec<String> = frontmatter
        .collisions
        .iter()
        .map(|collision| format!("frontmatter: {collision}"))
        .collect();
    if !valid_name(&fields.name) {
        let note = if fields.name.is_empty() {
            "name missing or invalid".to_owned()
        } else {
            format!("name {} invalid", decode::quote(&fields.name))
        };
        if fallback.is_empty() {
            return Err(refused(format!(
                "skill: {note}; directory {} is not usable as a fallback",
                decode::quote(&directory)
            )));
        }
        fields.name.clone_from(&fallback);
        warnings.push(format!(
            "{note}; fell back to directory name {}",
            decode::quote(&fallback)
        ));
    } else if fields.name != fallback && !fallback.is_empty() {
        // The standard asks for name == directory name; third-party skills
        // commonly break it, so this is a note and not a refusal.
        warnings.push(format!(
            "name {} does not match directory name {} (accepted)",
            decode::quote(&fields.name),
            decode::quote(&fallback)
        ));
    }
    if fields.description.is_empty() {
        return Err(refused("skill: description is required".into()));
    }
    let description = truncate_runes(&fields.description, MAX_DESCRIPTION_LEN);

    Ok(ParseResult {
        metadata: Metadata {
            name: fields.name,
            description,
            short_description: single_line(&fields.short_description),
            path: pathsafe::clean(path).to_string_lossy().into_owned(),
        },
        warnings,
    })
}

/// A refusal, as core writes it: a sentence with `skill: ` in front.
fn refused(message: String) -> Error {
    Error::new(ErrorKind::Validation, message)
}

/// `stat <path>: <sentence>` or `read <path>: <sentence>`: core's verb and path,
/// then the operating system's own text, which is the platform's.
fn os_failure(verb: &str, path: &Path, err: &std::io::Error) -> Error {
    refused(format!("{verb} {}: {err}", path.display()))
}

/// The sentence inside a [`LenientRefusal`]: both variants carry core's words,
/// and a caller prefixes its own subject.
fn refusal_sentence(refusal: &LenientRefusal) -> &str {
    match refusal {
        LenientRefusal::UnusableKeys(sentence) | LenientRefusal::Document(sentence) => sentence,
    }
}

/// The two halves of a `SKILL.md`.
pub(crate) struct Parts<'a> {
    /// The bytes between the delimiters.
    pub(crate) front: &'a [u8],
    /// The Markdown after the closing delimiter's line.
    pub(crate) body: &'a [u8],
}

/// Extracts the frontmatter and the body.
///
/// Byte-wise on purpose: the YAML reader is the one that judges UTF-8, and it
/// has to see the bytes the file actually holds, not a replacement-character
/// version of them.
pub(crate) fn split_frontmatter(data: &[u8]) -> Result<Parts<'_>, Error> {
    // Editors write a BOM often enough that refusing the file would drop a
    // usable skill.
    let text = data
        .strip_prefix(b"\xef\xbb\xbf".as_slice())
        .unwrap_or(data);
    let Some(rest) = text.strip_prefix(b"---".as_slice()) else {
        return Err(refused(
            "skill: missing YAML frontmatter (expected --- ... ---)".into(),
        ));
    };
    let rest = rest
        .strip_prefix(b"\n".as_slice())
        .or_else(|| rest.strip_prefix(b"\r\n".as_slice()))
        .unwrap_or(rest);
    let Some(at) = find(rest, b"\n---") else {
        return Err(refused("skill: unterminated YAML frontmatter".into()));
    };
    Ok(Parts {
        front: &rest[..at],
        body: &rest[at + 4..],
    })
}

/// The first occurrence of `needle` in `haystack`.
fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

/// The fields of the frontmatter this parser reads.
#[derive(Debug, Default)]
struct Frontmatter {
    name: String,
    description: String,
    short_description: String,
}

impl Frontmatter {
    /// Reads the fields out of a converted document, the way
    /// `sigs.k8s.io/yaml` decodes into a struct: unknown keys are ignored, keys
    /// match case-insensitively, a scalar is coerced to the spelling a string
    /// field needs, and a null is the zero value.
    fn read(root: &Value) -> Result<Frontmatter, String> {
        let object = match root {
            Value::Null => return Ok(Frontmatter::default()),
            Value::Object(object) => object,
            other => {
                return Err(format!(
                    "json: cannot unmarshal {} into the frontmatter, which is a mapping",
                    json_kind(other)
                ));
            }
        };
        let mut fields = Frontmatter::default();
        if let Some(value) = lookup(object, "name") {
            fields.name = spelling(value, "name")?;
        }
        if let Some(value) = lookup(object, "description") {
            fields.description = spelling(value, "description")?;
        }
        match lookup(object, "metadata") {
            None | Some(Value::Null) => {}
            Some(Value::Object(metadata)) => {
                if let Some(value) = lookup(metadata, "short-description") {
                    fields.short_description = spelling(value, "short-description")?;
                }
            }
            Some(other) => {
                return Err(format!(
                    "json: cannot unmarshal {} into the metadata field, which is a mapping",
                    json_kind(other)
                ));
            }
        }
        Ok(fields)
    }
}

/// A key of a JSON object, exact match first and then case-insensitive, which is
/// `encoding/json`'s rule and therefore `sigs.k8s.io/yaml`'s.
fn lookup<'a>(object: &'a Map<String, Value>, field: &str) -> Option<&'a Value> {
    if let Some(value) = object.get(field) {
        return Some(value);
    }
    object
        .iter()
        .find(|(key, _)| key.eq_ignore_ascii_case(field))
        .map(|(_, value)| value)
}

/// A YAML scalar in a string field, as the spelling `sigs.k8s.io/yaml` gives it:
/// an integer in decimal after YAML 1.1's resolution, a float at float32
/// precision, a boolean as `true`/`false`, a null as the empty string. A
/// collection is the one thing a string field refuses.
fn spelling(value: &Value, field: &str) -> Result<String, String> {
    match value {
        Value::Null => Ok(String::new()),
        Value::String(text) => Ok(text.clone()),
        Value::Bool(flag) => Ok(flag.to_string()),
        Value::Number(number) => Ok(number_spelling(number)),
        other => Err(format!(
            "json: cannot unmarshal {} into the {field} field, which is a string",
            json_kind(other)
        )),
    }
}

/// A resolved YAML number, spelled the way Go writes it.
fn number_spelling(number: &serde_json::Number) -> String {
    if let Some(value) = number.as_i64() {
        return value.to_string();
    }
    if let Some(value) = number.as_u64() {
        return value.to_string();
    }
    decode::go_float32(number.as_f64().unwrap_or(f64::NAN))
}

/// What `encoding/json` calls a value in its own refusal.
fn json_kind(value: &Value) -> &'static str {
    match value {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

/// `filepath.Base(filepath.Dir(path))`: the directory name a missing name falls
/// back to.
///
/// A string walk rather than `Path::parent`, because the two disagree exactly
/// where the answer is quoted in a refusal: `filepath.Dir("SKILL.md")` is `.`
/// and `filepath.Base(".")` is `.`, where `parent()` is empty and `file_name()`
/// is `None`.
fn fallback_directory(path: &Path) -> String {
    let text = path.to_string_lossy();
    let at = text.as_bytes().iter().rposition(|byte| *byte == b'/');
    let directory = match at {
        Some(at) => pathsafe::clean(Path::new(&text[..=at])),
        None => pathsafe::clean(Path::new("")),
    };
    base(&directory)
}

/// `filepath.Base`: the last element of a cleaned path, with `/` for the root and
/// `.` for nothing.
fn base(path: &Path) -> String {
    let text = path.to_string_lossy();
    if text.is_empty() {
        return ".".into();
    }
    let trimmed = text.trim_end_matches('/');
    if trimmed.is_empty() {
        return "/".into();
    }
    match trimmed.rsplit_once('/') {
        Some((_, name)) => name.to_owned(),
        None => trimmed.to_owned(),
    }
}

/// `^[a-z0-9]+(?:-[a-z0-9]+)*$`, at most [`MAX_NAME_LEN`] runes.
fn valid_name(name: &str) -> bool {
    !name.is_empty() && name.chars().count() <= MAX_NAME_LEN && is_slug(name)
}

/// The name grammar without the length rule.
fn is_slug(name: &str) -> bool {
    let mut after_dash = true;
    for ch in name.chars() {
        match ch {
            'a'..='z' | '0'..='9' => after_dash = false,
            '-' if !after_dash => after_dash = true,
            _ => return false,
        }
    }
    // A name may not end on a dash, which `after_dash` records.
    !name.is_empty() && !after_dash
}

/// Lowercase, hyphens for everything else, no leading or trailing dash.
///
/// One deliberate difference from core, below the contract's resolution: Go's
/// `strings.ToLower` applies the *simple* case mapping, and this applies the
/// full one, so the two disagree about `İ` (Go: `i`, here: `i` plus a combining
/// dot, which becomes a dash). No row spells such a directory.
fn slugify(text: &str) -> String {
    let mut out = String::new();
    let mut after_dash = false;
    for ch in text.chars().flat_map(char::to_lowercase) {
        match ch {
            'a'..='z' | '0'..='9' => {
                out.push(ch);
                after_dash = false;
            }
            _ if !out.is_empty() && !after_dash => {
                out.push('-');
                after_dash = true;
            }
            _ => {}
        }
    }
    out.trim_matches('-').chars().take(MAX_NAME_LEN).collect()
}

/// `strings.Fields` joined by one space: every whitespace run becomes a single
/// space, and both ends are trimmed.
fn single_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// At most `limit` runes. The caps are stated in characters, so a byte slice —
/// which could cut a three-byte character in half or over-truncate CJK text — is
/// not good enough.
pub(crate) fn truncate_runes(text: &str, limit: usize) -> String {
    if text.chars().count() <= limit {
        return text.to_owned();
    }
    text.chars().take(limit).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The refusal text of a parse that has to refuse.
    fn refusal(path: &str, data: &[u8]) -> String {
        match parse_bytes(Path::new(path), data) {
            Ok(parsed) => panic!("{path} parsed as {:?}", parsed.metadata),
            Err(err) => err.to_string(),
        }
    }

    /// The answer of a parse that has to succeed.
    fn parsed(path: &str, data: &[u8]) -> ParseResult {
        match parse_bytes(Path::new(path), data) {
            Ok(parsed) => parsed,
            Err(err) => panic!("{path}: {err}"),
        }
    }

    #[test]
    fn the_file_cap_is_two_sentences() {
        // The first check is core's `ParseFile` sentence, the second its
        // `parseBytes` one; a caller reads either as the same refusal.
        let big = vec![b'a'; MAX_SKILL_FILE_BYTES as usize + 1];
        assert_eq!(
            refusal("big/SKILL.md", &big),
            "skill: big/SKILL.md exceeds the 262144-byte SKILL.md limit"
        );
        let directory =
            std::env::temp_dir().join(format!("flowcraft-skill-cap-{}", std::process::id()));
        fs::create_dir_all(&directory).unwrap_or_else(|err| panic!("{directory:?}: {err}"));
        let path = directory.join("SKILL.md");
        fs::write(&path, &big).unwrap_or_else(|err| panic!("{path:?}: {err}"));
        match parse_file(&path) {
            Ok(_) => panic!("an oversized file parsed"),
            Err(err) => assert_eq!(
                err.to_string(),
                "skill: SKILL.md exceeds the 262144-byte limit"
            ),
        }
        let _ = fs::remove_dir_all(&directory);
    }

    #[test]
    fn a_bare_file_name_falls_back_to_dot() {
        // `filepath.Dir("SKILL.md")` is ".", and "." slugifies to nothing, so the
        // refusal quotes "." and not an empty string.
        assert_eq!(
            refusal("SKILL.md", b"---\ndescription: d\n---\n\nbody\n"),
            "skill: name missing or invalid; directory \".\" is not usable as a fallback"
        );
    }

    #[test]
    fn a_name_is_folded_before_it_is_validated() {
        let parsed = parsed(
            "review/SKILL.md",
            b"---\nname: \"  review \"\ndescription: d\n---\n\nbody\n",
        );
        assert!(parsed.warnings.is_empty());
        assert_eq!(parsed.metadata.name, "review");
        assert_eq!(parsed.metadata.path, "review/SKILL.md");
    }

    #[test]
    fn a_crlf_file_splits_like_an_lf_one() {
        let parsed = parsed(
            "review/SKILL.md",
            b"---\r\nname: review\r\ndescription: Review\r\n---\r\n\r\nbody\r\n",
        );
        assert_eq!(parsed.metadata.description, "Review");
    }
}
