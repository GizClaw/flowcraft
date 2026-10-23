//! The in-process tool contract: what the model can call, and how it runs.
//!
//! Ported from `core/tool/tool.go`. This is the agent-side `Tool`, distinct from
//! the cross-process [`ToolSource`](crate::contract::ToolSource): a `Tool` lives
//! in the registry, a `ToolSource` crosses a process boundary.

use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::Result;
use flowcraft_proto::message as msg;

use crate::session::Session;

/// Per-call context: which run the call belongs to. This is the Rust
/// counterpart of Go's execution context — explicit instead of ambient, so a
/// tool never reaches into global state to find its session.
#[derive(Clone, Default)]
pub struct CallContext {
    /// The per-run session, when the caller is an agent with dynamic injection.
    pub session: Option<Arc<dyn Session>>,
}

/// Advisory, execution-relevant metadata a tool may declare.
///
/// A zero value means "no claims": no rate limit, and callers should assume the
/// tool may mutate state (retries are then unsafe).
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct ToolMeta {
    /// Maximum executions per second this tool can sustain. Zero means no
    /// claim; a negative value is treated as zero.
    pub rate_limit: f64,
    /// The tool has side effects beyond returning a result.
    pub mutates_state: bool,
    /// The tool bounds its own execution time, so a timeout middleware should
    /// not impose its default deadline on top.
    pub self_timeout: bool,
}

/// An LLM-callable tool.
#[async_trait]
pub trait Tool: Send + Sync {
    /// What the model sees.
    fn definition(&self) -> msg::ToolDefinition;

    /// Run one call. `arguments` is the JSON document the model produced.
    async fn execute(&self, context: &CallContext, arguments: &str) -> Result<msg::Content>;

    /// Execution metadata. The default is a conservative zero value.
    fn metadata(&self) -> ToolMeta {
        ToolMeta::default()
    }

    /// Release whatever this tool holds. A deferred proxy forwards to the
    /// loaded tool; concrete tools default to a no-op.
    fn close(&self) -> Result<()> {
        Ok(())
    }

    /// Force a deferred load. A no-op for concrete tools; the registry calls it
    /// before a discovered tool's definition reaches the model.
    async fn ensure_loaded(&self) -> Result<()> {
        Ok(())
    }
}

/// A tool implementation as a plain function.
pub type Handler = Arc<dyn Fn(&CallContext, &str) -> Result<msg::Content> + Send + Sync>;

/// A [`Tool`] backed by a function, for builtins and tests.
pub struct FuncTool {
    definition: msg::ToolDefinition,
    handler: Handler,
    metadata: ToolMeta,
}

impl std::fmt::Debug for FuncTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FuncTool")
            .field("name", &self.definition.name)
            .finish_non_exhaustive()
    }
}

impl FuncTool {
    /// Build a tool from a definition and a handler.
    #[must_use]
    pub fn new(definition: msg::ToolDefinition, handler: Handler) -> Self {
        Self {
            definition,
            handler,
            metadata: ToolMeta::default(),
        }
    }

    /// Declare execution metadata.
    #[must_use]
    pub fn with_metadata(mut self, metadata: ToolMeta) -> Self {
        self.metadata = metadata;
        self
    }
}

#[async_trait]
impl Tool for FuncTool {
    fn definition(&self) -> msg::ToolDefinition {
        self.definition.clone()
    }

    async fn execute(&self, context: &CallContext, arguments: &str) -> Result<msg::Content> {
        (self.handler)(context, arguments)
    }

    fn metadata(&self) -> ToolMeta {
        self.metadata
    }
}

/// Wrap a string-returning function as a text-only tool.
#[must_use]
pub fn text_tool<F>(definition: msg::ToolDefinition, handler: F) -> FuncTool
where
    F: Fn(&str) -> Result<String> + Send + Sync + 'static,
{
    FuncTool::new(
        definition,
        Arc::new(move |_context: &CallContext, arguments: &str| {
            Ok(crate::local::text_result((handler)(arguments)?))
        }),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use flowcraft_error::ErrorKind;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    #[tokio::test]
    async fn a_text_tool_wraps_its_result() {
        let tool = text_tool(definition("echo"), |args| Ok(args.to_owned()));
        let content = tool
            .execute(&CallContext::default(), "hi")
            .await
            .expect("run");
        assert_eq!(content, crate::local::text_result("hi"));
    }

    #[tokio::test]
    async fn metadata_defaults_to_conservative() {
        let tool = text_tool(definition("echo"), |_| Ok(String::new()));
        let meta = tool.metadata();
        assert_eq!(meta.rate_limit, 0.0);
        assert!(!meta.mutates_state);
        assert!(!meta.self_timeout);
    }

    #[tokio::test]
    async fn a_failing_handler_surfaces_the_error() {
        let tool = text_tool(definition("boom"), |_| {
            Err(flowcraft_error::Error::validation("bad"))
        });
        assert_eq!(
            tool.execute(&CallContext::default(), "{}")
                .await
                .unwrap_err()
                .kind(),
            ErrorKind::Validation
        );
    }
}
