//! The in-process provider: a registry of tool implementations.

use std::collections::BTreeMap;
use std::sync::Arc;

use crate::error::Result;
use crate::proto::message as msg;
use async_trait::async_trait;

use crate::caps::tool::contract::ToolSource;
use crate::message::{validate_arguments, validate_content, validate_definition};

/// A tool's implementation. It receives the arguments string as sent and
/// returns the content the model will see.
pub type Handler = Arc<dyn Fn(&str) -> Result<msg::Content> + Send + Sync>;

/// A tool: what the model sees, and what runs.
#[derive(Clone)]
pub struct Tool {
    definition: msg::ToolDefinition,
    handler: Handler,
}

impl std::fmt::Debug for Tool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Tool")
            .field("name", &self.definition.name)
            .finish_non_exhaustive()
    }
}

impl Tool {
    /// Build a tool, validating its definition.
    pub fn new(definition: msg::ToolDefinition, handler: Handler) -> Result<Self> {
        validate_definition(&definition)?;
        Ok(Self {
            definition,
            handler,
        })
    }

    /// What the model sees.
    #[must_use]
    pub fn definition(&self) -> &msg::ToolDefinition {
        &self.definition
    }

    /// Run the handler and check what it produced.
    pub fn execute(&self, arguments: &str) -> Result<msg::Content> {
        validate_arguments(arguments)?;
        let content = (self.handler)(arguments)?;
        validate_content(&content)?;
        Ok(content)
    }
}

/// An in-process set of tools, built once and then shared.
#[derive(Debug, Clone, Default)]
pub struct LocalSource {
    tools: BTreeMap<String, Tool>,
}

impl LocalSource {
    /// An empty registry.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Register a tool. A duplicate name is a conflict, not an override: two
    /// tools fighting over one name is a deployment bug.
    pub fn insert(&mut self, definition: msg::ToolDefinition, handler: Handler) -> Result<()> {
        let tool = Tool::new(definition, handler)?;
        let name = tool.definition.name.clone();
        if self.tools.contains_key(&name) {
            return Err(crate::caps::tool::codes::conflict(
                crate::caps::tool::codes::Code::Duplicate,
                format!("tool {name:?} is registered twice"),
            ));
        }
        self.tools.insert(name, tool);
        Ok(())
    }

    /// Builder form of [`Self::insert`].
    pub fn with(mut self, definition: msg::ToolDefinition, handler: Handler) -> Result<Self> {
        self.insert(definition, handler)?;
        Ok(self)
    }

    /// How many tools are registered.
    #[must_use]
    pub fn len(&self) -> usize {
        self.tools.len()
    }

    /// Whether the registry is empty.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.tools.is_empty()
    }
}

#[async_trait]
impl ToolSource for LocalSource {
    async fn list(&self) -> Result<Vec<msg::ToolDefinition>> {
        Ok(self
            .tools
            .values()
            .map(|tool| tool.definition.clone())
            .collect())
    }

    async fn execute(&self, name: &str, arguments: &str) -> Result<msg::Content> {
        let tool = self.tools.get(name).ok_or_else(|| {
            crate::caps::tool::codes::not_found(
                crate::caps::tool::codes::Code::Unknown,
                format!("tool {name:?} is not registered"),
            )
        })?;
        tool.execute(arguments)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::error::ErrorKind;
    use crate::message::text_content;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: "test".to_owned(),
            input_schema: Vec::new(),
        }
    }

    fn echo() -> Handler {
        Arc::new(|arguments: &str| Ok(text_content(arguments)))
    }

    #[tokio::test]
    async fn executes_a_registered_tool() {
        let source = LocalSource::new()
            .with(definition("echo"), echo())
            .expect("insert");
        assert_eq!(source.len(), 1);
        let content = source.execute("echo", r#"{"x":1}"#).await.expect("run");
        assert_eq!(content, text_content(r#"{"x":1}"#));
    }

    #[tokio::test]
    async fn an_unknown_tool_is_not_found() {
        let source = LocalSource::new();
        let error = source.execute("missing", "{}").await.expect_err("missing");
        assert_eq!(error.kind(), ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn a_duplicate_name_is_a_conflict() {
        let mut source = LocalSource::new();
        source.insert(definition("echo"), echo()).expect("first");
        let error = source.insert(definition("echo"), echo()).expect_err("dup");
        assert_eq!(error.kind(), ErrorKind::Conflict);
    }

    #[tokio::test]
    async fn an_invalid_argument_is_refused_before_the_handler_runs() {
        let source = LocalSource::new()
            .with(definition("echo"), echo())
            .expect("insert");
        let error = source.execute("echo", "{oops").await.expect_err("bad json");
        assert_eq!(error.kind(), ErrorKind::Validation);
    }

    #[tokio::test]
    async fn arguments_must_be_a_json_object() {
        let source = LocalSource::new()
            .with(definition("echo"), echo())
            .expect("insert");
        for bad in ["[1]", "\"text\"", "1", "null"] {
            let error = source.execute("echo", bad).await.expect_err(bad);
            assert_eq!(error.kind(), ErrorKind::Validation, "{bad}");
        }
        source.execute("echo", "").await.expect("no arguments");
        source.execute("echo", r#"{"x":1}"#).await.expect("object");
    }

    #[test]
    fn an_invalid_schema_is_refused_at_registration() {
        let mut source = LocalSource::new();
        let mut bad = definition("echo");
        bad.input_schema = b"[1]".to_vec();
        let error = source.insert(bad, echo()).expect_err("schema");
        assert_eq!(error.kind(), ErrorKind::Validation);
    }

    #[tokio::test]
    async fn an_empty_result_is_refused() {
        let source = LocalSource::new()
            .with(
                definition("empty"),
                Arc::new(|_: &str| Ok(msg::Content { parts: Vec::new() })),
            )
            .expect("insert");
        let error = source.execute("empty", "").await.expect_err("empty");
        assert_eq!(error.kind(), ErrorKind::Validation);
    }
}
