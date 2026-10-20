//! `core/utils`'s decode layer, ported: the strict YAML/JSON conversion
//! (`utils.ToJSON`) and the sniffing rule that decides what to feed it
//! (`utils.FormatOf`).
//!
//! The Go implementation is the oracle. `rust/conformance` diffs this module
//! against it file by file, byte for byte, and every rule below cites the Go
//! behaviour it mirrors; the corpus in `rust/conformance/fixtures` is the
//! check on that claim.
//!
//! # Refusals
//!
//! A refusal is a sentence. The sentences here are core's: the
//! `config utils: ...` wrapper, the unusable-key and collapsing-key
//! sentences, and -- because core passes them on unchanged -- go-yaml's own
//! refusals (a repeated key, a scalar a tag cannot decode, a value JSON has
//! no form for). `rust/conformance/tools/errortext.py` compares every
//! refusal this module produces against the oracle's, and
//! `rust/conformance/error-text-exceptions.txt` names the few that cannot be
//! reproduced, each with its reason.
//!
//! Refusals are still `String`s: the *kind* of a refusal (invalid,
//! unavailable, ...) is a step of its own, and it is not this one.

use base64::Engine as _;
use flowcraft_contract::json;
use saphyr::{Mapping, Scalar, Yaml};
use saphyr_parser::{Event, Parser, ScalarStyle, Tag};
use serde_json::{Map, Number, Value};
use std::collections::HashMap;

/// The syntax of a configuration document, as `utils.Format` spells it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Format {
    /// Every document that does not start with `{`: `utils.FormatYAML`.
    Yaml,
    /// The first non-whitespace byte is `{`: `utils.FormatJSON`.
    Json,
}

impl Format {
    /// The name `utils.Format.String` reports.
    pub fn as_str(self) -> &'static str {
        match self {
            Format::Yaml => "yaml",
            Format::Json => "json",
        }
    }
}

impl std::fmt::Display for Format {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// `utils.FormatOf`: Kubernetes' sniffing rule -- JSON when the first
/// non-whitespace byte is `{`, YAML otherwise.
pub fn format_of(data: &[u8]) -> Format {
    let json = data
        .iter()
        .find(|b| !b.is_ascii_whitespace())
        .is_some_and(|b| *b == b'{');
    if json { Format::Json } else { Format::Yaml }
}

/// `config utils: `: what every refusal of `utils.ToJSON` starts with.
fn core(message: impl std::fmt::Display) -> String {
    format!("config utils: {message}")
}

/// `config utils: yaml: `: what core prefixes a go-yaml error with. The
/// sentence after it belongs to the library: where saphyr words the same
/// refusal differently, the difference is listed in
/// `rust/conformance/error-text-exceptions.txt` rather than papered over.
fn core_yaml(message: impl std::fmt::Display) -> String {
    format!("config utils: yaml: {message}")
}

/// `config utils: convert YAML to JSON: `: the prefix core puts on the
/// refusals it raises *after* go-yaml has had the document.
fn convert_refusal(message: impl std::fmt::Display) -> String {
    format!("config utils: convert YAML to JSON: {message}")
}

/// JSON input is validated and passed through as-is, which is what
/// `utils.ToJSON` does with a document `utils.FormatOf` calls JSON: the bytes
/// are not re-encoded, so key order and number spelling survive.
///
/// The refusal carries no detail, because core's does not: `json.Valid` says
/// only that the document is not JSON.
fn passthrough_json(data: &[u8]) -> Result<String, String> {
    let text = std::str::from_utf8(data).map_err(|_| core("invalid JSON document"))?;
    if serde_json::from_str::<Value>(text).is_err() {
        return Err(core("invalid JSON document"));
    }
    Ok(text.to_string())
}

/// `utils.ToJSON`: the strict conversion core applies to a configuration
/// document before anything reads it.
///
/// The rules below are go-yaml v2's as `sigs.k8s.io/yaml` uses them, quirks
/// included: scalar resolution, key coercion, merge keys, duplicate keys, the
/// key-collapse refusal, the alias guard, and the float64 funnel every YAML
/// number goes through.
///
/// The order of the refusals is core's too, and it is measurable: the reader
/// refuses a document whose *characters* are not YAML at all, the decoder
/// refuses what it cannot decode, then core refuses a document whose keys no
/// JSON key can spell, one whose keys repeat, one holding a value JSON cannot
/// write, and finally one whose distinct keys collapse onto a single JSON
/// key -- `core/utils/decode.go`'s `ToJSON` runs the checks in that order, and
/// `rust/conformance` measures each one against the oracle.
pub fn to_json(data: &[u8]) -> Result<String, String> {
    if format_of(data) == Format::Json {
        return passthrough_json(data);
    }
    let text = reader_check(data)?;
    // libyaml (hence go-yaml) strips a BOM; the pure-Rust scanner does not.
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    directive_guard(text)?;
    alias_guard(text)?;

    let mut loader: saphyr::YamlLoader<'_, Yaml<'_>> = saphyr::YamlLoader::default();
    loader.early_parse(false);
    // go-yaml keeps the last value of a key it sees twice and reports the
    // repetition itself, so the loader must not refuse the document first or
    // the report could not be made (fixture 12). A `<<` key may also appear
    // twice, and then neither appearance is a key at all (fixture 74).
    loader.allow_duplicate_keys(true);
    let mut parser = Parser::new_from_str(text);
    parser.load(&mut loader, true).map_err(core_yaml)?;
    if let Some(error) = loader.error() {
        return Err(core_yaml(error));
    }
    let docs = loader.into_documents();
    match docs.len() {
        0 => Ok(json::to_string(&Value::Null)),
        1 => convert_document(&docs[0], text),
        _ => Err(core("multiple YAML documents")),
    }
}

/// The conversion itself, in core's order of refusals.
///
/// `text` is the document's source: the key scan walks it as a stream of
/// parser events, because the loaded tree is a map and a map cannot show the
/// same key twice -- see `scan_keys`.
fn convert_document(document: &Yaml<'_>, text: &str) -> Result<String, String> {
    decode_check(document)?;
    let scan = scan_keys(text)?;
    if let Some(sentence) = scan.unusable_sentence() {
        return Err(convert_refusal(sentence));
    }
    if let Some(report) = scan.duplicate_report() {
        return Err(convert_refusal(report));
    }
    // The conversion runs before the collapse check because Go's does: the
    // library marshals the converted document on its way out, so a value JSON
    // cannot write is reported first, and a collapsing document that also
    // holds one reports the value (measured: `a: .nan` next to a
    // `1`/`"1"` pair is `json: unsupported value: NaN`).
    let mut unsupported = None;
    let value = go_value(document, Numbers::Float64, &mut unsupported)?;
    if let Some(spelling) = unsupported {
        return Err(convert_refusal(format!(
            "error converting YAML to JSON: json: unsupported value: {spelling}"
        )));
    }
    if let Some(sentences) = scan.collision_sentences() {
        return Err(convert_refusal(sentences.join("; ")));
    }
    Ok(json::to_string(&value))
}

/// What a lenient conversion produced.
#[derive(Debug, Clone)]
pub struct Lenient {
    /// The document as JSON text, with go-yaml's value rules: a repeated key
    /// keeps its last value, and nothing is reported about it.
    pub json: String,
    /// `YAML keys ... convert to the same JSON key ...`, one per collapsing
    /// group, in `utils.KeyCollisions` order. Reported rather than refused.
    pub collisions: Vec<String>,
}

/// Why a lenient conversion refused a document, split by who wrote the sentence.
#[derive(Debug, Clone)]
pub enum LenientRefusal {
    /// A key no JSON key can spell, in core's own words (`utils.UnusableKeys`):
    /// `YAML key !!null null cannot be spelled as a JSON key`.
    UnusableKeys(String),
    /// One of the refusals the YAML library makes for itself -- not YAML, a key
    /// no Go map can hold, more than one document, a value JSON cannot write --
    /// as a bare sentence. `to_json` puts `config utils: ` in front of these;
    /// a caller that is not `utils.ToJSON` says what it was parsing instead.
    Document(String),
}

/// The conversion a *typed* decode needs, which is not `utils.ToJSON`'s.
///
/// A caller that decodes a document into its own struct -- a skill's
/// frontmatter, for one -- goes through `sigs.k8s.io/yaml`, and that path differs
/// from `utils.ToJSON` in exactly two places:
///
///   - a repeated key keeps its last value and is reported to nobody, because
///     `yaml.v2` decodes into a Go map before anything looks at the repetition;
///   - distinct keys that spell one JSON key are converted, not refused: the
///     library builds its `map[string]interface{}` by assignment, so one value
///     wins. `utils.ToJSON` refuses such a document, which is what
///     [`to_json`] does; a frontmatter is somebody else's file, and refusing it
///     for a collapse inside an extension block would drop a usable skill.
///
/// What stays a refusal is everything the library itself refuses before any
/// target exists: a document that is not YAML, a key no Go map can hold, and --
/// through the key scan core runs first -- a key no JSON key can spell. The
/// sentences are bare, so the caller can say what it was parsing.
pub fn to_json_lenient(data: &[u8]) -> Result<Lenient, LenientRefusal> {
    if format_of(data) == Format::Json {
        let json =
            passthrough_json(data).map_err(|message| LenientRefusal::Document(bare(message)))?;
        return Ok(Lenient {
            json,
            collisions: Vec::new(),
        });
    }
    let text = reader_check(data).map_err(|message| LenientRefusal::Document(bare(message)))?;
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    directive_guard(text).map_err(|message| LenientRefusal::Document(bare(message)))?;
    alias_guard(text).map_err(|message| LenientRefusal::Document(bare(message)))?;

    let mut loader: saphyr::YamlLoader<'_, Yaml<'_>> = saphyr::YamlLoader::default();
    loader.early_parse(false);
    loader.allow_duplicate_keys(true);
    let mut parser = Parser::new_from_str(text);
    parser
        .load(&mut loader, true)
        .map_err(|error| LenientRefusal::Document(bare(core_yaml(error))))?;
    if let Some(error) = loader.error() {
        return Err(LenientRefusal::Document(bare(core_yaml(error))));
    }
    let docs = loader.into_documents();
    match docs.len() {
        0 => Ok(Lenient {
            json: json::to_string(&Value::Null),
            collisions: Vec::new(),
        }),
        1 => convert_lenient(&docs[0], text),
        _ => Err(LenientRefusal::Document("multiple YAML documents".into())),
    }
}

/// The lenient conversion itself: the same walk as `convert_document`, with the
/// two refusals a typed decode does not make removed.
fn convert_lenient(document: &Yaml<'_>, text: &str) -> Result<Lenient, LenientRefusal> {
    decode_check(document).map_err(|message| LenientRefusal::Document(bare(message)))?;
    let scan = scan_keys(text).map_err(|message| LenientRefusal::Document(bare(message)))?;
    if let Some(sentence) = scan.unusable_sentence() {
        return Err(LenientRefusal::UnusableKeys(sentence));
    }
    let mut unsupported = None;
    let value = go_value(document, Numbers::Exact, &mut unsupported)
        .map_err(|message| LenientRefusal::Document(bare(message)))?;
    if let Some(spelling) = unsupported {
        return Err(LenientRefusal::Document(format!(
            "json: unsupported value: {spelling}"
        )));
    }
    Ok(Lenient {
        json: json::to_string(&value),
        collisions: scan.collision_sentences().unwrap_or_default(),
    })
}

/// A sentence without core's `config utils: ` wrapper: the prefix belongs to
/// `utils.ToJSON`, and a caller with a different subject writes its own.
fn bare(message: String) -> String {
    match message.strip_prefix("config utils: ") {
        Some(rest) => rest.to_owned(),
        None => message,
    }
}

/// Go's `%q` for a string -- `strconv.Quote`'s spelling, which is what `%q`
/// prints in a formatted message.
///
/// Public for the same reason [`go_float32`] is: a port that quotes a value its
/// own way changes a sentence core wrote, and that sentence reaches a caller's
/// log. A skill's warnings quote a name and a directory that a third party
/// chose, so `\x07` and `\u0085` have to be spelled Go's way there too.
pub fn quote(text: &str) -> String {
    go_quote(text)
}

/// Go's `%g` at float32 precision -- `strconv.FormatFloat(v, 'g', -1, 32)`.
///
/// Not the same string as Go's `%v` for the same float: the digit count is what
/// float32 can round-trip, so `123456789012345678901234567890` is `1.2345679e+29`
/// here and `1.2345678901234568e+29` there, and a small float keeps the two-digit
/// exponent (`1e-06`). It is the spelling `sigs.k8s.io/yaml` gives a float that
/// lands in a string field, which is why it is public.
pub fn go_float32(value: f64) -> String {
    if value.is_nan() {
        return "NaN".into();
    }
    if value.is_infinite() {
        return if value > 0.0 { "+Inf" } else { "-Inf" }.into();
    }
    shortest_g(&f32_digits(value as f32))
}

/// go-yaml's reader, which runs before the scanner sees anything: every *raw*
/// character of a document must be a valid UTF-8 encoding and a printable
/// YAML character. It returns the document's text when it is both.
///
/// The character classes and the sentences are libyaml's, and they are
/// reproduced here because they are decidable from the document alone:
///
///   - a leading octet that encodes no sequence is `invalid leading UTF-8
///     octet`; a continuation that is not one is `invalid trailing UTF-8
///     octet`; an overlong or too-short sequence is `invalid length of a
///     UTF-8 sequence`; a surrogate or a value past U+10FFFF is `invalid
///     Unicode character`.
///   - a character outside `\t`, `\n`, `\r`, `#x20-#x7E`, `#x85`,
///     `#xA0-#xD7FF`, `#xE000-#xFFFD`, `#x10000-#x10FFFF` is `control
///     characters are not allowed` -- which is why a raw `\x01` anywhere in a
///     document (inside a quoted scalar or a comment included) refuses it.
///
/// libyaml reads a NUL-terminated buffer, so a sequence that runs off the end
/// is judged on the terminator and named as a bad trailing octet rather than
/// an incomplete sequence (measured: `\xe2\x82` at EOF is `invalid trailing
/// UTF-8 octet`).
fn reader_check(data: &[u8]) -> Result<&str, String> {
    let mut index = 0;
    while index < data.len() {
        let octet = data[index];
        // RFC 3629's leading octets, tested the way libyaml tests them.
        let (width, value) = match octet {
            _ if octet & 0x80 == 0x00 => (1, u32::from(octet)),
            _ if octet & 0xE0 == 0xC0 => (2, u32::from(octet & 0x1F)),
            _ if octet & 0xF0 == 0xE0 => (3, u32::from(octet & 0x0F)),
            _ if octet & 0xF8 == 0xF0 => (4, u32::from(octet & 0x07)),
            _ => return Err(core_yaml("invalid leading UTF-8 octet")),
        };
        let mut value = value;
        for offset in 1..width {
            let trailing = data.get(index + offset).copied().unwrap_or(0);
            if trailing & 0xC0 != 0x80 {
                return Err(core_yaml("invalid trailing UTF-8 octet"));
            }
            value = (value << 6) | u32::from(trailing & 0x3F);
        }
        let enough_digits = match width {
            1 => true,
            2 => value >= 0x80,
            3 => value >= 0x800,
            _ => value >= 0x10000,
        };
        if !enough_digits {
            return Err(core_yaml("invalid length of a UTF-8 sequence"));
        }
        if (0xD800..=0xDFFF).contains(&value) || value > 0x10FFFF {
            return Err(core_yaml("invalid Unicode character"));
        }
        let printable = matches!(value, 0x09 | 0x0A | 0x0D | 0x85)
            || (0x20..=0x7E).contains(&value)
            || (0xA0..=0xD7FF).contains(&value)
            || (0xE000..=0xFFFD).contains(&value)
            || (0x10000..=0x10FFFF).contains(&value);
        if !printable {
            return Err(core_yaml("control characters are not allowed"));
        }
        index += width;
    }
    std::str::from_utf8(data).map_err(|_| core_yaml("invalid UTF-8"))
}

/// The refusals yaml.v2 raises while *decoding*, before anything is converted:
/// a key no Go map can hold, a scalar a tag cannot decode, a merge value that
/// is not a mapping. They come first because the decoder walks the document in
/// order and stops at the first one, and core wraps them with
/// `config utils: yaml: ` -- measured on each of them in
/// `rust/conformance/fixtures`.
fn decode_check(node: &Yaml<'_>) -> Result<(), String> {
    match node {
        Yaml::Mapping(map) => {
            for (key, value) in map.iter() {
                decode_check_key(key)?;
                if is_merge_key(key) {
                    merge_sources(value).map_err(core_yaml)?;
                }
                decode_check(value)?;
            }
            Ok(())
        }
        Yaml::Sequence(seq) => {
            for item in seq {
                decode_check(item)?;
            }
            Ok(())
        }
        // A tag on a collection is a tag the JSON view drops (fixture 28).
        Yaml::Tagged(_, inner) => decode_check(inner),
        // A tagged scalar is decoded here, so a tag the scalar cannot satisfy
        // is a refusal of this walk -- and a *fail*, which go-yaml reports
        // ahead of any duplicate it has collected so far.
        Yaml::Representation(text, _, Some(tag)) => {
            tagged_value(text, tag, Numbers::Float64, &mut None).map(|_| ())
        }
        _ => Ok(()),
    }
}

/// The key half of `decode_check`: a scalar key is decoded with its tag, and a
/// key that is a collection -- a sequence or a mapping -- cannot be a Go map
/// key at all, which is the one refusal that prints the key itself.
fn decode_check_key(key: &Yaml<'_>) -> Result<(), String> {
    decode_check(key)?;
    match key {
        Yaml::Sequence(_) | Yaml::Mapping(_) => {
            Err(core_yaml(format!("invalid map key: {}", go_syntax(key))))
        }
        _ => Ok(()),
    }
}

/// yaml.v2 accepts YAML 1.1 directives only: `%YAML 1.2` is "found
/// incompatible YAML document" (fixture 40), while saphyr accepts 1.2.
fn directive_guard(text: &str) -> Result<(), String> {
    for line in text.lines() {
        if line.trim().is_empty() || line.trim_start().starts_with('#') {
            continue;
        }
        if !line.starts_with('%') {
            break;
        }
        let mut parts = line.split_whitespace();
        if parts.next() == Some("%YAML") {
            let version = parts.next().unwrap_or_default();
            let (major, minor) = version.split_once('.').unwrap_or((version, "0"));
            let ok = major.parse::<u32>() == Ok(1) && minor.parse::<u32>() == Ok(1);
            if !ok {
                return Err(core_yaml("found incompatible YAML document"));
            }
        }
    }
    Ok(())
}

/// go-yaml's `document contains excessive aliasing` guard, evaluated *before*
/// the loader expands anything: the scan walks the event stream, computes the
/// expanded size of every anchor by dynamic programming, and charges each
/// alias occurrence the size of the subtree it points at. That is what the
/// decoder's `aliasCount`/`decodeCount` ratio measures, and it is why a single
/// large alias is rejected while small documents with plenty of aliases are
/// not.
fn alias_guard(text: &str) -> Result<(), String> {
    /// Charges `size` nodes to the innermost open level. The root level is
    /// pushed before the walk and never popped, so there is always one to
    /// charge -- but the walk does not assume it: a stream that closes a level
    /// it never opened is a malformed document, not a reason to panic.
    fn charge(levels: &mut [(usize, usize)], size: usize) {
        if let Some(top) = levels.last_mut() {
            top.0 += size;
        }
    }

    let mut sizes: HashMap<usize, usize> = HashMap::new();
    // One accumulator per open collection: (expanded nodes inside, anchor id).
    let mut levels: Vec<(usize, usize)> = vec![(0, 0)];
    let mut aliased = 0usize;
    let mut parser = Parser::new_from_str(text);
    while let Some(event) = parser.next_event() {
        let event = event.map_err(core_yaml)?.0;
        match event {
            Event::Scalar(_, _, anchor, _) => {
                charge(&mut levels, 1);
                if anchor > 0 {
                    sizes.insert(anchor, 1);
                }
            }
            Event::SequenceStart(anchor, _) | Event::MappingStart(anchor, _) => {
                levels.push((0, anchor));
            }
            Event::SequenceEnd | Event::MappingEnd => {
                if let Some((inner, anchor)) = levels.pop() {
                    let size = inner + 1;
                    charge(&mut levels, size);
                    if anchor > 0 {
                        sizes.insert(anchor, size);
                    }
                }
            }
            Event::Alias(id) => {
                let size = sizes.get(&id).copied().unwrap_or(0);
                aliased += size;
                charge(&mut levels, size);
            }
            _ => {}
        }
    }

    let decoded: usize = levels.iter().map(|(nodes, _)| nodes).sum();
    if aliased > 100
        && decoded > 1000
        && (aliased as f64) / (decoded as f64) > allowed_alias_ratio(decoded)
    {
        return Err(core_yaml("document contains excessive aliasing"));
    }
    Ok(())
}

/// go-yaml's `allowedAliasRatio`.
fn allowed_alias_ratio(decode_count: usize) -> f64 {
    const LOW: usize = 400_000;
    const HIGH: usize = 4_000_000;
    const RANGE: f64 = (HIGH - LOW) as f64;
    if decode_count <= LOW {
        0.99
    } else if decode_count >= HIGH {
        0.10
    } else {
        0.99 - 0.89 * ((decode_count - LOW) as f64 / RANGE)
    }
}

/// One mapping key as the key scan resolved it.
#[derive(Clone, Debug)]
struct Entry {
    /// The key as go-yaml resolved it, which is what duplicate detection
    /// compares.
    resolved: Resolved,
    /// The key as the JSON object will spell it (`jsonKeySpelling`), computed
    /// once here because collisions group on it.
    json_key: String,
    /// The line the key is written on, which is the line a repetition of it
    /// is reported on -- for a key a `<<` spreads, the line it was written on
    /// where it was defined.
    line: usize,
}

/// A key a mapping holds twice, and the line go-yaml reports it on: the line
/// of the key being *inserted*, which is the later one for a repeat inside one
/// mapping and the merged-in one for a key a `<<` brings into a mapping that
/// already has it.
#[derive(Clone, Debug)]
struct Duplicate {
    /// Where the duplicate was encountered, in bytes. The report is a
    /// document-order list, and this is the only place that order can come
    /// from: an injected key is reported on the line it was *written* on.
    at: usize,
    line: usize,
    spelling: String,
}

/// One JSON key that two or more YAML keys of a single mapping are spelled as.
#[derive(Clone, Debug)]
struct Collision {
    json_key: String,
    sentence: String,
}

/// What core asks of a document's keys before it converts anything: which keys
/// no JSON key can spell, which keys a mapping holds twice, and which distinct
/// keys of one mapping are one JSON key.
///
/// The scan is a walk of the parser's event stream rather than of the loaded
/// tree, because the tree is a map: the loader keeps the last value of a
/// repeated key (as go-yaml does) and so cannot see the repetition itself --
/// and for a `float64` NaN key there is no repetition to see even in Go, since
/// a NaN never equals anything, not even itself. The events can see both, and
/// they carry the markers go-yaml reports.
#[derive(Default)]
struct KeyScan {
    /// `yamlKeySpelling`s of the keys no JSON key can spell, deduplicated and
    /// sorted when reported.
    unusable: Vec<String>,
    duplicates: Vec<Duplicate>,
    collisions: Vec<Collision>,
}

impl KeyScan {
    /// `UnusableKeys`: one sentence naming every key of the document that the
    /// conversion cannot spell as a JSON key at all, or nothing.
    ///
    /// The conversion spells a key when it is a string, an int, an int64, a
    /// float64 or a bool, and refuses the rest: a null key, and an integer
    /// past int64 (which go-yaml resolves to a uint64, and `jsonKeySpelling`
    /// does not spell a uint64). Naming the keys here rather than letting the
    /// conversion fail on one of them makes the report a function of the
    /// document: the library this replaced spelled keys while iterating a Go
    /// map, so a document holding two of them named a different one per
    /// process.
    fn unusable_sentence(&self) -> Option<String> {
        if self.unusable.is_empty() {
            return None;
        }
        let mut unique = self.unusable.clone();
        unique.sort();
        unique.dedup();
        Some(if unique.len() == 1 {
            format!("YAML key {} cannot be spelled as a JSON key", unique[0])
        } else {
            format!(
                "YAML keys {} cannot be spelled as JSON keys",
                unique.join(", ")
            )
        })
    }

