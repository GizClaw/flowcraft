//! `core/utils`'s decode layer, ported: the strict YAML/JSON conversion
//! (`utils.ToJSON`) and the sniffing rule that decides what to feed it
//! (`utils.FormatOf`).
//!
//! The Go implementation is the oracle. `rust/conformance` diffs this module
//! against it file by file, byte for byte, and every rule below cites the Go
//! behaviour it mirrors; the corpus in `rust/conformance/fixtures` is the
//! check on that claim.
//!
//! Errors are `String`s for now, exactly as the probe had them. The error
//! contract -- the kinds, and the wording a caller may match on -- is a step
//! of its own, and it is not this one.

use base64::Engine as _;
use flowcraft_contract::json;
use saphyr::{Mapping, Scalar, Yaml};
use saphyr_parser::{Parser, ScalarStyle, Tag};
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

/// JSON input is validated and passed through as-is, which is what
/// `utils.ToJSON` does with a document `utils.FormatOf` calls JSON: the bytes
/// are not re-encoded, so key order and number spelling survive.
fn passthrough_json(data: &[u8]) -> Result<String, String> {
    let text = std::str::from_utf8(data).map_err(|e| format!("invalid UTF-8: {e}"))?;
    serde_json::from_str::<Value>(text).map_err(|e| format!("invalid JSON document: {e}"))?;
    Ok(text.to_string())
}

/// `utils.ToJSON`: the strict conversion core applies to a configuration
/// document before anything reads it.
///
/// The rules below are go-yaml v2's as `sigs.k8s.io/yaml` uses them, quirks
/// included: scalar resolution, key coercion, merge keys, duplicate keys, the
/// key-collapse refusal, the alias guard, and the float64 funnel every YAML
/// number goes through.
pub fn to_json(data: &[u8]) -> Result<String, String> {
    if format_of(data) == Format::Json {
        return passthrough_json(data);
    }
    let text = text_of(data)?;
    // libyaml (hence go-yaml) strips a BOM; the pure-Rust scanner does not.
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    directive_guard(text)?;
    alias_guard(text)?;

    let mut loader: saphyr::YamlLoader<'_, Yaml<'_>> = saphyr::YamlLoader::default();
    loader.early_parse(false);
    let mut parser = Parser::new_from_str(text);
    parser.load(&mut loader, true).map_err(|e| e.to_string())?;
    if let Some(error) = loader.error() {
        return Err(error.to_string());
    }
    let docs = loader.into_documents();
    let value = match docs.len() {
        0 => Value::Null,
        1 => go_value(&docs[0])?,
        _ => return Err("multiple YAML documents".into()),
    };
    Ok(json::to_string(&value))
}

