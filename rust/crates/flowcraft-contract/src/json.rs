//! Go-compatible JSON text: the output half, and the raw object reader the
//! wire decoders need.
//!
//! `encoding/json`'s encoder is the reference: compact output, sorted map
//! keys, HTML-escaped `<`, `>`, `&`, U+2028/U+2029 escaped, and a float
//! rule of its own — `'f'` formatting unless the magnitude is `>= 1e21` or
//! `< 1e-6`, in which case `'e'` with the exponent's leading zero stripped
//! (`1e-07` -> `1e-7`). Reproducing that byte for byte is what makes
//! `utils.ToJSON` output diffable against the Go oracle.
//!
//! The reader half ([`Members`]) exists because Go's `json.RawMessage` keeps a
//! sub-document's *text*: a tool call's arguments come back out exactly as they
//! went in (compacted, but with their own key order and number spelling), and
//! re-rendering them from a parsed `Value` would sort the keys and rewrite
//! `1.0` as `1`. So a decoder here parses an object once into its members,
//! each with the raw text of its value, and re-parses only the fields it has
//! to understand.

use std::fmt;
use std::marker::PhantomData;

use serde::Deserialize;
use serde::de::{Deserializer, MapAccess, Visitor};
use serde_json::Value;
use serde_json::value::RawValue;

/// The Go-compatible rendering of a value, which is what `utils.ToJSON` hands
/// back for the documents the port converts.
pub fn to_string(value: &Value) -> String {
    let mut out = String::new();
    write_value(value, &mut out);
    out
}

fn write_value(value: &Value, out: &mut String) {
    match value {
        Value::Null => out.push_str("null"),
        Value::Bool(true) => out.push_str("true"),
        Value::Bool(false) => out.push_str("false"),
        Value::Number(n) => out.push_str(&number(n)),
        Value::String(s) => write_string(s, out),
        Value::Array(items) => {
            out.push('[');
            for (i, item) in items.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_value(item, out);
            }
            out.push(']');
        }
        Value::Object(map) => {
            out.push('{');
            for (i, (key, value)) in map.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_string(key, out);
                out.push(':');
                write_value(value, out);
            }
            out.push('}');
        }
    }
}

fn number(n: &serde_json::Number) -> String {
    if let Some(i) = n.as_i64() {
        return i.to_string();
    }
    if let Some(u) = n.as_u64() {
        return u.to_string();
    }
    f64_go(n.as_f64().unwrap_or(f64::NAN))
}

/// `strconv.AppendFloat(f, fmt, -1, 64)` for the format Go picks.
pub fn f64_go(f: f64) -> String {
    if f == 0.0 {
        return if f.is_sign_negative() {
            "-0".into()
        } else {
            "0".into()
        };
    }
    let abs = f.abs();
    if !(1e-6..1e21).contains(&abs) {
        return exp_form(f);
    }
    // Rust's Display is the shortest round-tripping decimal, in plain
    // notation — which is exactly Go's `'f'` with precision -1. Note the
    // difference from `{:.0}`: Go prints `9223372036854775807 as f64` as
    // `9223372036854776000`, not as its exact integral value.
    format!("{f}")
}

/// Go's `'e'` form: `1e+21`, `1.5e-7` (leading zero stripped by the encoder).
fn exp_form(f: f64) -> String {
    let s = format!("{f:e}"); // shortest mantissa, exponent without sign padding
    // `{:e}` always writes `<mantissa>e<exponent>`, so neither fallback below
    // is reachable -- they are here because this crate forbids panicking.
    let Some((mantissa, exponent)) = s.split_once('e') else {
        return s;
    };
    let Ok(exponent) = exponent.parse::<i32>() else {
        return s;
    };
    let sign = if exponent < 0 { "-" } else { "+" };
    format!("{mantissa}e{sign}{}", exponent.abs())
}