    /// go-yaml's strict-decode report for a repeated key, verbatim: the
    /// library's own prefix and one line per extra occurrence, in document
    /// order. This is the sentence core's tests match on
    /// (`core/utils/decode_test.go`, "already set in map"), so it is part of
    /// the refusal's contract and not a detail of the message.
    fn duplicate_report(&self) -> Option<String> {
        if self.duplicates.is_empty() {
            return None;
        }
        let mut found = self.duplicates.clone();
        found.sort_by_key(|duplicate| duplicate.at);
        let mut out = String::from("error converting YAML to JSON: yaml: unmarshal errors:");
        for duplicate in found {
            out.push_str(&format!(
                "\n  line {}: key {} already set in map",
                duplicate.line, duplicate.spelling
            ));
        }
        Some(out)
    }

    /// `KeyCollisions`: one sentence per JSON key that two or more distinct
    /// YAML keys of one mapping are spelled as, in the order core reports
    /// them -- sorted by the JSON key, then by the sentence.
    ///
    /// A sentence names keys and not places, so two mappings that collide the
    /// same way report one sentence between them, and an aliased anchor (which
    /// is decoded once per alias) does not report its sentence once per alias.
    fn collision_sentences(&self) -> Option<Vec<String>> {
        if self.collisions.is_empty() {
            return None;
        }
        let mut found = self.collisions.clone();
        found.sort_by(|a, b| {
            (a.json_key.as_str(), a.sentence.as_str())
                .cmp(&(b.json_key.as_str(), b.sentence.as_str()))
        });
        let mut out: Vec<String> = Vec::with_capacity(found.len());
        for collision in found {
            if out.last() != Some(&collision.sentence) {
                out.push(collision.sentence);
            }
        }
        Some(out)
    }
}

/// One open collection of the key scan.
struct Frame {
    kind: FrameKind,
    /// How this collection sits in its parent, which is what the parent needs
    /// to know when it closes.
    role: Role,
}

enum Role {
    Root,
    /// The key of a mapping: its value follows once the collection closes.
    Key,
    /// The value of a mapping entry: the entry is complete once it closes.
    Value,
    /// An element of a sequence.
    Item,
}

enum FrameKind {
    Mapping {
        entries: Vec<Entry>,
        /// Whether the next event is a key (`true`) or the value of the key
        /// just seen.
        expecting_key: bool,
        /// The `<<` key's position, while its value is still open; the keys
        /// the value brings in are inserted here, and a conflict is reported
        /// at the position of the insert rather than of the key's definition.
        merge_at: Option<usize>,
        anchor: usize,
    },
    Sequence {
        /// The entry lists of the sequence's items, when the sequence is a
        /// merge value: a `<<` may name a sequence of mappings.
        items: Vec<Vec<Entry>>,
        merge: bool,
        anchor: usize,
    },
}

/// What an anchor's node holds, as far as a `<<` needs to know.
#[derive(Clone)]
enum Anchor {
    Entries(Vec<Entry>),
    Items(Vec<Vec<Entry>>),
}

/// The key scan: one walk of the document's events, tracking what each
/// mapping holds.
///
/// `<<` keys are not keys: go-yaml merges their value's mappings into the
/// mapping around them, so the scan inserts those entries into the mapping's
/// own list, in the order go-yaml inserts them -- a sequence of sources is
/// walked backwards, since an earlier source takes precedence over a later
/// one (fixture 53 measures the order, fixture 74 the fact that the `<<` key
/// itself is never inserted, and fixture 11 that the merged entries are then
/// ordinary entries for duplicate detection).
fn scan_keys(text: &str) -> Result<KeyScan, String> {
    let mut scan = KeyScan::default();
    let mut frames: Vec<Frame> = Vec::new();
    let mut anchors: HashMap<usize, Anchor> = HashMap::new();
    let mut parser = Parser::new_from_str(text);
    while let Some(event) = parser.next_event() {
        let (event, span) = event.map_err(core_yaml)?;
        let at = span.start.index();
        match event {
            Event::Scalar(text, style, anchor, tag) => {
                let resolved = resolve_scalar(&text, style, tag.as_deref())?;
                if anchor > 0 {
                    // A scalar anchor is only ever aliased as a value here; a
                    // merge naming a scalar is a decode refusal, checked
                    // before this walk.
                    anchors.insert(anchor, Anchor::Entries(Vec::new()));
                }
                let merge_key =
                    matches!(style, ScalarStyle::Plain) && tag.is_none() && text.as_ref() == "<<";
                if let Some(Frame {
                    kind:
                        FrameKind::Mapping {
                            entries,
                            expecting_key,
                            merge_at,
                            ..
                        },
                    ..
                }) = frames.last_mut()
                {
                    if *expecting_key {
                        *expecting_key = false;
                        if merge_key {
                            *merge_at = Some(at);
                        } else {
                            register(&mut scan, entries, resolved, at, span.start.line());
                        }
                    } else {
                        *expecting_key = true;
                        *merge_at = None;
                    }
                }
            }
            Event::SequenceStart(anchor, _) => {
                open(
                    &mut frames,
                    FrameKind::Sequence {
                        items: Vec::new(),
                        merge: false,
                        anchor,
                    },
                );
            }
            Event::MappingStart(anchor, _) => {
                open(
                    &mut frames,
                    FrameKind::Mapping {
                        entries: Vec::new(),
                        expecting_key: true,
                        merge_at: None,
                        anchor,
                    },
                );
            }
            Event::SequenceEnd => {
                close(&mut scan, &mut frames, &mut anchors, at);
            }
            Event::MappingEnd => {
                close(&mut scan, &mut frames, &mut anchors, at);
            }
            Event::Alias(id) => {
                let Some(anchor) = anchors.get(&id) else {
                    // An alias the loader will refuse; the walk has nothing to
                    // add to that refusal.
                    continue;
                };
                match frames.last_mut() {
                    Some(Frame {
                        kind:
                            FrameKind::Mapping {
                                entries,
                                expecting_key,
                                merge_at,
                                ..
                            },
                        ..
                    }) => {
                        if *expecting_key {
                            // An alias where a key belongs: saphyr's parser
                            // does not accept the form, so there is nothing to
                            // scan here.
                            *expecting_key = false;
                        } else {
                            *expecting_key = true;
                            if let Some(spread_at) = merge_at.take() {
                                spread(&mut scan, entries, anchor, spread_at);
                            }
                        }
                    }
                    Some(Frame {
                        kind: FrameKind::Sequence { items, merge, .. },
                        ..
                    }) if *merge => {
                        if let Anchor::Entries(entries) = anchor {
                            items.push(entries.clone());
                        }
                    }
                    _ => {}
                }
            }
            _ => {}
        }
    }
    Ok(scan)
}

/// Open a collection: the parent decides what the collection *is* to it, and a
/// `<<` whose value is a collection marks that collection as the merge value.
///
/// The parent's `merge_at` is left in place and read when the collection
/// closes: that is where the merged entries belong.
fn open(frames: &mut Vec<Frame>, mut kind: FrameKind) {
    let role = match frames.last_mut() {
        None => Role::Root,
        Some(parent) => match &mut parent.kind {
            FrameKind::Mapping {
                expecting_key,
                merge_at,
                ..
            } => {
                if *expecting_key {
                    *expecting_key = false;
                    Role::Key
                } else {
                    if merge_at.is_some()
                        && let FrameKind::Sequence { merge, .. } = &mut kind
                    {
                        *merge = true;
                    }
                    Role::Value
                }
            }
            FrameKind::Sequence { .. } => Role::Item,
        },
    };
    frames.push(Frame { kind, role });
}

/// Close a collection: record it under its anchor, then hand it to its parent.
///
/// The entries an anchor holds are the mapping's own entries -- the ones it
/// was written with and the ones a `<<` spread into it, which is exactly what
/// go-yaml's decoded map holds.
fn close(
    scan: &mut KeyScan,
    frames: &mut Vec<Frame>,
    anchors: &mut HashMap<usize, Anchor>,
    close_at: usize,
) {
    let Some(frame) = frames.pop() else {
        return;
    };
    let (anchor, content) = match &frame.kind {
        FrameKind::Mapping {
            entries, anchor, ..
        } => {
            report_collisions(scan, entries);
            (*anchor, Anchor::Entries(entries.clone()))
        }
        FrameKind::Sequence { items, anchor, .. } => (*anchor, Anchor::Items(items.clone())),
    };
    if anchor > 0 {
        anchors.insert(anchor, content.clone());
    }
    let Some(parent) = frames.last_mut() else {
        return;
    };
    match frame.role {
        Role::Value => {
            if let FrameKind::Mapping {
                entries,
                expecting_key,
                merge_at,
                ..
            } = &mut parent.kind
            {
                *expecting_key = true;
                if merge_at.take().is_some() {
                    spread(scan, entries, &content, close_at);
                }
            }
        }
        Role::Key => {
            if let FrameKind::Mapping { expecting_key, .. } = &mut parent.kind {
                // A collection as a key: its value follows.
                *expecting_key = false;
            }
        }
        Role::Item => {
            if let FrameKind::Sequence { items, merge, .. } = &mut parent.kind
                && *merge
            {
                match content {
                    Anchor::Entries(entries) => items.push(entries),
                    Anchor::Items(nested) => items.extend(nested),
                }
            }
        }
        Role::Root => {}
    }
}

/// Insert a `<<`'s entries into the mapping that named it, checking each one
/// against the entries already there -- a merged-in key is an ordinary key for
/// duplicate detection. A sequence of sources is inserted backwards: go-yaml
/// walks it that way, so an earlier source is the one that conflicts.
fn spread(scan: &mut KeyScan, into: &mut Vec<Entry>, from: &Anchor, at: usize) {
    match from {
        Anchor::Entries(entries) => {
            for entry in entries {
                insert(scan, into, entry.clone(), at);
            }
        }
        Anchor::Items(items) => {
            for item in items.iter().rev() {
                for entry in item {
                    insert(scan, into, entry.clone(), at);
                }
            }
        }
    }
}

/// Register one key of a mapping: an unspellable key is reported by name, an
/// already-present key as a repetition, and the rest join the mapping.
fn register(
    scan: &mut KeyScan,
    entries: &mut Vec<Entry>,
    resolved: Resolved,
    at: usize,
    line: usize,
) {
    match json_key_of(&resolved) {
        Some(json_key) => {
            let entry = Entry {
                resolved,
                json_key,
                line,
            };
            insert(scan, entries, entry, at);
        }
        None => scan.unusable.push(yaml_key_spelling(&resolved)),
    }
}

/// Insert one entry, reporting a repetition when the mapping already holds the
/// same key.
///
/// The comparison is Go's: a `-0.0` and a `0.0` are one key, and a NaN is equal
/// to nothing at all -- so a mapping may hold `.nan` twice, and core then
/// refuses it as a collapse rather than as a repetition.
fn insert(scan: &mut KeyScan, entries: &mut Vec<Entry>, entry: Entry, at: usize) {
    let known = entries
        .iter()
        .find(|existing| go_key_eq(&existing.resolved, &entry.resolved));
    if known.is_some() {
        scan.duplicates.push(Duplicate {
            at,
            line: entry.line,
            spelling: go_syntax_of(&entry.resolved),
        });
    }
    entries.push(entry);
}

/// The collisions of one mapping: group its entries by the JSON key they are
/// spelled as, and report every group with more than one *distinct* key.
///
/// Distinct is Go's key equality and not a spelling comparison, because the
/// findings are read off a Go map: a key written twice collapsed into one entry
/// before `KeyCollisions` ever sees it, so `description: a` above `description:
/// b` collides with nothing -- while two `.nan` keys, which are unequal to
/// themselves, are two entries that collide with each other and are reported
/// twice. Counting spellings instead would get both of those backwards.
fn report_collisions(scan: &mut KeyScan, entries: &[Entry]) {
    let mut groups: Vec<(&str, Vec<&Entry>)> = Vec::new();
    for entry in entries {
        match groups
            .iter_mut()
            .find(|(json_key, _)| *json_key == entry.json_key)
        {
            Some((_, group)) => group.push(entry),
            None => groups.push((entry.json_key.as_str(), vec![entry])),
        }
    }
    for (json_key, group) in groups {
        let mut distinct: Vec<&Entry> = Vec::with_capacity(group.len());
        for entry in group {
            if !distinct
                .iter()
                .any(|known| go_key_eq(&known.resolved, &entry.resolved))
            {
                distinct.push(entry);
            }
        }
        if distinct.len() < 2 {
            continue;
        }
        let mut spellings: Vec<String> = distinct
            .iter()
            .map(|entry| yaml_key_spelling(&entry.resolved))
            .collect();
        spellings.sort();
        scan.collisions.push(Collision {
            json_key: json_key.to_string(),
            sentence: format!(
                "YAML keys {} convert to the same JSON key {}",
                spellings.join(", "),
                go_quote(json_key)
            ),
        });
    }
}

/// Go's key equality for `interface{}` map keys: the dynamic type and the
/// value, with `float64` compared by `==`, i.e. `-0.0 == 0.0` and
/// `NaN != NaN`.
fn go_key_eq(a: &Resolved, b: &Resolved) -> bool {
    match (a, b) {
        (Resolved::Null, Resolved::Null) => true,
        (Resolved::Bool(x), Resolved::Bool(y)) => x == y,
        (Resolved::Int(x), Resolved::Int(y)) => x == y,
        (Resolved::UInt(x), Resolved::UInt(y)) => x == y,
        (Resolved::F64(x), Resolved::F64(y)) => x == y,
        (Resolved::Str(x), Resolved::Str(y)) => x == y,
        _ => false,
    }
}

/// The types go-yaml v2 resolves a scalar to.
#[derive(Debug, Clone)]
enum Resolved {
    Null,
    Bool(bool),
    Int(i64),
    UInt(u64),
    F64(f64),
    Str(String),
}

/// How a resolved number reaches the JSON document.
///
/// The two policies are two callers' pipelines rather than a preference:
/// `utils.ToJSON` converts for an `interface{}` target and so passes every
/// number through a float64, while a typed decode converts for a target that
/// knows what each field is, and `encoding/json` marshals an `int64` or a
/// `uint64` as its exact decimal spelling.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Numbers {
    /// Through `interface{}`: a big integer loses its last digits to float64,
    /// and a value JSON cannot write refuses the document. This is the strict
    /// conversion.
    Float64,
    /// As itself: an integer keeps every digit, and a float JSON cannot write
    /// becomes the spelling Go gives it -- `+Inf`, `-Inf`, `NaN` -- instead of
    /// refusing, because the target has already said the field is a string
    /// (`sigs.k8s.io/yaml` turns `.inf` into `"+Inf"` before it writes
    /// anything). This is the typed decode's conversion.
    Exact,
}