fn text_of(data: &[u8]) -> Result<&str, String> {
    std::str::from_utf8(data).map_err(|e| format!("invalid UTF-8: {e}"))
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

fn go_value(node: &Yaml<'_>) -> Result<Value, String> {
    match node {
        Yaml::Representation(text, style, tag) => scalar_go(text, *style, tag.as_deref()),
        Yaml::Value(scalar) => resolved_to_json(match scalar {
            Scalar::Null => Resolved::Null,
            Scalar::Boolean(b) => Resolved::Bool(*b),
            Scalar::Integer(i) => Resolved::Int(*i),
            Scalar::FloatingPoint(f) => Resolved::F64(f.0),
            Scalar::String(s) => Resolved::Str(s.to_string()),
        }),
        Yaml::Sequence(seq) => {
            let mut out = Vec::with_capacity(seq.len());
            for item in seq {
                out.push(go_value(item)?);
            }
            Ok(Value::Array(out))
        }
        Yaml::Mapping(map) => go_mapping(map),
        // A tag on a collection stays a tag on the node; the JSON view only
        // cares about the content.
        Yaml::Tagged(_, inner) => go_value(inner),
        Yaml::Alias(_) => Err("unresolved alias".into()),
        Yaml::BadValue => Err("bad value".into()),
    }
}

fn scalar_go(text: &str, style: ScalarStyle, tag: Option<&Tag>) -> Result<Value, String> {
    if let Some(tag) = tag {
        return tagged_go(text, tag);
    }
    // Quoted and block scalars are strings; go-yaml tags them `!!str`.
    if style != ScalarStyle::Plain {
        return Ok(Value::String(text.to_string()));
    }
    resolved_to_json(resolve_plain(text)?)
}

/// `utils.ToJSON` funnels every number through `interface{}`, i.e. through
/// float64, so a YAML document's big integers come out rounded. The JSON
/// *input* path does not: `utils.ToJSON` passes valid JSON through untouched.
fn resolved_to_json(resolved: Resolved) -> Result<Value, String> {
    Ok(match resolved {
        Resolved::Null => Value::Null,
        Resolved::Bool(b) => Value::Bool(b),
        Resolved::Int(i) => number(i as f64)?,
        Resolved::UInt(u) => number(u as f64)?,
        Resolved::F64(f) => number(f)?,
        Resolved::Str(s) => Value::String(s),
    })
}

fn number(f: f64) -> Result<Value, String> {
    Number::from_f64(f)
        .map(Value::Number)
        .ok_or_else(|| format!("unsupported value: {f}"))
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
    // A leading zero selects octal, and "08" then fails — which is exactly
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
fn tagged_go(text: &str, tag: &Tag) -> Result<Value, String> {
    if !tag.is_yaml_core_schema() {
        return Ok(Value::String(text.to_string()));
    }
    match tag.suffix.as_str() {
        "str" => Ok(Value::String(text.to_string())),
        "binary" => {
            let cleaned: String = text.chars().filter(|c| !c.is_whitespace()).collect();
            let bytes = base64::engine::general_purpose::STANDARD
                .decode(cleaned)
                .map_err(|e| format!("!!binary value contains invalid base64 data: {e}"))?;
            Ok(Value::String(String::from_utf8_lossy(&bytes).into_owned()))
        }
        "null" => match resolve_plain(text)? {
            Resolved::Null => Ok(Value::Null),
            other => Err(format!("cannot decode {other:?} as a !!null")),
        },
        "bool" => match resolve_plain(text)? {
            Resolved::Bool(b) => Ok(Value::Bool(b)),
            other => Err(format!("cannot decode {other:?} as a !!bool")),
        },
        "int" => match resolve_plain(text)? {
            Resolved::Int(i) => resolved_to_json(Resolved::Int(i)),
            Resolved::UInt(u) => resolved_to_json(Resolved::UInt(u)),
            other => Err(format!("cannot decode {other:?} as a !!int")),
        },
        "float" => match resolve_plain(text)? {
            Resolved::F64(f) => resolved_to_json(Resolved::F64(f)),
            Resolved::Int(i) => resolved_to_json(Resolved::F64(i as f64)),
            other => Err(format!("cannot decode {other:?} as a !!float")),
        },
        // An explicit `!!timestamp` is a string too, for the same
        // backward-compatibility reason as the plain case.
        "timestamp" => Ok(Value::String(text.to_string())),
        _ => Ok(Value::String(text.to_string())),
    }
}

fn go_mapping(map: &Mapping<'_>) -> Result<Value, String> {
    let mut out = Map::new();
    let mut spelled: HashMap<String, (KeyId, String)> = HashMap::new();
    for (key, value) in map.iter() {
        if is_merge_key(key) {
            for source in merge_sources(value)? {
                for (skey, svalue) in source.iter() {
                    insert_go(&mut out, &mut spelled, skey, svalue)?;
                }
            }
            continue;
        }
        insert_go(&mut out, &mut spelled, key, value)?;
    }
    Ok(Value::Object(out))
}

/// Insert one key into the JSON object, refusing the two ways it can collide
/// with a key already there.
///
/// The map is keyed by the *JSON spelling* (so a second key that spells the
/// same JSON key is seen) and each entry remembers the resolved key that
/// produced it, which answers both questions the Go side asks:
///
///   - go-yaml's strict duplicate detection keys on the *resolved* key (type
///     and value), not on its JSON spelling: `true:` and `yes:` are the same
///     resolved key (fixture 09) and are refused as "already set".
///   - `utils.ToJSON` refuses a document whose two *different* keys of one
///     mapping spell the same JSON key (`1:` and `"1":`, fixture 44): which of
///     the two values survives is decided by Go's map iteration order, so
///     there is no defined answer for a port to be faithful to.
fn insert_go(
    out: &mut Map<String, Value>,
    spelled: &mut HashMap<String, (KeyId, String)>,
    key: &Yaml<'_>,
    value: &Yaml<'_>,
) -> Result<(), String> {
    let resolved = resolve_key(key)?;
    let id = key_id_of(&resolved);
    let json_key = key_string_of(&resolved)?;
    let label = yaml_key_spelling(&resolved);
    if let Some((previous, previous_label)) = spelled.get(&json_key) {
        if *previous == id {
            return Err(format!("key {json_key} already set in map"));
        }
        return Err(format!(
            "YAML keys {previous_label} and {label} convert to the same JSON key {json_key:?}"
        ));
    }
    spelled.insert(json_key.clone(), (id, label));
    out.insert(json_key, go_value(value)?);
    Ok(())
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
        Resolved::Str(s) => format!("!!str {s:?}"),
    }
}

/// YAML's own spellings for the special floats, everything else in Rust's
/// shortest round-trip form (i.e. full precision, the opposite of the float32
/// truncation `key_string_of` applies). The point of the message is that the
/// value the document wrote is not the value its JSON key says.
fn yaml_float_spelling(value: f64) -> String {
    if value.is_nan() {
        ".nan".into()
    } else if value == f64::INFINITY {
        ".inf".into()
    } else if value == f64::NEG_INFINITY {
        "-.inf".into()
    } else {
        format!("{value}")
    }
}

/// `isMerge`: a plain, untagged `<<`; anything else is a normal key.
fn is_merge_key(key: &Yaml<'_>) -> bool {
    matches!(key, Yaml::Representation(text, ScalarStyle::Plain, None) if text.as_ref() == "<<")
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

/// The key as go-yaml resolved it; `convertToJSONableObject` then decides how
/// (and whether) it can become a JSON object key.
fn resolve_key(key: &Yaml<'_>) -> Result<Resolved, String> {
    Ok(match key {
        Yaml::Representation(text, style, tag) => match tag {
            // Unknown tags keep the raw text; core-schema tags re-resolve.
            Some(tag) if tag.is_yaml_core_schema() && tag.suffix != "str" => resolve_plain(text)?,
            Some(_) => Resolved::Str(text.to_string()),
            None if *style == ScalarStyle::Plain => resolve_plain(text)?,
            None => Resolved::Str(text.to_string()),
        },
        Yaml::Value(scalar) => match scalar {
            Scalar::Null => Resolved::Null,
            Scalar::Boolean(b) => Resolved::Bool(*b),
            Scalar::Integer(i) => Resolved::Int(*i),
            Scalar::FloatingPoint(f) => Resolved::F64(f.0),
            Scalar::String(s) => Resolved::Str(s.to_string()),
        },
        // Complex keys are not representable in JSON.
        _ => return Err("unsupported map key of type: complex".into()),
    })
}

/// `convertToJSONableObject`'s key spelling, quirks included: floats go
/// through `strconv.FormatFloat(f, 'g', -1, 32)` (32-bit!), and any other
/// type is a hard error.
///
/// The one error text in here is core's own sentence, not the library's: a nil
/// key is a key no JSON key exists for, and core names it (`UnusableKeys`)
/// where the library it replaced printed the type with `%s` and produced
/// `unsupported map key of type: %!s(<nil>)`. A caller that reads the Go
/// side's refusal should read the same sentence here.
fn key_string_of(resolved: &Resolved) -> Result<String, String> {
    Ok(match resolved {
        Resolved::Str(s) => s.clone(),
        Resolved::Int(i) => i.to_string(),
        Resolved::UInt(u) => u.to_string(),
        Resolved::Bool(b) => b.to_string(),
        // go-yaml's `.inf`/`.nan` spellings are kept for float keys.
        Resolved::F64(f) => {
            if f.is_nan() {
                ".nan".into()
            } else if *f == f64::INFINITY {
                ".inf".into()
            } else if *f == f64::NEG_INFINITY {
                "-.inf".into()
            } else {
                format!("{}", *f as f32)
            }
        }
        Resolved::Null => {
            return Err("YAML key !!null null cannot be spelled as a JSON key".into());
        }
    })
}

#[derive(PartialEq, Eq, Hash)]
enum KeyId {
    Null,
    Bool(bool),
    Int(i64),
    UInt(u64),
    Float(u64),
    Str(String),
}

fn key_id_of(resolved: &Resolved) -> KeyId {
    match resolved {
        Resolved::Null => KeyId::Null,
        Resolved::Bool(b) => KeyId::Bool(*b),
        Resolved::Int(i) => KeyId::Int(*i),
        Resolved::UInt(u) => KeyId::UInt(*u),
        Resolved::F64(f) => KeyId::Float(f.to_bits()),
        Resolved::Str(s) => KeyId::Str(s.clone()),
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
                return Err(format!("found incompatible YAML document: {version}"));
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
    use saphyr_parser::Event;

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
        let event = event.map_err(|e| e.to_string())?.0;
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
        return Err("document contains excessive aliasing".into());
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
