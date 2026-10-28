//! The tool-source contract.

use std::sync::Arc;

use crate::error::Result;
use crate::proto::message as msg;
use crate::resource::Resource;
use async_trait::async_trait;

/// The registry kind.
pub const KIND: &str = "tool.Source";
/// The cross-process contract identity.
pub const CONTRACT: &str = "tool.ToolSource/v1";

/// A named set of tools: list what is available, run one call.
#[async_trait]
pub trait ToolSource: Send + Sync {
    /// Every tool this source exposes.
    ///
    /// Order is not significant: the endpoint sorts by name before a peer sees
    /// the catalog, so a source does not have to promise an order.
    async fn list(&self) -> Result<Vec<msg::ToolDefinition>>;

    /// Run `name` with `arguments` (a JSON object, empty for no arguments).
    async fn execute(&self, name: &str, arguments: &str) -> Result<msg::Content>;
}

/// The value a factory of this capability produces.
#[derive(Clone)]
pub struct Handle(pub Arc<dyn ToolSource>);

impl Handle {
    /// Wrap a provider.
    #[must_use]
    pub fn new(source: Arc<dyn ToolSource>) -> Self {
        Self(source)
    }

    /// The provider.
    #[must_use]
    pub fn source(&self) -> Arc<dyn ToolSource> {
        self.0.clone()
    }
}

impl std::fmt::Debug for Handle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("tool.Handle")
    }
}

impl std::ops::Deref for Handle {
    type Target = Arc<dyn ToolSource>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl Resource for Handle {}
