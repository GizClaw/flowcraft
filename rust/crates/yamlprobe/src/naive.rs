//! `saphyr` as delivered: YAML 1.2 core-schema resolution, straight into JSON.
//!
//! This is the *measurement* reading that decided against the bare crate: it is
//! what the port would have looked like without the semantic layer that now
//! lives in `flowcraft-utils`. It is kept runnable so that comparison stays
//! reproducible (`CONFORMANCE_BACKEND=saphyr`); the port itself never goes
//! through here, and `flowcraft-utils::decode` is the code that ships.
//!
//! Only YAML documents reach this reader: the probe answers a JSON document
//! with core's own JSON branch, which is the same answer for every reading.

use flowcraft_contract::json;
use saphyr::{LoadableYamlNode, Scalar, Yaml};
use serde_json::{Map, Number, Value};

pub fn naive(data: &[u8]) -> Result<String, String> {
    let text = text_of(data)?;
    let docs = Yaml::load_from_str(text).map_err(|e| e.to_string())?;
    let value = match docs.len() {
        0 => Value::Null,
        1 => naive_value(&docs[0])?,
        _ => return Err("multiple YAML documents".into()),
    };
    Ok(json::to_string(&value))
}

fn text_of(data: &[u8]) -> Result<&str, String> {
    std::str::from_utf8(data).map_err(|e| format!("invalid UTF-8: {e}"))
}

fn naive_value(node: &Yaml<'_>) -> Result<Value, String> {
    Ok(match node {
        Yaml::Value(scalar) => match scalar {
            Scalar::Null => Value::Null,
            Scalar::Boolean(b) => Value::Bool(*b),
            Scalar::Integer(i) => Value::Number((*i).into()),
            Scalar::FloatingPoint(f) => number(f.0)?,
            Scalar::String(s) => Value::String(s.to_string()),
        },
        Yaml::Representation(s, _, _) => Value::String(s.to_string()),
        Yaml::Sequence(seq) => {
            let mut out = Vec::with_capacity(seq.len());
            for item in seq {
                out.push(naive_value(item)?);
            }
            Value::Array(out)
        }
        Yaml::Mapping(map) => {
            let mut out = Map::new();
            for (key, value) in map.iter() {
                out.insert(naive_key(key)?, naive_value(value)?);
            }
            Value::Object(out)
        }
        Yaml::Tagged(_, inner) => naive_value(inner)?,
        Yaml::Alias(_) => return Err("unresolved alias".into()),
        Yaml::BadValue => return Err("bad value".into()),
    })
}

fn naive_key(key: &Yaml<'_>) -> Result<String, String> {
    match key {
        Yaml::Value(Scalar::String(s)) => Ok(s.to_string()),
        Yaml::Value(Scalar::Integer(i)) => Ok(i.to_string()),
        Yaml::Value(Scalar::FloatingPoint(f)) => Ok(format!("{}", f.0)),
        Yaml::Value(Scalar::Boolean(b)) => Ok(b.to_string()),
        Yaml::Representation(s, _, _) => Ok(s.to_string()),
        _ => Err("mapping keys must be strings".into()),
    }
}

/// The JSON view of one number, which is where the bare crate and the port
/// already agree: both funnel through `serde_json`, and the writer spells it
/// the way `encoding/json` does.
fn number(f: f64) -> Result<Value, String> {
    Number::from_f64(f)
        .map(Value::Number)
        .ok_or_else(|| format!("unsupported value: {f}"))
}
