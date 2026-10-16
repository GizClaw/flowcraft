//! The two serde-based forks of the archived `serde_yaml`, used exactly the
//! way production would use them: `from_slice` straight into a JSON value.
//!
//! Only YAML documents reach them: the probe answers a JSON document with
//! core's own JSON branch, which is the same answer for every reading.

use flowcraft_contract::json;
use serde_json::Value;

pub fn norway(data: &[u8]) -> Result<String, String> {
    let value: Value = serde_norway::from_slice(data).map_err(|e| e.to_string())?;
    Ok(json::to_string(&value))
}

pub fn yaml_ng(data: &[u8]) -> Result<String, String> {
    let value: Value = serde_yaml_ng::from_slice(data).map_err(|e| e.to_string())?;
    Ok(json::to_string(&value))
}
