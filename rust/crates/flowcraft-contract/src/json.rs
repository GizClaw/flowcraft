//! Go-compatible JSON text output.
//!
//! `encoding/json`'s encoder is the reference: compact output, sorted map
//! keys, HTML-escaped `<`, `>`, `&`, U+2028/U+2029 escaped, and a float
//! rule of its own — `'f'` formatting unless the magnitude is `>= 1e21` or
//! `< 1e-6`, in which case `'e'` with the exponent's leading zero stripped
//! (`1e-07` -> `1e-7`). Reproducing that byte for byte is what makes
//! `utils.ToJSON` output diffable against the Go oracle.

use serde_json::Value;

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
fn write_string(s: &str, out: &mut String) {
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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

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