fn go_value(
    node: &Yaml<'_>,
    numbers: Numbers,
    unsupported: &mut Option<String>,
) -> Result<Value, String> {
    match node {
        Yaml::Representation(text, style, tag) => {
            scalar_go(text, *style, tag.as_deref(), numbers, unsupported)
        }
        Yaml::Value(scalar) => resolved_to_json(
            match scalar {
                Scalar::Null => Resolved::Null,
                Scalar::Boolean(b) => Resolved::Bool(*b),
                Scalar::Integer(i) => Resolved::Int(*i),
                Scalar::FloatingPoint(f) => Resolved::F64(f.0),
                Scalar::String(s) => Resolved::Str(s.to_string()),
            },
            numbers,
            unsupported,
        ),
        Yaml::Sequence(seq) => {
            let mut out = Vec::with_capacity(seq.len());
            for item in seq {
                out.push(go_value(item, numbers, unsupported)?);
            }
            Ok(Value::Array(out))
        }
        Yaml::Mapping(map) => go_mapping(map, numbers, unsupported),
        // A tag on a collection stays a tag on the node; the JSON view only
        // cares about the content.
        Yaml::Tagged(_, inner) => go_value(inner, numbers, unsupported),
        Yaml::Alias(_) => Err("unresolved alias".into()),
        Yaml::BadValue => Err("bad value".into()),
    }
}

