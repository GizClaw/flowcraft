//! Building a tool's input schema: the property helpers and the fluent
//! [`SchemaBuilder`] core exposes, with the same JSON on the other side.
//!
//! The output is Go's: `encoding/json` sorts a map's keys, so a schema comes
//! out with `additionalProperties`, `properties`, `required` and `type` in that
//! order whatever order the caller added them in, and every nested property map
//! is sorted the same way. [`crate::json::to_string`] writes in that order,
//! which is why the builder can keep its fields in a plain object.

use serde_json::{Map, Value};

use crate::json;
use crate::message::tool::ToolDefinition;

/// One property of a tool's input schema.
#[derive(Debug, Clone, PartialEq)]
pub struct ToolPropertyDef {
    name: String,
    schema: Map<String, Value>,
}

impl ToolPropertyDef {
    /// The property's name, as the argument object spells it.
    pub fn name(&self) -> &str {
        &self.name
    }

    /// The property's schema.
    pub fn schema(&self) -> &Map<String, Value> {
        &self.schema
    }
}

/// A simple typed property.
pub fn tool_property(name: &str, typ: &str, description: &str) -> ToolPropertyDef {
    let mut schema = Map::new();
    schema.insert("type".into(), Value::String(typ.into()));
    schema.insert("description".into(), Value::String(description.into()));
    ToolPropertyDef {
        name: name.to_owned(),
        schema,
    }
}

/// The item schema of an array property: the common "array of plain-typed
/// values" case without a raw map.
pub fn items(typ: &str) -> Value {
    let mut schema = Map::new();
    schema.insert("type".into(), Value::String(typ.into()));
    Value::Object(schema)
}

/// An array property with an item schema.
pub fn tool_array_property(name: &str, description: &str, items: Value) -> ToolPropertyDef {
    let mut schema = Map::new();
    schema.insert("type".into(), Value::String("array".into()));
    schema.insert("description".into(), Value::String(description.into()));
    schema.insert("items".into(), items);
    ToolPropertyDef {
        name: name.to_owned(),
        schema,
    }
}

/// An object property with nested properties. An empty map is left out, as
/// core leaves it out.
pub fn tool_object_property(
    name: &str,
    description: &str,
    properties: &Map<String, Value>,
) -> ToolPropertyDef {
    let mut schema = Map::new();
    schema.insert("type".into(), Value::String("object".into()));
    schema.insert("description".into(), Value::String(description.into()));
    if !properties.is_empty() {
        schema.insert("properties".into(), Value::Object(properties.clone()));
    }
    ToolPropertyDef {
        name: name.to_owned(),
        schema,
    }
}

/// An object property whose values must be strings.
pub fn tool_string_map_property(name: &str, description: &str) -> ToolPropertyDef {
    let mut schema = Map::new();
    schema.insert("type".into(), Value::String("object".into()));
    schema.insert("description".into(), Value::String(description.into()));
    schema.insert("additionalProperties".into(), items("string"));
    ToolPropertyDef {
        name: name.to_owned(),
        schema,
    }
}

/// A typed property with a default value.
pub fn tool_property_with_default(
    name: &str,
    typ: &str,
    description: &str,
    default: Value,
) -> ToolPropertyDef {
    let mut property = tool_property(name, typ, description);
    property.schema.insert("default".into(), default);
    property
}

/// A property restricted to a set of string values.
pub fn tool_enum_property(
    name: &str,
    typ: &str,
    description: &str,
    values: &[&str],
) -> ToolPropertyDef {
    let mut property = tool_property(name, typ, description);
    let values = values
        .iter()
        .map(|value| Value::String((*value).to_owned()))
        .collect();
    property.schema.insert("enum".into(), Value::Array(values));
    property
}

/// A tool definition under construction.
#[derive(Debug, Clone)]
pub struct SchemaBuilder {
    name: String,
    description: String,
    properties: Map<String, Value>,
    required: Vec<String>,
    closed: bool,
}

/// Starts building a tool definition with the given properties.
pub fn define_schema(
    name: &str,
    description: &str,
    properties: Vec<ToolPropertyDef>,
) -> SchemaBuilder {
    let mut map = Map::new();
    for property in properties {
        // A repeated name keeps the last schema, as core's map does.
        map.insert(property.name, Value::Object(property.schema));
    }
    SchemaBuilder {
        name: name.to_owned(),
        description: description.to_owned(),
        properties: map,
        required: Vec::new(),
        closed: false,
    }
}

impl SchemaBuilder {
    /// Marks the object schema closed: providers that honor
    /// `additionalProperties` will reject arguments outside the declared
    /// properties.
    pub fn disallow_additional_properties(mut self) -> Self {
        self.closed = true;
        self
    }

    /// Marks property names as required. Duplicates are silently ignored,
    /// including ones already marked.
    pub fn required(mut self, names: &[&str]) -> Self {
        for name in names {
            if !self.required.iter().any(|known| known == name) {
                self.required.push((*name).to_owned());
            }
        }
        self
    }

    /// The final definition.
    pub fn build(self) -> ToolDefinition {
        let mut schema = Map::new();
        schema.insert("type".into(), Value::String("object".into()));
        schema.insert("properties".into(), Value::Object(self.properties));
        if !self.required.is_empty() {
            let required = self
                .required
                .into_iter()
                .map(Value::String)
                .collect::<Vec<_>>();
            schema.insert("required".into(), Value::Array(required));
        }
        if self.closed {
            schema.insert("additionalProperties".into(), Value::Bool(false));
        }
        ToolDefinition {
            name: self.name,
            description: self.description,
            input_schema: json::to_string(&Value::Object(schema)),
        }
    }
}