/// `encoding/json`'s string escaping, including the default HTML escaping.
/// Shared with the wire codecs, which have to write the same bytes core does
/// for fields core writes as struct fields rather than as map entries.
pub fn write_string(s: &str, out: &mut String) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c if (c as u32) < 0x20 => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// Go's `appendCompact` with the default HTML escaping: whitespace outside
/// strings dropped, `<`, `>` and `&` rewritten as `\u00XX`, and the raw UTF-8
/// of U+2028/U+2029 rewritten as its escape. Nothing is reordered or re-spelled
/// -- numbers, key order and existing escapes are exactly as they came in,
/// which is what `encoding/json` does to a `json.RawMessage` on the way out.
///
/// The input is expected to be valid JSON; a caller that cannot promise that
/// checks first with [`is_valid`] (which stays crate-internal: callers above
/// this crate write their own refusal).
pub fn compact(raw: &str) -> String {
    let bytes = raw.as_bytes();
    let mut out: Vec<u8> = Vec::with_capacity(bytes.len());
    let mut in_string = false;
    let mut escaped = false;
    let mut i = 0;
    while i < bytes.len() {
        let byte = bytes[i];
        if in_string {
            if escaped {
                escaped = false;
            } else if byte == b'\\' {
                escaped = true;
            } else if byte == b'"' {
                in_string = false;
            }
        } else {
            match byte {
                b' ' | b'\t' | b'\n' | b'\r' => {
                    i += 1;
                    continue;
                }
                b'"' => in_string = true,
                _ => {}
            }
        }
        if matches!(byte, b'<' | b'>' | b'&') {
            out.extend_from_slice(format!("\\u{byte:04x}").as_bytes());
            i += 1;
            continue;
        }
        // U+2028 and U+2029 are E2 80 A8 and E2 80 A9.
        if byte == 0xE2 && i + 2 < bytes.len() && bytes[i + 1] == 0x80 && bytes[i + 2] & !1 == 0xA8
        {
            out.extend_from_slice(if bytes[i + 2] == 0xA8 {
                b"\\u2028".as_slice()
            } else {
                b"\\u2029".as_slice()
            });
            i += 3;
            continue;
        }
        out.push(byte);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// Whether `raw` is one well-formed JSON value with nothing after it.
pub(crate) fn is_valid(raw: &str) -> bool {
    serde_json::from_str::<Value>(raw).is_ok()
}

/// One JSON array's elements, each with its raw text -- the counterpart of
/// [`Members`] for the arrays the wire carries (`content.parts`).
pub(crate) fn raw_array(raw: &str) -> Result<Vec<&RawValue>, String> {
    serde_json::from_str::<Vec<&RawValue>>(raw).map_err(|err| err.to_string())
}

/// One JSON object's members, in document order, each with the raw text of its
/// value.
///
/// Go hands a sub-document's bytes to a custom unmarshaller untouched, and its
/// strict struct decode names the *first* unknown field in document order. Both
/// need the text and the order, which is why this is not a `serde_json::Map`.
pub(crate) struct Members<'a> {
    entries: Vec<(String, &'a RawValue)>,
}

impl<'a> Members<'a> {
    /// Parses one object. A document that is not exactly one object is an
    /// error in the caller's own words -- core's text for it comes from
    /// `encoding/json` and is not reproduced here.
    pub(crate) fn parse(document: &'a str) -> Result<Members<'a>, String> {
        serde_json::from_str::<Members<'a>>(document).map_err(|err| err.to_string())
    }

    /// The raw text of a member, last occurrence first: a repeated key keeps
    /// the last value, as Go's decoder does.
    pub(crate) fn raw(&self, key: &str) -> Option<&'a str> {
        self.entries
            .iter()
            .rev()
            .find(|(name, _)| name == key)
            .map(|(_, value)| value.get())
    }

    /// The first member, in document order, whose name is not in `allowed` --
    /// what `DisallowUnknownFields` refuses.
    pub(crate) fn first_unknown(&self, allowed: &[&str]) -> Option<&str> {
        self.entries
            .iter()
            .map(|(name, _)| name.as_str())
            .find(|name| !allowed.contains(name))
    }

    /// A string member with Go's rules: absent and `null` are the empty
    /// string, anything else that is not a string is refused.
    pub(crate) fn string(&self, key: &str) -> Result<String, String> {
        let Some(raw) = self.raw(key) else {
            return Ok(String::new());
        };
        match serde_json::from_str::<Value>(raw) {
            Ok(Value::String(value)) => Ok(value),
            Ok(Value::Null) => Ok(String::new()),
            _ => Err(format!("field {key} must be a string, not {raw}")),
        }
    }

    /// An integer member: absent and `null` are `None`, a number without a
    /// fraction is `Some`, and everything else -- a fraction included, which
    /// Go refuses as well -- is an error.
    pub(crate) fn i64(&self, key: &str) -> Result<Option<i64>, String> {
        let Some(raw) = self.raw(key) else {
            return Ok(None);
        };
        match serde_json::from_str::<Value>(raw) {
            Ok(Value::Null) => Ok(None),
            Ok(Value::Number(number)) => number
                .as_i64()
                .map(Some)
                .ok_or_else(|| format!("field {key} must be an integer, not {raw}")),
            _ => Err(format!("field {key} must be an integer, not {raw}")),
        }
    }

    /// A boolean member: absent and `null` are false.
    pub(crate) fn bool(&self, key: &str) -> Result<bool, String> {
        let Some(raw) = self.raw(key) else {
            return Ok(false);
        };
        match serde_json::from_str::<Value>(raw) {
            Ok(Value::Bool(value)) => Ok(value),
            Ok(Value::Null) => Ok(false),
            _ => Err(format!("field {key} must be a boolean, not {raw}")),
        }
    }
}

impl<'de: 'a, 'a> Deserialize<'de> for Members<'a> {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        struct MembersVisitor<'a>(PhantomData<&'a ()>);

        impl<'de: 'a, 'a> Visitor<'de> for MembersVisitor<'a> {
            type Value = Members<'a>;

            fn expecting(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                formatter.write_str("a JSON object")
            }

            fn visit_map<A>(self, mut map: A) -> Result<Members<'a>, A::Error>
            where
                A: MapAccess<'de>,
            {
                let mut entries = Vec::new();
                while let Some((name, value)) = map.next_entry::<String, &'a RawValue>()? {
                    entries.push((name, value));
                }
                Ok(Members { entries })
            }
        }

        deserializer.deserialize_map(MembersVisitor(PhantomData))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// Unwraps a rule the test expects to hold; a refusal fails the test with
    /// its own sentence instead of a backtrace line.
    fn ok<T>(result: Result<T, String>, what: &str) -> T {
        match result {
            Ok(value) => value,
            Err(err) => panic!("{what}: {err}"),
        }
    }

    /// Every expectation here was copied from `encoding/json` itself, one per
    /// rule the module doc names. The corpus reaches only some of these forms,
    /// and the float rule in particular is easy to get almost right.
    #[test]
    fn floats_are_go_formatted() {
        assert_eq!(f64_go(1.0), "1");
        assert_eq!(f64_go(1.5), "1.5");
        assert_eq!(f64_go(0.1), "0.1");
        assert_eq!(f64_go(-2.5), "-2.5");
        assert_eq!(f64_go(1e-6), "0.000001");
        assert_eq!(f64_go(1e-7), "1e-7");
        assert_eq!(f64_go(1e20), "100000000000000000000");
        assert_eq!(f64_go(1e21), "1e+21");
        assert_eq!(f64_go(1.5e21), "1.5e+21");
        assert_eq!(f64_go(1.2345678901234568e29), "1.2345678901234568e+29");
        assert_eq!(f64_go(123456789.5), "123456789.5");
        // The shortest form is not the exact value: this is a float64, and Go
        // writes the decimal that round-trips, which is not the integer it
        // started as.
        assert_eq!(f64_go(9223372036854775807_f64), "9223372036854776000");
        assert_eq!(f64_go(-0.0), "-0");
    }

    #[test]
    fn strings_are_go_escaped() {
        assert_eq!(to_string(&json!("<a>&")), r#""\u003ca\u003e\u0026""#);
        assert_eq!(to_string(&json!("\u{2028}\u{2029}")), r#""\u2028\u2029""#);
        // DEL is not escaped; the C0 range is, in lowercase hex.
        assert_eq!(
            to_string(&json!("\u{1}\u{1f}\u{7f}")),
            "\"\\u0001\\u001f\u{7f}\""
        );
        assert_eq!(to_string(&json!("\"\\")), r#""\"\\""#);
        assert_eq!(to_string(&json!("日本")), "\"日本\"");
        assert_eq!(to_string(&json!("\n\r\t")), r#""\n\r\t""#);
    }

    #[test]
    fn object_keys_come_out_sorted() {
        // serde_json's map is a BTreeMap here (no `preserve_order`), which is
        // the order `encoding/json` writes a Go map in: byte order, uppercase
        // before lowercase.
        assert_eq!(
            to_string(&json!({"b": 1, "a": 2, "B": 3, "1": 4})),
            r#"{"1":4,"B":3,"a":2,"b":1}"#
        );
    }

    /// What `json.RawMessage` survives on the way out: whitespace gone, text
    /// untouched. Key order, number spelling and existing escapes are Go's
    /// reasons for compacting rather than re-marshalling, so they are what this
    /// pins.
    #[test]
    fn compact_removes_only_whitespace_and_escapes() {
        assert_eq!(
            compact("{ \"b\" : 1 , \"a\" : [ 2 ] }"),
            r#"{"b":1,"a":[2]}"#
        );
        // Inside a string the whitespace is text, and stays.
        assert_eq!(compact(r#"{ "a" : " x " }"#), r#"{"a":" x "}"#);
        // Number spelling and key order are the caller's, not the encoder's.
        assert_eq!(compact(r#"{"z":1.0,"a":1e100}"#), r#"{"z":1.0,"a":1e100}"#);
        // HTML escaping applies inside strings, as it does in Go.
        assert_eq!(compact(r#"{"a":"<b>&"}"#), r#"{"a":"\u003cb\u003e\u0026"}"#);
        // U+2028/2029 are escaped from their UTF-8 bytes, not their escape.
        assert_eq!(
            compact("{\"a\":\"\u{2028}\u{2029}\"}"),
            r#"{"a":"\u2028\u2029"}"#
        );
        assert_eq!(compact(r#"{"a":"\u2028"}"#), r#"{"a":"\u2028"}"#);
        // An escaped quote does not end the string, so the space after it is
        // still inside one.
        assert_eq!(compact(r#"{ "a" : "q\" x" }"#), r#"{"a":"q\" x"}"#);
    }

    /// An object reader that keeps order and text: `encoding/json` hands a
    /// custom unmarshaller the raw bytes, and names the first unknown field in
    /// document order.
    #[test]
    fn members_keep_document_order_and_the_last_duplicate() {
        let members = ok(
            Members::parse(r#"{"a": 1, "b": "x", "c": {"d": 2}, "a": 3}"#),
            "the document is an object",
        );
        assert_eq!(members.raw("a"), Some("3"));
        assert_eq!(members.raw("b"), Some(r#""x""#));
        assert_eq!(members.raw("c"), Some(r#"{"d": 2}"#));
        assert_eq!(members.raw("zzz"), None);
        assert_eq!(ok(members.string("b"), "a string"), "x");
        assert_eq!(ok(members.i64("a"), "an integer"), Some(3));
        assert_eq!(members.first_unknown(&["a", "b"]), Some("c"));
        assert_eq!(members.first_unknown(&["a", "b", "c"]), None);
        // Absent and null read as the zero value; a wrong shape is refused.
        assert_eq!(ok(members.string("zzz"), "absent"), "");
        assert!(!ok(members.bool("zzz"), "absent"));
        assert!(members.string("c").is_err());
        assert!(Members::parse("[1]").is_err());
    }

    /// The value walk, not just the leaves: nesting, empty containers and the
    /// three scalar shapes `encoding/json` writes without quotes.
    #[test]
    fn structures_are_go_shaped() {
        assert_eq!(
            to_string(&json!({"a": [null, true, false], "b": {}, "c": []})),
            r#"{"a":[null,true,false],"b":{},"c":[]}"#
        );
    }
}