fn scalar_go(
    text: &str,
    style: ScalarStyle,
    tag: Option<&Tag>,
    numbers: Numbers,
    unsupported: &mut Option<String>,
) -> Result<Value, String> {
    if let Some(tag) = tag {
        return tagged_value(text, tag, numbers, unsupported);
    }
    // Quoted and block scalars are strings; go-yaml tags them `!!str`.
    if style != ScalarStyle::Plain {
        return Ok(Value::String(text.to_string()));
    }
    resolved_to_json(resolve_plain(text)?, numbers, unsupported)
}

/// The entries a JSON object gets, in the order `encoding/json` walks them.
///
/// Two orders meet here. The entries are collected in document order, because
/// that is where a `<<` spreads its sources and where the last value of a
/// repeated key wins -- and it is the order the *first* unsupported value
/// would be found in. They are then handed to the map sorted by JSON key,
/// because Go marshals a map in sorted key order, and the value the refusal
/// names is therefore the first one *there*: measured with `b: .inf` above
/// `z: .nan`, which reports `+Inf` where document order alone would report
/// `NaN` (fixtures 62 and 85).
fn go_mapping(
    map: &Mapping<'_>,
    numbers: Numbers,
    unsupported: &mut Option<String>,
) -> Result<Value, String> {
    let mut entries: Vec<(String, &Yaml<'_>)> = Vec::new();
    collect_entries(map, &mut entries)?;
    entries.sort_by(|a, b| a.0.cmp(&b.0));
    let mut out = Map::new();
    for (json_key, value) in entries {
        out.insert(json_key, go_value(value, numbers, unsupported)?);
    }
    Ok(Value::Object(out))
}

/// One mapping's entries with its merge keys expanded, in document order.
fn collect_entries<'a>(
    map: &'a Mapping<'a>,
    out: &mut Vec<(String, &'a Yaml<'a>)>,
) -> Result<(), String> {
    for (key, value) in map.iter() {
        if is_merge_key(key) {
            for source in merge_sources(value)? {
                collect_entries(source, out)?;
            }
            continue;
        }
        let resolved = resolve_key(key)?;
        // Unspellable keys never reach the conversion: the key scan refuses
        // the document first.
        if let Some(json_key) = json_key_of(&resolved) {
            out.push((json_key, value));
        }
    }
    Ok(())
}

/// A value a JSON document has no form for -- an infinity or a NaN -- stops
/// the conversion, and the refusal names the value the way Go's marshaler
/// does. The conversion itself carries on and writes a null in its place: the
/// value is never serialized, and the refusal is raised above.
fn resolved_to_json(
    resolved: Resolved,
    numbers: Numbers,
    unsupported: &mut Option<String>,
) -> Result<Value, String> {
    Ok(match resolved {
        Resolved::Null => Value::Null,
        Resolved::Bool(b) => Value::Bool(b),
        Resolved::Int(i) => match numbers {
            Numbers::Float64 => number(i as f64, unsupported),
            Numbers::Exact => Value::Number(Number::from(i)),
        },
        Resolved::UInt(u) => match numbers {
            Numbers::Float64 => number(u as f64, unsupported),
            Numbers::Exact => Value::Number(Number::from(u)),
        },
        Resolved::F64(f) => match numbers {
            // A value JSON cannot write, and a target that has already said
            // the field is a string: the spelling, not a refusal.
            Numbers::Exact if !f.is_finite() => Value::String(go_float_g(f)),
            _ => number(f, unsupported),
        },
        Resolved::Str(s) => Value::String(s),
    })
}

/// `utils.ToJSON` funnels every number through `interface{}`, i.e. through
/// float64, so a YAML document's big integers come out rounded. The JSON
/// *input* path does not: `utils.ToJSON` passes valid JSON through untouched.
/// A typed decode does not either; see [`Numbers`].
fn number(f: f64, unsupported: &mut Option<String>) -> Value {
    match Number::from_f64(f) {
        Some(number) => Value::Number(number),
        None => {
            if unsupported.is_none() {
                *unsupported = Some(go_float_g(f));
            }
            Value::Null
        }
    }
}

/// go-yaml v2's `resolve()` for plain scalars: the exact-match table first,
/// then the first-byte hint switch.
fn resolve_plain(s: &str) -> Result<Resolved, String> {
    match s {
        "" | "~" | "null" | "Null" | "NULL" => return Ok(Resolved::Null),
        "y" | "Y" | "yes" | "Yes" | "YES" | "true" | "True" | "TRUE" | "on" | "On" | "ON" => {
            return Ok(Resolved::Bool(true));
        }
        "n" | "N" | "no" | "No" | "NO" | "false" | "False" | "FALSE" | "off" | "Off" | "OFF" => {
            return Ok(Resolved::Bool(false));
        }
        ".nan" | ".NaN" | ".NAN" => return Ok(Resolved::F64(f64::NAN)),
        ".inf" | ".Inf" | ".INF" | "+.inf" | "+.Inf" | "+.INF" => {
            return Ok(Resolved::F64(f64::INFINITY));
        }
        "-.inf" | "-.Inf" | "-.INF" => return Ok(Resolved::F64(f64::NEG_INFINITY)),
        // `<<` is the merge key; the mapping layer handles it.
        "<<" => return Ok(Resolved::Str(s.to_string())),
        _ => {}
    }

    match s.as_bytes()[0] {
        // "Not in the map, so maybe a normal float."
        b'.' => Ok(match parse_float_go(s) {
            Some(f) => Resolved::F64(f),
            None => Resolved::Str(s.to_string()),
        }),
        // "Int, float, or timestamp." Timestamps are deliberately *not*
        // resolved: go-yaml keeps timestamp-like values as strings for
        // `interface{}` targets ("for backward compatibility reasons", see
        // decoder.scalar), and the JSON pipeline only ever has interface{}
        // targets. See fixture 08.
        b'0'..=b'9' | b'+' | b'-' => {
            let plain = s.replace('_', "");
            if let Some(i) = parse_int_go(&plain) {
                return Ok(Resolved::Int(i));
            }
            if let Some(u) = parse_uint_go(&plain) {
                return Ok(Resolved::UInt(u));
            }
            if float_re_go(&plain)
                && let Some(f) = parse_float_go(&plain)
            {
                return Ok(Resolved::F64(f));
            }
            if let Some(rest) = plain.strip_prefix("0b") {
                if let Ok(i) = i64::from_str_radix(rest, 2) {
                    return Ok(Resolved::Int(i));
                }
                if let Ok(u) = u64::from_str_radix(rest, 2) {
                    return Ok(Resolved::UInt(u));
                }
            } else if let Some(rest) = plain.strip_prefix("-0b")
                && let Ok(i) = i64::from_str_radix(rest, 2)
            {
                return Ok(Resolved::Int(-i));
            }
            Ok(Resolved::Str(s.to_string()))
        }
        _ => Ok(Resolved::Str(s.to_string())),
    }
}

/// `strconv.ParseInt(plain, 0, 64)`: optional sign, then `0x`/`0o`/`0b` or a
/// leading `0` for octal, else decimal.
fn parse_int_go(plain: &str) -> Option<i64> {
    let (negative, body) = match plain.as_bytes().first()? {
        b'-' => (true, &plain[1..]),
        b'+' => (false, &plain[1..]),
        _ => (false, plain),
    };
    let signed = |v: i64| Some(if negative { -v } else { v });
    if let Some(rest) = body.strip_prefix("0x").or_else(|| body.strip_prefix("0X")) {
        return signed(i64::from_str_radix(rest, 16).ok()?);
    }
    if let Some(rest) = body.strip_prefix("0o").or_else(|| body.strip_prefix("0O")) {
        return signed(i64::from_str_radix(rest, 8).ok()?);
    }
    if let Some(rest) = body.strip_prefix("0b").or_else(|| body.strip_prefix("0B")) {
        return signed(i64::from_str_radix(rest, 2).ok()?);
    }
    // A leading zero selects octal, and "08" then fails -- which is exactly
    // what makes `08` fall through to the float branch in go-yaml.
    if body.len() > 1 && body.starts_with('0') {
        return signed(i64::from_str_radix(&body[1..], 8).ok()?);
    }
    signed(body.parse::<i64>().ok()?)
}

/// `strconv.ParseUint(plain, 0, 64)` (no sign allowed).
fn parse_uint_go(plain: &str) -> Option<u64> {
    if let Some(rest) = plain
        .strip_prefix("0x")
        .or_else(|| plain.strip_prefix("0X"))
    {
        return u64::from_str_radix(rest, 16).ok();
    }
    if let Some(rest) = plain
        .strip_prefix("0o")
        .or_else(|| plain.strip_prefix("0O"))
    {
        return u64::from_str_radix(rest, 8).ok();
    }
    if let Some(rest) = plain
        .strip_prefix("0b")
        .or_else(|| plain.strip_prefix("0B"))
    {
        return u64::from_str_radix(rest, 2).ok();
    }
    if plain.len() > 1 && plain.starts_with('0') {
        return u64::from_str_radix(&plain[1..], 8).ok();
    }
    plain.parse::<u64>().ok()
}

/// `var yamlStyleFloat = regexp.MustCompile("^[-+]?(\\.[0-9]+|[0-9]+(\\.[0-9]*)?)([eE][-+]?[0-9]+)?$")`
fn float_re_go(s: &str) -> bool {
    let s = match s.as_bytes().first() {
        Some(b'+') | Some(b'-') => &s[1..],
        _ => s,
    };
    let (mantissa, exponent) = match s.find(['e', 'E']) {
        Some(i) => (&s[..i], Some(&s[i + 1..])),
        None => (s, None),
    };
    if let Some(exp) = exponent {
        let exp = match exp.as_bytes().first() {
            Some(b'+') | Some(b'-') => &exp[1..],
            _ => exp,
        };
        if exp.is_empty() || !exp.bytes().all(|b| b.is_ascii_digit()) {
            return false;
        }
    }
    if let Some(frac) = mantissa.strip_prefix('.') {
        return !frac.is_empty() && frac.bytes().all(|b| b.is_ascii_digit());
    }
    let (int, frac) = match mantissa.find('.') {
        Some(i) => (&mantissa[..i], &mantissa[i + 1..]),
        None => (mantissa, ""),
    };
    if int.is_empty() || !int.bytes().all(|b| b.is_ascii_digit()) {
        return false;
    }
    frac.bytes().all(|b| b.is_ascii_digit())
}

/// `strconv.ParseFloat`, restricted to what go-yaml feeds it.
fn parse_float_go(s: &str) -> Option<f64> {
    if s.is_empty() {
        return None;
    }
    s.parse::<f64>().ok()
}

/// Explicit tags: `resolvableTag` decides whether the value is re-resolved or
/// kept as the raw string.
fn tagged_value(
    text: &str,
    tag: &Tag,
    numbers: Numbers,
    unsupported: &mut Option<String>,
) -> Result<Value, String> {
    if !tag.is_yaml_core_schema() {
        return Ok(Value::String(text.to_string()));
    }
    match tag.suffix.as_str() {
        "str" => Ok(Value::String(text.to_string())),
        "binary" => {
            let cleaned: String = text.chars().filter(|c| !c.is_whitespace()).collect();
            let bytes = base64::engine::general_purpose::STANDARD
                .decode(cleaned)
                .map_err(|_| core_yaml("!!binary value contains invalid base64 data"))?;
            Ok(Value::String(String::from_utf8_lossy(&bytes).into_owned()))
        }
        "null" => match resolve_plain(text)? {
            Resolved::Null => Ok(Value::Null),
            other => Err(tag_refusal(text, &other, tag)),
        },
        "bool" => match resolve_plain(text)? {
            Resolved::Bool(b) => Ok(Value::Bool(b)),
            other => Err(tag_refusal(text, &other, tag)),
        },
        "int" => match resolve_plain(text)? {
            Resolved::Int(i) => resolved_to_json(Resolved::Int(i), numbers, unsupported),
            Resolved::UInt(u) => resolved_to_json(Resolved::UInt(u), numbers, unsupported),
            other => Err(tag_refusal(text, &other, tag)),
        },
        "float" => match resolve_plain(text)? {
            Resolved::F64(f) => resolved_to_json(Resolved::F64(f), numbers, unsupported),
            Resolved::Int(i) => resolved_to_json(Resolved::F64(i as f64), numbers, unsupported),
            other => Err(tag_refusal(text, &other, tag)),
        },
        // An explicit `!!timestamp` is a string too, for the same
        // backward-compatibility reason as the plain case.
        "timestamp" => Ok(Value::String(text.to_string())),
        _ => Ok(Value::String(text.to_string())),
    }
}

