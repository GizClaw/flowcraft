//! The two serde-based forks of the archived `serde_yaml`, used exactly the
//! way production would use them: `from_slice` straight into a JSON value.

use crate::{gojson, is_json_prefix, json_passthrough};
use serde_json::Value;

pub fn norway(data: &[u8]) -> Result<String, String> {
    if is_json_prefix(data) {
        return json_passthrough(data);
    }
    let value: Value = serde_norway::from_slice(data).map_err(|e| e.to_string())?;
    Ok(gojson::to_string(&value))
}

pub fn yaml_ng(data: &[u8]) -> Result<String, String> {
    if is_json_prefix(data) {
        return json_passthrough(data);
    }
    let value: Value = serde_yaml_ng::from_slice(data).map_err(|e| e.to_string())?;
    Ok(gojson::to_string(&value))
}