/// yaml.v2's refusal for a scalar a tag cannot decode: the tag the scalar
/// *did* resolve to, the scalar as written, and the tag it was asked for
/// (measured on `!!int abc`, `!!null 5`, `!!float abc` and `!!null .inf`).
fn tag_refusal(text: &str, resolved: &Resolved, tag: &Tag) -> String {
    let actual = match resolved {
        Resolved::Null => "!!null",
        Resolved::Bool(_) => "!!bool",
        Resolved::Int(_) | Resolved::UInt(_) => "!!int",
        Resolved::F64(_) => "!!float",
        Resolved::Str(_) => "!!str",
    };
    core_yaml(format!(
        "cannot decode {actual} `{text}` as a !!{}",
        tag.suffix
    ))
}

/// The key of a mapping as go-yaml resolved it, from a scalar event or a
/// loaded node's representation.
fn resolve_scalar(text: &str, style: ScalarStyle, tag: Option<&Tag>) -> Result<Resolved, String> {
    Ok(match tag {
        // Unknown tags keep the raw text; core-schema tags re-resolve.
        Some(tag) if tag.is_yaml_core_schema() && tag.suffix != "str" => {
            match tag.suffix.as_str() {
                "binary" => {
                    let cleaned: String = text.chars().filter(|c| !c.is_whitespace()).collect();
                    let bytes = base64::engine::general_purpose::STANDARD
                        .decode(cleaned)
                        .map_err(|_| core_yaml("!!binary value contains invalid base64 data"))?;
                    Resolved::Str(String::from_utf8_lossy(&bytes).into_owned())
                }
                _ => resolve_plain(text)?,
            }
        }
        Some(_) => Resolved::Str(text.to_string()),
        None if style == ScalarStyle::Plain => resolve_plain(text)?,
        None => Resolved::Str(text.to_string()),
    })
}

/// The key as go-yaml resolved it; `convertToJSONableObject` then decides how
/// (and whether) it can become a JSON object key.
fn resolve_key(key: &Yaml<'_>) -> Result<Resolved, String> {
    Ok(match key {
        Yaml::Representation(text, style, tag) => resolve_scalar(text, *style, tag.as_deref())?,
        Yaml::Value(scalar) => match scalar {
            Scalar::Null => Resolved::Null,
            Scalar::Boolean(b) => Resolved::Bool(*b),
            Scalar::Integer(i) => Resolved::Int(*i),
            Scalar::FloatingPoint(f) => Resolved::F64(f.0),
            Scalar::String(s) => Resolved::Str(s.to_string()),
        },
        // Complex keys are not representable in JSON; the decode check refuses
        // them before the conversion runs.
        _ => return Err(core_yaml("invalid map key")),
    })
}

/// `convertToJSONableObject`'s key spelling for the key types it accepts, or
/// `None` for the two it refuses.
///
/// Floats go through `strconv.FormatFloat(f, 'g', -1, 32)` -- 32-bit on
/// purpose, so `1.0000000001` and `1` are the same JSON key, a float above
/// the float32 range is `.inf` and one below it is `0`. The two refusals are
/// a null key and an integer past int64: go-yaml resolves such an integer to
/// a uint64, and the spelling has no case for a uint64.
fn json_key_of(resolved: &Resolved) -> Option<String> {
    Some(match resolved {
        Resolved::Str(s) => s.clone(),
        Resolved::Int(i) => i.to_string(),
        Resolved::Bool(b) => b.to_string(),
        Resolved::F64(f) => float32_key(*f),
        Resolved::Null | Resolved::UInt(_) => return None,
    })
}

/// A float key as a JSON key: `strconv.FormatFloat(f, 'g', -1, 32)`'s form of
/// the value cast to a float32, with the infinities and NaN spelled the way
/// go-yaml spells them.
fn float32_key(f: f64) -> String {
    if f.is_nan() {
        return ".nan".into();
    }
    let rounded = f as f32;
    if rounded.is_infinite() {
        return if rounded > 0.0 { ".inf" } else { "-.inf" }.into();
    }
    shortest_g(&f32_digits(rounded))
}

/// The digits and exponent of `strconv.FormatFloat`'s shortest form, as
/// `(digits, decimal exponent)`.
struct Shortest {
    negative: bool,
    digits: String,
    exponent: i32,
}

/// Rust's `{:e}` is the shortest round-trip form too, so the digits are its
/// mantissa and the exponent is its exponent: only the notation differs, and
/// `shortest_g` writes that.
fn f64_digits(value: f64) -> Shortest {
    digits_of(&format!("{value:e}"))
}

fn f32_digits(value: f32) -> Shortest {
    digits_of(&format!("{value:e}"))
}

fn digits_of(text: &str) -> Shortest {
    let (mantissa, exponent) = text.split_once('e').unwrap_or((text, "0"));
    let (negative, mantissa) = match mantissa.strip_prefix('-') {
        Some(rest) => (true, rest),
        None => (false, mantissa),
    };
    Shortest {
        negative,
        digits: mantissa.chars().filter(|c| *c != '.').collect(),
        exponent: exponent.parse().unwrap_or(0),
    }
}

/// Go's shortest `'g'` notation: `fmt`'s `%v` for a float, and
/// `strconv.FormatFloat(f, 'g', -1, 64)` for the same value.
///
/// Exponent notation is used when the decimal exponent is below -4 or at least
/// 6 -- that second bound is what makes a float key print as `1e+06` where
/// `999999` stays itself, and it is the reason Rust's `{}` cannot stand in for
/// this: Rust never uses exponent notation, so it writes `1000000` where Go
/// writes `1e+06`.
fn shortest_g(shortest: &Shortest) -> String {
    let Shortest {
        negative,
        digits,
        exponent,
    } = shortest;
    let point = exponent + 1;
    let mut out = String::new();
    if *negative {
        out.push('-');
    }
    if *exponent < -4 || *exponent >= 6 {
        out.push_str(digits.get(..1).unwrap_or(""));
        if digits.len() > 1 {
            out.push('.');
            out.push_str(digits.get(1..).unwrap_or(""));
        }
        let sign = if *exponent < 0 { '-' } else { '+' };
        out.push_str(&format!("e{sign}{:02}", exponent.abs()));
    } else if point <= 0 {
        out.push_str("0.");
        for _ in 0..-point {
            out.push('0');
        }
        out.push_str(digits);
    } else if point as usize >= digits.len() {
        out.push_str(digits);
        for _ in digits.len()..point as usize {
            out.push('0');
        }
    } else {
        out.push_str(digits.get(..point as usize).unwrap_or(""));
        out.push('.');
        out.push_str(digits.get(point as usize..).unwrap_or(""));
    }
    out
}

/// `fmt`'s `%v` for a float, which is what the unsupported-value refusal (and
/// the `%#v` of a float key) prints.
fn go_float_g(value: f64) -> String {
    if value.is_nan() {
        return "NaN".into();
    }
    if value.is_infinite() {
        return if value > 0.0 { "+Inf" } else { "-Inf" }.into();
    }
    shortest_g(&f64_digits(value))
}

/// How YAML spells a resolved key: the tag it resolved to, then its value.
/// The tag is what keeps two keys of a collapse apart -- `!!int 1` and
/// `!!float 1` share the JSON key "1" but not the spelling.
fn yaml_key_spelling(resolved: &Resolved) -> String {
    match resolved {
        // `!!null null`, not `!!null`: core's spelling carries the value, and
        // a message the two sides disagree on is a message a reader cannot
        // compare (`core/utils/decode.go`, yamlKeySpelling).
        Resolved::Null => "!!null null".into(),
        Resolved::Bool(b) => format!("!!bool {b}"),
        Resolved::Int(i) => format!("!!int {i}"),
        Resolved::UInt(u) => format!("!!int {u}"),
        Resolved::F64(f) => format!("!!float {}", yaml_float_spelling(*f)),
        Resolved::Str(s) => format!("!!str {}", go_quote(s)),
    }
}

/// YAML's own spellings for the special floats, everything else in Go's
/// shortest form (i.e. full precision, the opposite of the float32
/// truncation the JSON key gets). The point of the message is that the value
/// the document wrote is not the value its JSON key says.
fn yaml_float_spelling(value: f64) -> String {
    if value.is_nan() {
        ".nan".into()
    } else if value == f64::INFINITY {
        ".inf".into()
    } else if value == f64::NEG_INFINITY {
        "-.inf".into()
    } else {
        go_float_g(value)
    }
}

/// `%#v` of a resolved scalar: Go syntax, which is what go-yaml prints a
/// repeated key with. A `uint64` is hexadecimal there, and a float prints the
/// way `%v` prints it -- `-0` for negative zero, `NaN` for a NaN.
fn go_syntax_of(resolved: &Resolved) -> String {
    match resolved {
        // `%#v` of a nil is `<nil>`; a nil *map key* cannot reach this (the
        // conversion refuses it, and core names it in the unusable sentence)
        // but a nil inside a complex key can, and there it prints as the
        // interface it is (see `go_syntax`).
        Resolved::Null => "<nil>".into(),
        Resolved::Bool(b) => b.to_string(),
        Resolved::Int(i) => i.to_string(),
        Resolved::UInt(u) => format!("0x{u:x}"),
        Resolved::F64(f) => go_float_g(*f),
        Resolved::Str(s) => go_quote(s),
    }
}

/// `%#v` of a value, for the one message that prints one: an invalid map key.
///
/// The spellings are Go's, down to the type names (`[]interface {}`,
/// `map[interface {}]interface {}`) and the `interface {}(nil)` a nil prints
/// as once it sits inside one of them. A mapping's entries are printed in
/// sorted order when their keys are all strings, because that is what `fmt`
/// does; keys of *different* concrete types are ordered by the addresses of
/// their type descriptors in Go, which no other program can reproduce -- see
/// `rust/conformance/error-text-exceptions.txt`.
fn go_syntax(node: &Yaml<'_>) -> String {
    match node {
        Yaml::Sequence(seq) => {
            let items: Vec<String> = seq.iter().map(go_syntax_element).collect();
            format!("[]interface {{{}}}{{{}}}", "", items.join(", "))
        }
        Yaml::Mapping(map) => {
            let mut entries: Vec<(String, String)> = Vec::new();
            let mut all_strings = true;
            for (key, value) in map.iter() {
                let rendered_key = go_syntax_element(key);
                all_strings = all_strings && matches!(resolve_key(key), Ok(Resolved::Str(_)));
                entries.push((rendered_key, go_syntax_element(value)));
            }
            if all_strings {
                entries.sort();
            }
            let joined: Vec<String> = entries
                .into_iter()
                .map(|(key, value)| format!("{key}:{value}"))
                .collect();
            format!(
                "map[interface {{}}]interface {{{}}}{{{}}}",
                "",
                joined.join(", ")
            )
        }
        Yaml::Tagged(_, inner) => go_syntax(inner),
        Yaml::Representation(text, style, tag) => {
            match resolve_scalar(text, *style, tag.as_deref()) {
                Ok(resolved) => go_syntax_of(&resolved),
                Err(_) => go_quote(text),
            }
        }
        Yaml::Value(scalar) => go_syntax_of(&match scalar {
            Scalar::Null => Resolved::Null,
            Scalar::Boolean(b) => Resolved::Bool(*b),
            Scalar::Integer(i) => Resolved::Int(*i),
            Scalar::FloatingPoint(f) => Resolved::F64(f.0),
            Scalar::String(s) => Resolved::Str(s.to_string()),
        }),
        Yaml::Alias(_) | Yaml::BadValue => go_quote(""),
    }
}

/// `%#v` of a value in an `interface{}` slot -- every value inside a
/// collection, that is. A nil prints as the interface it sits in
/// (`interface {}(nil)`, measured on a nil key, a nil value and a nil
/// sequence element alike), and anything else is `go_syntax`.
fn go_syntax_element(node: &Yaml<'_>) -> String {
    match resolve_key(node) {
        Ok(Resolved::Null) => "interface {}(nil)".to_string(),
        Ok(resolved) => go_syntax_of(&resolved),
        // A collection: the same rendering, one level down.
        Err(_) => go_syntax(node),
    }
}

/// `strconv.Quote` / `%q`: Go's own quoting, which is what core's sentences
/// print a key with.
///
/// Rust's `{:?}` is close and wrong in the details: Go writes `\x07` where
/// Rust writes `\u{7}`, and the two disagree about which characters are
/// printable at all -- `é` and `日本語` are literal in both, `\u00a0` and
/// `\u2028` are escaped in both, but Go escapes `\u0085` and
/// `\u00ad` where Rust's `is_control` does not. What decides is Go's
/// `strconv.IsPrint`, ported below.
///
/// Public because a crate's refusals quote what the deployment wrote, and
/// core's do it with `%q`: a secret name, a store id, a settings key.
pub fn go_quote(text: &str) -> String {
    let mut out = String::with_capacity(text.len() + 2);
    out.push('"');
    for ch in text.chars() {
        let rune = ch as u32;
        match ch {
            '"' | '\\' => {
                out.push('\\');
                out.push(ch);
            }
            '\u{7}' => out.push_str("\\a"),
            '\u{8}' => out.push_str("\\b"),
            '\u{c}' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{b}' => out.push_str("\\v"),
            _ if go_is_print(rune) => out.push(ch),
            _ if rune < 0x20 || rune == 0x7f => out.push_str(&format!("\\x{rune:02x}")),
            _ if rune < 0x10000 => out.push_str(&format!("\\u{rune:04x}")),
            _ => out.push_str(&format!("\\U{rune:08x}")),
        }
    }
    out.push('"');
    out
}

/// `strconv.IsPrint`: letters, marks, numbers, punctuation, symbols and the
/// ASCII space are printable, everything else is not.
///
/// The tables are `strconv/isprint.go`'s, which `makeisprint.go` generates
/// from the Unicode data of the Go release that owns them; they are copied
/// here rather than approximated, and `go_quote`'s tests pin the characters
/// where an approximation would show.
fn go_is_print(rune: u32) -> bool {
    if rune <= 0xFF {
        return (0x20..=0x7E).contains(&rune) || ((0xA1..=0xFF).contains(&rune) && rune != 0xAD);
    }
    if rune < 1 << 16 {
        // The 16-bit tables are pairs: the found index is the start or the end
        // of a range that might span the rune, and a rune in a range is
        // printable unless the second table says otherwise.
        let (index, _) = bsearch(&ISPRINT16, rune);
        let (low, high) = (ISPRINT16.get(index & !1), ISPRINT16.get(index | 1));
        match (low, high) {
            (Some(low), Some(high)) if *low <= rune && rune <= *high => {}
            _ => return false,
        }
        return !bsearch(&ISNOTPRINT16, rune).1;
    }
    let (index, _) = bsearch(&ISPRINT32, rune);
    let (low, high) = (ISPRINT32.get(index & !1), ISPRINT32.get(index | 1));
    match (low, high) {
        (Some(low), Some(high)) if *low <= rune && rune <= *high => {}
        _ => return false,
    }
    if rune >= 0x20000 {
        return true;
    }
    !bsearch(&ISNOTPRINT32, rune - 0x10000).1
}

/// Go's `bsearch`: the first index whose entry is at least `value`, and
/// whether that entry is `value`.
fn bsearch(table: &[u32], value: u32) -> (usize, bool) {
    let (mut low, mut high) = (0usize, table.len());
    while low < high {
        let middle = (low + high) / 2;
        match table.get(middle) {
            Some(entry) if *entry < value => low = middle + 1,
            _ => high = middle,
        }
    }
    (low, table.get(low) == Some(&value))
}

// `strconv/isprint.go`'s tables, as Go 1.27.2 ships them: `isPrint16` and
// `isPrint32` are the inclusive ranges whose runes are printable, and the
// `isNot` tables carve the exceptions out of them. `makeisprint.go` generates
// them from the Unicode tables of the Go release that owns them, so a Go
// upgrade that changes what prints is a change here, and the conformance
// corpus decides whether it was noticed (fixture 56's key is U+00A0, which
// only the Latin-1 fast path covers, and `go_quote_matches_strconv_quote`
// pins the 16-bit table).
const ISPRINT16: [u32; 412] = [
    0x0020, 0x007e, 0x00a1, 0x0377, 0x037a, 0x037f, 0x0384, 0x0556, 0x0559, 0x058a, 0x058d, 0x05c7,
    0x05d0, 0x05ea, 0x05ef, 0x05f4, 0x0606, 0x070d, 0x0710, 0x074a, 0x074d, 0x07b1, 0x07c0, 0x07fa,
    0x07fd, 0x082d, 0x0830, 0x085b, 0x085e, 0x086a, 0x0870, 0x088f, 0x0897, 0x098c, 0x098f, 0x0990,
    0x0993, 0x09b2, 0x09b6, 0x09b9, 0x09bc, 0x09c4, 0x09c7, 0x09c8, 0x09cb, 0x09ce, 0x09d7, 0x09d7,
    0x09dc, 0x09e3, 0x09e6, 0x09fe, 0x0a01, 0x0a0a, 0x0a0f, 0x0a10, 0x0a13, 0x0a39, 0x0a3c, 0x0a42,
    0x0a47, 0x0a48, 0x0a4b, 0x0a4d, 0x0a51, 0x0a51, 0x0a59, 0x0a5e, 0x0a66, 0x0a76, 0x0a81, 0x0ab9,
    0x0abc, 0x0acd, 0x0ad0, 0x0ad0, 0x0ae0, 0x0ae3, 0x0ae6, 0x0af1, 0x0af9, 0x0b0c, 0x0b0f, 0x0b10,
    0x0b13, 0x0b39, 0x0b3c, 0x0b44, 0x0b47, 0x0b48, 0x0b4b, 0x0b4d, 0x0b55, 0x0b57, 0x0b5c, 0x0b63,
    0x0b66, 0x0b77, 0x0b82, 0x0b8a, 0x0b8e, 0x0b95, 0x0b99, 0x0b9f, 0x0ba3, 0x0ba4, 0x0ba8, 0x0baa,
    0x0bae, 0x0bb9, 0x0bbe, 0x0bc2, 0x0bc6, 0x0bcd, 0x0bd0, 0x0bd0, 0x0bd7, 0x0bd7, 0x0be6, 0x0bfa,
    0x0c00, 0x0c39, 0x0c3c, 0x0c4d, 0x0c55, 0x0c5d, 0x0c60, 0x0c63, 0x0c66, 0x0c6f, 0x0c77, 0x0cb9,
    0x0cbc, 0x0ccd, 0x0cd5, 0x0cd6, 0x0cdc, 0x0ce3, 0x0ce6, 0x0cf3, 0x0d00, 0x0d4f, 0x0d54, 0x0d63,
    0x0d66, 0x0d96, 0x0d9a, 0x0dbd, 0x0dc0, 0x0dc6, 0x0dca, 0x0dca, 0x0dcf, 0x0ddf, 0x0de6, 0x0def,
    0x0df2, 0x0df4, 0x0e01, 0x0e3a, 0x0e3f, 0x0e5b, 0x0e81, 0x0ebd, 0x0ec0, 0x0ed9, 0x0edc, 0x0edf,
    0x0f00, 0x0f6c, 0x0f71, 0x0fda, 0x1000, 0x10c7, 0x10cd, 0x10cd, 0x10d0, 0x124d, 0x1250, 0x125d,
    0x1260, 0x128d, 0x1290, 0x12b5, 0x12b8, 0x12c5, 0x12c8, 0x1315, 0x1318, 0x135a, 0x135d, 0x137c,
    0x1380, 0x1399, 0x13a0, 0x13f5, 0x13f8, 0x13fd, 0x1400, 0x169c, 0x16a0, 0x16f8, 0x1700, 0x1715,
    0x171f, 0x1736, 0x1740, 0x1753, 0x1760, 0x1773, 0x1780, 0x17dd, 0x17e0, 0x17e9, 0x17f0, 0x17f9,
    0x1800, 0x1819, 0x1820, 0x1878, 0x1880, 0x18aa, 0x18b0, 0x18f5, 0x1900, 0x192b, 0x1930, 0x193b,
    0x1940, 0x1940, 0x1944, 0x196d, 0x1970, 0x1974, 0x1980, 0x19ab, 0x19b0, 0x19c9, 0x19d0, 0x19da,
    0x19de, 0x1a1b, 0x1a1e, 0x1a7c, 0x1a7f, 0x1a89, 0x1a90, 0x1a99, 0x1aa0, 0x1aad, 0x1ab0, 0x1add,
    0x1ae0, 0x1aeb, 0x1b00, 0x1bf3, 0x1bfc, 0x1c37, 0x1c3b, 0x1c49, 0x1c4d, 0x1c8a, 0x1c90, 0x1cba,
    0x1cbd, 0x1cc7, 0x1cd0, 0x1cfa, 0x1d00, 0x1f15, 0x1f18, 0x1f1d, 0x1f20, 0x1f45, 0x1f48, 0x1f4d,
    0x1f50, 0x1f7d, 0x1f80, 0x1fd3, 0x1fd6, 0x1fef, 0x1ff2, 0x1ffe, 0x2010, 0x2027, 0x2030, 0x205e,
    0x2070, 0x2071, 0x2074, 0x209c, 0x20a0, 0x20c1, 0x20d0, 0x20f0, 0x2100, 0x218b, 0x2190, 0x2429,
    0x2440, 0x244a, 0x2460, 0x2b73, 0x2b76, 0x2cf3, 0x2cf9, 0x2d27, 0x2d2d, 0x2d2d, 0x2d30, 0x2d67,
    0x2d6f, 0x2d70, 0x2d7f, 0x2d96, 0x2da0, 0x2e5d, 0x2e80, 0x2ef3, 0x2f00, 0x2fd5, 0x2ff0, 0x3096,
    0x3099, 0x30ff, 0x3105, 0x31e5, 0x31ef, 0xa48c, 0xa490, 0xa4c6, 0xa4d0, 0xa62b, 0xa640, 0xa6f7,
    0xa700, 0xa7dc, 0xa7f1, 0xa82c, 0xa830, 0xa839, 0xa840, 0xa877, 0xa880, 0xa8c5, 0xa8ce, 0xa8d9,
    0xa8e0, 0xa953, 0xa95f, 0xa97c, 0xa980, 0xa9d9, 0xa9de, 0xaa36, 0xaa40, 0xaa4d, 0xaa50, 0xaa59,
    0xaa5c, 0xaac2, 0xaadb, 0xaaf6, 0xab01, 0xab06, 0xab09, 0xab0e, 0xab11, 0xab16, 0xab20, 0xab6b,
    0xab70, 0xabed, 0xabf0, 0xabf9, 0xac00, 0xd7a3, 0xd7b0, 0xd7c6, 0xd7cb, 0xd7fb, 0xf900, 0xfa6d,
    0xfa70, 0xfad9, 0xfb00, 0xfb06, 0xfb13, 0xfb17, 0xfb1d, 0xfdcf, 0xfdf0, 0xfe19, 0xfe20, 0xfe6b,
    0xfe70, 0xfefc, 0xff01, 0xffbe, 0xffc2, 0xffc7, 0xffca, 0xffcf, 0xffd2, 0xffd7, 0xffda, 0xffdc,
    0xffe0, 0xffee, 0xfffc, 0xfffd,
];
const ISNOTPRINT16: [u32; 132] = [
    0x00ad, 0x038b, 0x038d, 0x03a2, 0x0530, 0x0590, 0x061c, 0x06dd, 0x083f, 0x085f, 0x08e2, 0x0984,
    0x09a9, 0x09b1, 0x09de, 0x0a04, 0x0a29, 0x0a31, 0x0a34, 0x0a37, 0x0a3d, 0x0a5d, 0x0a84, 0x0a8e,
    0x0a92, 0x0aa9, 0x0ab1, 0x0ab4, 0x0ac6, 0x0aca, 0x0b00, 0x0b04, 0x0b29, 0x0b31, 0x0b34, 0x0b5e,
    0x0b84, 0x0b91, 0x0b9b, 0x0b9d, 0x0bc9, 0x0c0d, 0x0c11, 0x0c29, 0x0c45, 0x0c49, 0x0c57, 0x0c5b,
    0x0c8d, 0x0c91, 0x0ca9, 0x0cb4, 0x0cc5, 0x0cc9, 0x0cdf, 0x0cf0, 0x0d0d, 0x0d11, 0x0d45, 0x0d49,
    0x0d80, 0x0d84, 0x0db2, 0x0dbc, 0x0dd5, 0x0dd7, 0x0e83, 0x0e85, 0x0e8b, 0x0ea4, 0x0ea6, 0x0ec5,
    0x0ec7, 0x0ecf, 0x0f48, 0x0f98, 0x0fbd, 0x0fcd, 0x10c6, 0x1249, 0x1257, 0x1259, 0x1289, 0x12b1,
    0x12bf, 0x12c1, 0x12d7, 0x1311, 0x1680, 0x176d, 0x1771, 0x180e, 0x191f, 0x1a5f, 0x1b4d, 0x1f58,
    0x1f5a, 0x1f5c, 0x1f5e, 0x1fb5, 0x1fc5, 0x1fdc, 0x1ff5, 0x208f, 0x2d26, 0x2da7, 0x2daf, 0x2db7,
    0x2dbf, 0x2dc7, 0x2dcf, 0x2dd7, 0x2ddf, 0x2e9a, 0x3000, 0x3040, 0x3130, 0x318f, 0x321f, 0xa9ce,
    0xa9ff, 0xab27, 0xab2f, 0xfb37, 0xfb3d, 0xfb3f, 0xfb42, 0xfb45, 0xfe53, 0xfe67, 0xfe75, 0xffe7,
];
const ISPRINT32: [u32; 566] = [
    0x010000, 0x01004d, 0x010050, 0x01005d, 0x010080, 0x0100fa, 0x010100, 0x010102, 0x010107,
    0x010133, 0x010137, 0x01019c, 0x0101a0, 0x0101a0, 0x0101d0, 0x0101fd, 0x010280, 0x01029c,
    0x0102a0, 0x0102d0, 0x0102e0, 0x0102fb, 0x010300, 0x010323, 0x01032d, 0x01034a, 0x010350,
    0x01037a, 0x010380, 0x0103c3, 0x0103c8, 0x0103d5, 0x010400, 0x01049d, 0x0104a0, 0x0104a9,
    0x0104b0, 0x0104d3, 0x0104d8, 0x0104fb, 0x010500, 0x010527, 0x010530, 0x010563, 0x01056f,
    0x0105bc, 0x0105c0, 0x0105f3, 0x010600, 0x010736, 0x010740, 0x010755, 0x010760, 0x010767,
    0x010780, 0x0107ba, 0x010800, 0x010805, 0x010808, 0x010838, 0x01083c, 0x01083c, 0x01083f,
    0x01089e, 0x0108a7, 0x0108af, 0x0108e0, 0x0108f5, 0x0108fb, 0x01091b, 0x01091f, 0x010939,
    0x01093f, 0x010959, 0x010980, 0x0109b7, 0x0109bc, 0x0109cf, 0x0109d2, 0x010a06, 0x010a0c,
    0x010a35, 0x010a38, 0x010a3a, 0x010a3f, 0x010a48, 0x010a50, 0x010a58, 0x010a60, 0x010a9f,
    0x010ac0, 0x010ae6, 0x010aeb, 0x010af6, 0x010b00, 0x010b35, 0x010b39, 0x010b55, 0x010b58,
    0x010b72, 0x010b78, 0x010b91, 0x010b99, 0x010b9c, 0x010ba9, 0x010baf, 0x010c00, 0x010c48,
    0x010c80, 0x010cb2, 0x010cc0, 0x010cf2, 0x010cfa, 0x010d27, 0x010d30, 0x010d39, 0x010d40,
    0x010d65, 0x010d69, 0x010d85, 0x010d8e, 0x010d8f, 0x010e60, 0x010ead, 0x010eb0, 0x010eb1,
    0x010ec2, 0x010ec7, 0x010ed0, 0x010ed8, 0x010efa, 0x010f27, 0x010f30, 0x010f59, 0x010f70,
    0x010f89, 0x010fb0, 0x010fcb, 0x010fe0, 0x010ff6, 0x011000, 0x01104d, 0x011052, 0x011075,
    0x01107f, 0x0110c2, 0x0110d0, 0x0110e8, 0x0110f0, 0x0110f9, 0x011100, 0x011147, 0x011150,
    0x011176, 0x011180, 0x0111f4, 0x011200, 0x011241, 0x011280, 0x0112a9, 0x0112b0, 0x0112ea,
    0x0112f0, 0x0112f9, 0x011300, 0x01130c, 0x01130f, 0x011310, 0x011313, 0x011344, 0x011347,
    0x011348, 0x01134b, 0x01134d, 0x011350, 0x011350, 0x011357, 0x011357, 0x01135d, 0x011363,
    0x011366, 0x01136c, 0x011370, 0x011374, 0x011380, 0x01138b, 0x01138e, 0x0113c2, 0x0113c5,
    0x0113d8, 0x0113e1, 0x0113e2, 0x011400, 0x011461, 0x011480, 0x0114c7, 0x0114d0, 0x0114d9,
    0x011580, 0x0115b5, 0x0115b8, 0x0115dd, 0x011600, 0x011644, 0x011650, 0x011659, 0x011660,
    0x01166c, 0x011680, 0x0116b9, 0x0116c0, 0x0116c9, 0x0116d0, 0x0116e3, 0x011700, 0x01171a,
    0x01171d, 0x01172b, 0x011730, 0x011746, 0x011800, 0x01183b, 0x0118a0, 0x0118f2, 0x0118ff,
    0x011906, 0x011909, 0x011909, 0x01190c, 0x011938, 0x01193b, 0x011946, 0x011950, 0x011959,
    0x0119a0, 0x0119a7, 0x0119aa, 0x0119d7, 0x0119da, 0x0119e4, 0x011a00, 0x011a47, 0x011a50,
    0x011aa2, 0x011ab0, 0x011af8, 0x011b00, 0x011b09, 0x011b60, 0x011b67, 0x011bc0, 0x011be1,
    0x011bf0, 0x011bf9, 0x011c00, 0x011c45, 0x011c50, 0x011c6c, 0x011c70, 0x011c8f, 0x011c92,
    0x011cb6, 0x011d00, 0x011d36, 0x011d3a, 0x011d47, 0x011d50, 0x011d59, 0x011d60, 0x011d98,
    0x011da0, 0x011da9, 0x011db0, 0x011ddb, 0x011de0, 0x011de9, 0x011ee0, 0x011ef8, 0x011f00,
    0x011f3a, 0x011f3e, 0x011f5a, 0x011fb0, 0x011fb0, 0x011fc0, 0x011ff1, 0x011fff, 0x012399,
    0x012400, 0x012474, 0x012480, 0x012543, 0x012f90, 0x012ff2, 0x013000, 0x01342f, 0x013440,
    0x013455, 0x013460, 0x0143fa, 0x014400, 0x014646, 0x016100, 0x016139, 0x016800, 0x016a38,
    0x016a40, 0x016a69, 0x016a6e, 0x016ac9, 0x016ad0, 0x016aed, 0x016af0, 0x016af5, 0x016b00,
    0x016b45, 0x016b50, 0x016b77, 0x016b7d, 0x016b8f, 0x016d40, 0x016d79, 0x016e40, 0x016e9a,
    0x016ea0, 0x016eb8, 0x016ebb, 0x016ed3, 0x016f00, 0x016f4a, 0x016f4f, 0x016f87, 0x016f8f,
    0x016f9f, 0x016fe0, 0x016fe4, 0x016ff0, 0x016ff6, 0x017000, 0x018cd5, 0x018cff, 0x018d1e,
    0x018d80, 0x018df2, 0x01aff0, 0x01b122, 0x01b132, 0x01b132, 0x01b150, 0x01b152, 0x01b155,
    0x01b155, 0x01b164, 0x01b167, 0x01b170, 0x01b2fb, 0x01bc00, 0x01bc6a, 0x01bc70, 0x01bc7c,
    0x01bc80, 0x01bc88, 0x01bc90, 0x01bc99, 0x01bc9c, 0x01bc9f, 0x01cc00, 0x01ccfc, 0x01cd00,
    0x01ceb3, 0x01ceba, 0x01ced0, 0x01cee0, 0x01cef0, 0x01cf00, 0x01cf2d, 0x01cf30, 0x01cf46,
    0x01cf50, 0x01cfc3, 0x01d000, 0x01d0f5, 0x01d100, 0x01d126, 0x01d129, 0x01d172, 0x01d17b,
    0x01d1ea, 0x01d200, 0x01d245, 0x01d2c0, 0x01d2d3, 0x01d2e0, 0x01d2f3, 0x01d300, 0x01d356,
    0x01d360, 0x01d378, 0x01d400, 0x01d49f, 0x01d4a2, 0x01d4a2, 0x01d4a5, 0x01d4a6, 0x01d4a9,
    0x01d50a, 0x01d50d, 0x01d546, 0x01d54a, 0x01d6a5, 0x01d6a8, 0x01d7cb, 0x01d7ce, 0x01da8b,
    0x01da9b, 0x01daaf, 0x01df00, 0x01df1e, 0x01df25, 0x01df2a, 0x01e000, 0x01e018, 0x01e01b,
    0x01e02a, 0x01e030, 0x01e06d, 0x01e08f, 0x01e08f, 0x01e100, 0x01e12c, 0x01e130, 0x01e13d,
    0x01e140, 0x01e149, 0x01e14e, 0x01e14f, 0x01e290, 0x01e2ae, 0x01e2c0, 0x01e2f9, 0x01e2ff,
    0x01e2ff, 0x01e4d0, 0x01e4f9, 0x01e5d0, 0x01e5fa, 0x01e5ff, 0x01e5ff, 0x01e6c0, 0x01e6f5,
    0x01e6fe, 0x01e6ff, 0x01e7e0, 0x01e8c4, 0x01e8c7, 0x01e8d6, 0x01e900, 0x01e94b, 0x01e950,
    0x01e959, 0x01e95e, 0x01e95f, 0x01ec71, 0x01ecb4, 0x01ed01, 0x01ed3d, 0x01ee00, 0x01ee24,
    0x01ee27, 0x01ee3b, 0x01ee42, 0x01ee42, 0x01ee47, 0x01ee54, 0x01ee57, 0x01ee64, 0x01ee67,
    0x01ee9b, 0x01eea1, 0x01eebb, 0x01eef0, 0x01eef1, 0x01f000, 0x01f02b, 0x01f030, 0x01f093,
    0x01f0a0, 0x01f0ae, 0x01f0b1, 0x01f0f5, 0x01f100, 0x01f1ad, 0x01f1e6, 0x01f202, 0x01f210,
    0x01f23b, 0x01f240, 0x01f248, 0x01f250, 0x01f251, 0x01f260, 0x01f265, 0x01f300, 0x01f6d8,
    0x01f6dc, 0x01f6ec, 0x01f6f0, 0x01f6fc, 0x01f700, 0x01f7d9, 0x01f7e0, 0x01f7eb, 0x01f7f0,
    0x01f7f0, 0x01f800, 0x01f80b, 0x01f810, 0x01f847, 0x01f850, 0x01f859, 0x01f860, 0x01f887,
    0x01f890, 0x01f8ad, 0x01f8b0, 0x01f8bb, 0x01f8c0, 0x01f8c1, 0x01f8d0, 0x01f8d8, 0x01f900,
    0x01fa57, 0x01fa60, 0x01fa6d, 0x01fa70, 0x01fa7c, 0x01fa80, 0x01fa8a, 0x01fa8e, 0x01fac8,
    0x01facd, 0x01fadc, 0x01fadf, 0x01faea, 0x01faef, 0x01faf8, 0x01fb00, 0x01fbfa, 0x020000,
    0x02a6df, 0x02a700, 0x02b81d, 0x02b820, 0x02cead, 0x02ceb0, 0x02ebe0, 0x02ebf0, 0x02ee5d,
    0x02f800, 0x02fa1d, 0x030000, 0x03134a, 0x031350, 0x033479, 0x0e0100, 0x0e01ef,
];
const ISNOTPRINT32: [u32; 120] = [
    0x000c, 0x0027, 0x003b, 0x003e, 0x018f, 0x039e, 0x057b, 0x058b, 0x0593, 0x0596, 0x05a2, 0x05b2,
    0x05ba, 0x0786, 0x07b1, 0x0809, 0x0836, 0x0856, 0x08f3, 0x0a04, 0x0a14, 0x0a18, 0x0e7f, 0x0eaa,
    0x10bd, 0x1135, 0x11e0, 0x1212, 0x1287, 0x1289, 0x128e, 0x129e, 0x1304, 0x1329, 0x1331, 0x1334,
    0x133a, 0x138a, 0x138f, 0x13b6, 0x13c1, 0x13c6, 0x13cb, 0x13d6, 0x145c, 0x1914, 0x1917, 0x1936,
    0x1c09, 0x1c37, 0x1ca8, 0x1d07, 0x1d0a, 0x1d3b, 0x1d3e, 0x1d66, 0x1d69, 0x1d8f, 0x1d92, 0x1f11,
    0x246f, 0x6a5f, 0x6abf, 0x6b5a, 0x6b62, 0xaff4, 0xaffc, 0xafff, 0xd455, 0xd49d, 0xd4ad, 0xd4ba,
    0xd4bc, 0xd4c4, 0xd506, 0xd515, 0xd51d, 0xd53a, 0xd53f, 0xd545, 0xd551, 0xdaa0, 0xe007, 0xe022,
    0xe025, 0xe6df, 0xe7e7, 0xe7ec, 0xe7ef, 0xe7ff, 0xee04, 0xee20, 0xee23, 0xee28, 0xee33, 0xee38,
    0xee3a, 0xee48, 0xee4a, 0xee4c, 0xee50, 0xee53, 0xee58, 0xee5a, 0xee5c, 0xee5e, 0xee60, 0xee63,
    0xee6b, 0xee73, 0xee78, 0xee7d, 0xee7f, 0xee8a, 0xeea4, 0xeeaa, 0xf0c0, 0xf0d0, 0xfac7, 0xfb93,
];

/// `isMerge`: a plain, untagged `<<`; anything else is a normal key.
fn is_merge_key(key: &Yaml<'_>) -> bool {
    matches!(
        key,
        Yaml::Representation(text, ScalarStyle::Plain, None) if text.as_ref() == "<<"
    )
}

fn merge_sources<'a>(value: &'a Yaml<'a>) -> Result<Vec<&'a Mapping<'a>>, String> {
    match value {
        Yaml::Mapping(map) => Ok(vec![map]),
        Yaml::Sequence(seq) => {
            let mut out = Vec::new();
            for item in seq {
                match item {
                    Yaml::Mapping(map) => out.push(map),
                    _ => {
                        return Err(
                            "map merge requires map or sequence of maps as the value".into()
                        );
                    }
                }
            }
            Ok(out)
        }
        _ => Err("map merge requires map or sequence of maps as the value".into()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Every expectation below was copied out of the oracle's own output for
    // the same document (`tools/conformance-oracle`, Go 1.27.2), and
    // `rust/conformance` is what keeps the copies honest: the same documents
    // are fixtures there, compared against the oracle on every run.
    const DUPLICATE_WRAPPER: &str = "config utils: convert YAML to JSON: error converting YAML to JSON: yaml: unmarshal errors:";

    fn refusal(document: &str) -> String {
        match to_json(document.as_bytes()) {
            Ok(json) => panic!("expected a refusal, got {json}"),
            Err(error) => error,
        }
    }

    fn converts(document: &str) -> String {
        match to_json(document.as_bytes()) {
            Ok(json) => json,
            Err(error) => panic!("expected a conversion, got {error}"),
        }
    }

    fn refusal_bytes(document: &[u8]) -> String {
        match to_json(document) {
            Ok(json) => panic!("expected a refusal, got {json}"),
            Err(error) => error,
        }
    }

    #[test]
    fn every_refusal_carries_cores_prefix() {
        assert_eq!(refusal("{ \"a\": }"), "config utils: invalid JSON document");
        assert_eq!(
            refusal("a: 1\n---\nb: 2\n"),
            "config utils: multiple YAML documents"
        );
        assert_eq!(
            refusal("a: 1\na: 2\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key \"a\" already set in map")
        );
    }

    #[test]
    fn unusable_keys_are_collected_sorted_and_deduplicated() {
        assert_eq!(
            refusal("~: one\n"),
            "config utils: convert YAML to JSON: YAML key !!null null cannot be spelled as a JSON key"
        );
        assert_eq!(
            refusal("18446744073709551615: a\n"),
            "config utils: convert YAML to JSON: YAML key !!int 18446744073709551615 cannot be spelled as a JSON key"
        );
        // Sorted by spelling, and a key written twice is named once.
        assert_eq!(
            refusal("~: a\n18446744073709551615: b\n~: c\n"),
            "config utils: convert YAML to JSON: YAML keys !!int 18446744073709551615, !!null null cannot be spelled as JSON keys"
        );
    }

    #[test]
    fn a_duplicate_key_is_reported_per_occurrence_in_document_order() {
        assert_eq!(
            refusal("a: 1\na: 2\nb: 1\nb: 2\n"),
            format!(
                "{DUPLICATE_WRAPPER}\n  line 2: key \"a\" already set in map\n  line 4: key \"b\" already set in map"
            )
        );
        // A NaN never equals itself, so a document may hold `.nan` twice --
        // and then it is a collapse, not a repetition.
        assert_eq!(
            refusal(".nan: a\n.NaN: b\n.nan: c\n"),
            "config utils: convert YAML to JSON: YAML keys !!float .nan, !!float .nan, !!float .nan convert to the same JSON key \".nan\""
        );
        // But `-0.0 == 0.0`, so these two are one key.
        assert_eq!(
            refusal("0.0: a\n-0.0: b\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key -0 already set in map")
        );
        // And equality is on the *resolved* key, not on the text: `01` is the
        // octal 1, and `true` and `yes` are the same boolean.
        assert_eq!(
            refusal("01: a\n1: b\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key 1 already set in map")
        );
        assert_eq!(
            refusal("true: a\nyes: b\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key true already set in map")
        );
    }

    #[test]
    fn a_repeated_key_is_spelled_the_way_go_spells_it() {
        // A control character, where Rust's `{:?}` would write `\u{7}`.
        assert_eq!(
            refusal("\"\\u0007\": 1\n\"\\u0007\": 2\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key \"\\a\" already set in map")
        );
        // A non-breaking space, which Go escapes and Rust would not.
        assert_eq!(
            refusal("\"\u{a0}\": 1\n\"\u{a0}\": 2\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key \"\\u00a0\" already set in map")
        );
    }

    #[test]
    fn a_merged_key_is_an_ordinary_key() {
        // The merged copy is inserted where the `<<` sits, so the entry that
        // already holds the key is reported on its own line -- here the
        // anchored one, which is the one being inserted.
        assert_eq!(
            refusal("base: &b {k: 1}\nd:\n  k: 1\n  <<: *b\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 1: key \"k\" already set in map")
        );
        // A sequence of sources is inserted backwards: the earlier source
        // takes precedence, and it is the one that conflicts.
        assert_eq!(
            refusal("base1: &b1 {k: 1}\nbase2: &b2 {k: 2}\nd:\n  <<: [*b1, *b2]\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 1: key \"k\" already set in map")
        );
        // An anchored mapping holding a duplicate reports it once per use.
        assert_eq!(
            refusal("base: &b\n  k: 1\n  k: 2\nd:\n  <<: *b\n"),
            format!(
                "{DUPLICATE_WRAPPER}\n  line 3: key \"k\" already set in map\n  line 3: key \"k\" already set in map"
            )
        );
        // A merge that collides with nothing converts.
        assert_eq!(
            converts("base: &b {k: 1}\nd:\n  <<: *b\n  j: 2\n"),
            "{\"base\":{\"k\":1},\"d\":{\"j\":2,\"k\":1}}"
        );
    }

    #[test]
    fn collisions_name_every_key_of_the_group() {
        assert_eq!(
            refusal("1: a\n\"1\": b\n1.0: c\n"),
            "config utils: convert YAML to JSON: YAML keys !!float 1, !!int 1, !!str \"1\" convert to the same JSON key \"1\""
        );
        // Two mappings that collide the same way are one sentence between
        // them, and the sentences are ordered by JSON key.
        assert_eq!(
            refusal("a:\n  1: x\n  \"1\": y\nb:\n  true: 1\n  \"true\": 2\n"),
            "config utils: convert YAML to JSON: YAML keys !!int 1, !!str \"1\" convert to the same JSON key \"1\"; YAML keys !!bool true, !!str \"true\" convert to the same JSON key \"true\""
        );
    }

    #[test]
    fn the_refusals_follow_cores_order() {
        // An unusable key outranks a repetition and a collapse.
        assert_eq!(
            refusal("~: a\n~: b\n1: c\n\"1\": d\n"),
            "config utils: convert YAML to JSON: YAML key !!null null cannot be spelled as a JSON key"
        );
        // A repetition outranks a value JSON cannot write.
        assert_eq!(
            refusal("a: 1\na: 2\nb: .nan\n"),
            format!("{DUPLICATE_WRAPPER}\n  line 2: key \"a\" already set in map")
        );
        // And a value JSON cannot write outranks a collapse: the library
        // marshals the document before core looks for collisions.
        assert_eq!(
            refusal("a: .nan\n1: b\n\"1\": c\n"),
            "config utils: convert YAML to JSON: error converting YAML to JSON: json: unsupported value: NaN"
        );
        // A tag the scalar cannot satisfy is a decode refusal, so it outranks
        // both -- and it is reported without the conversion's wrapper.
        assert_eq!(
            refusal("a: !!int abc\nb: 1\nb: 2\n"),
            "config utils: yaml: cannot decode !!str `abc` as a !!int"
        );
    }

    #[test]
    fn an_unsupported_value_is_the_first_in_sorted_key_order() {
        // Document order would find `.inf` first; the marshal walks the
        // object's keys in sorted order, so it finds `.nan`.
        assert_eq!(
            refusal("a:\n  z: .inf\n  b: .nan\n"),
            "config utils: convert YAML to JSON: error converting YAML to JSON: json: unsupported value: NaN"
        );
        assert_eq!(
            refusal("a:\n  b: .inf\n  z: .nan\n"),
            "config utils: convert YAML to JSON: error converting YAML to JSON: json: unsupported value: +Inf"
        );
        assert_eq!(
            refusal("a: !!float .inf\n"),
            "config utils: convert YAML to JSON: error converting YAML to JSON: json: unsupported value: +Inf"
        );
    }

    #[test]
    fn float_keys_use_the_float32_shortest_form() {
        // `strconv.FormatFloat(f, 'g', -1, 32)`: float32 precision, Go's
        // exponent thresholds (`1e+06` past 999999, `1e-06` at a millionth),
        // and the special floats spelled the YAML way.
        assert_eq!(
            converts("1e30: a\n1e-30: b\n1.5e300: c\n0.000001: d\n"),
            "{\".inf\":\"c\",\"1e+30\":\"a\",\"1e-06\":\"d\",\"1e-30\":\"b\"}"
        );
        assert_eq!(converts("999999: a\n"), "{\"999999\":\"a\"}");
        assert_eq!(converts("-0.0: a\n"), "{\"-0\":\"a\"}");
    }

    #[test]
    fn reader_refusals_are_libyamls() {
        assert_eq!(
            refusal("a: \u{1}0\n"),
            "config utils: yaml: control characters are not allowed"
        );
        assert_eq!(
            refusal_bytes(b"a: \xc3\x28\n"),
            "config utils: yaml: invalid trailing UTF-8 octet"
        );
        assert_eq!(
            refusal_bytes(b"a: \xff\n"),
            "config utils: yaml: invalid leading UTF-8 octet"
        );
        assert_eq!(
            refusal_bytes(b"a: \xc0\x80\n"),
            "config utils: yaml: invalid length of a UTF-8 sequence"
        );
        assert_eq!(
            refusal_bytes(b"a: \xed\xa0\x80\n"),
            "config utils: yaml: invalid Unicode character"
        );
        // A tab is a printable YAML character, and so is a line break.
        assert_eq!(converts("a: \"x\ty\"\n"), "{\"a\":\"x\\ty\"}");
    }

    #[test]
    fn tags_are_refused_the_way_go_yaml_refuses_them() {
        assert_eq!(
            refusal("a: !!int abc\n"),
            "config utils: yaml: cannot decode !!str `abc` as a !!int"
        );
        assert_eq!(
            refusal("a: !!null 5\n"),
            "config utils: yaml: cannot decode !!int `5` as a !!null"
        );
        assert_eq!(
            refusal("a: !!float abc\n"),
            "config utils: yaml: cannot decode !!str `abc` as a !!float"
        );
        assert_eq!(
            refusal("a: !!bool maybe\n"),
            "config utils: yaml: cannot decode !!str `maybe` as a !!bool"
        );
        assert_eq!(
            refusal("a: !!binary \"###\"\n"),
            "config utils: yaml: !!binary value contains invalid base64 data"
        );
        // The document's own tag wins over the text it is written on.
        assert_eq!(converts("a: !!int \"5\"\n"), "{\"a\":5}");
        // A merge value has to be a mapping, or a sequence of them.
        assert_eq!(
            refusal("b:\n  <<: 5\n"),
            "config utils: yaml: map merge requires map or sequence of maps as the value"
        );
    }

    #[test]
    fn go_quote_matches_strconv_quote() {
        assert_eq!(go_quote("a"), "\"a\"");
        assert_eq!(go_quote("\u{7}"), "\"\\a\"");
        assert_eq!(go_quote("\u{0}"), "\"\\x00\"");
        assert_eq!(go_quote("\u{7f}"), "\"\\x7f\"");
        assert_eq!(go_quote("é"), "\"é\"");
        assert_eq!(go_quote("\u{a0}"), "\"\\u00a0\"");
        assert_eq!(go_quote("\u{2028}"), "\"\\u2028\"");
        assert_eq!(go_quote("\u{3000}"), "\"\\u3000\"");
        assert_eq!(go_quote("\u{e000}"), "\"\\ue000\"");
        assert_eq!(go_quote("\u{feff}"), "\"\\ufeff\"");
        assert_eq!(go_quote("\u{fffe}"), "\"\\ufffe\"");
        assert_eq!(go_quote("\u{1d173}"), "\"\\U0001d173\"");
        assert_eq!(go_quote("\u{10ffff}"), "\"\\U0010ffff\"");
        // The 32-bit table is the only thing that decides these: a
        // private-use rune is not printable to Go, an emoji is, and an
        // ideographic description character is too.
        assert_eq!(go_quote("🙂"), "\"🙂\"");
        assert_eq!(go_quote("\u{2ff0}"), "\"⿰\"");
    }

    #[test]
    fn go_float_g_matches_fmt() {
        assert_eq!(go_float_g(1.5), "1.5");
        assert_eq!(go_float_g(1e6), "1e+06");
        assert_eq!(go_float_g(999_999.0), "999999");
        assert_eq!(go_float_g(123_456.0), "123456");
        assert_eq!(go_float_g(1_234_567.0), "1.234567e+06");
        assert_eq!(go_float_g(0.0001), "0.0001");
        assert_eq!(go_float_g(0.00001), "1e-05");
        assert_eq!(go_float_g(1e20), "1e+20");
        assert_eq!(go_float_g(1e100), "1e+100");
        assert_eq!(go_float_g(-0.0), "-0");
        assert_eq!(go_float_g(f64::NAN), "NaN");
        assert_eq!(go_float_g(f64::INFINITY), "+Inf");
        assert_eq!(go_float_g(f64::NEG_INFINITY), "-Inf");
    }

    #[test]
    fn go_syntax_renders_collections() {
        assert_eq!(
            refusal("? [a, b]\n: x\n"),
            "config utils: yaml: invalid map key: []interface {}{\"a\", \"b\"}"
        );
        // A mapping key whose keys are all strings is sorted by `fmt`; one
        // with mixed types is ordered by type address in Go, which no other
        // program can reproduce -- see known-divergences.txt's `error-text`
        // entry for fixture 65.
        assert_eq!(
            refusal("? {c: d, a: b}\n: x\n"),
            "config utils: yaml: invalid map key: map[interface {}]interface {}{\"a\":\"b\", \"c\":\"d\"}"
        );
        // A nil prints as the interface it sits in, inside a collection.
        assert_eq!(
            refusal("? [~, 1]\n: x\n"),
            "config utils: yaml: invalid map key: []interface {}{interface {}(nil), 1}"
        );
        assert_eq!(
            refusal("? {a: ~}\n: x\n"),
            "config utils: yaml: invalid map key: map[interface {}]interface {}{\"a\":interface {}(nil)}"
        );
    }

    #[test]
    fn an_invalid_map_key_is_a_decode_refusal() {
        // The document-level decode fails on the key, so the refusal carries
        // the decode prefix and no conversion wrapper -- and a repetition
        // beside it is never reported, because a fail drops the terrors.
        assert_eq!(
            refusal("1: a\n1: b\n? [x]\n: y\n"),
            "config utils: yaml: invalid map key: []interface {}{\"x\"}"
        );
    }
}
