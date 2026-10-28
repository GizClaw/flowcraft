//! The `tool.Registry` resource factory, and the bridge between capability
//! sources and the in-process registry.
//!
//! A deployment can declare one registry over many `tool.Source` deps:
//!
//! ```yaml
//! tools:
//!   kind: tool.Registry
//!   impl: memory
//!   deps:
//!     tool.mcp: mcp-file
//!     tool.workspace: ws-tools
//! ```
//!
//! deps named `tool` or `tool.*` are merged in edge-name order, which is the
//! source order `ConflictOverwrite` uses. The output is a `tool.ToolSource`
//! handle, so it feeds `tool.Endpoint` directly.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use crate::error::{Error, Result};
use crate::proto::message as msg;
use crate::resource::{BoxFuture, BuildCtx, Kind, Phase, Spec, require_many};
use async_trait::async_trait;
use serde::Deserialize;

use crate::caps::tool::callable::{CallContext, Tool};
use crate::caps::tool::contract::{CONTRACT, Handle, ToolSource};
use crate::caps::tool::executor::{Dispatcher, Executor};
use crate::caps::tool::registry::{Catalog, ConflictPolicy, Registry, Source};

/// The registry kind.
pub const KIND: &str = "tool.Registry";
/// The implementation name.
pub const IMPL: &str = "memory";

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    conflict: ConflictSetting,
}

#[derive(Debug, Default, Clone, Copy, Deserialize)]
#[serde(rename_all = "lowercase")]
enum ConflictSetting {
    #[default]
    Error,
    Overwrite,
}

impl From<ConflictSetting> for ConflictPolicy {
    fn from(setting: ConflictSetting) -> Self {
        match setting {
            ConflictSetting::Error => Self::Error,
            ConflictSetting::Overwrite => Self::Overwrite,
        }
    }
}

fn validate(value: &serde_json::Value) -> Result<()> {
    let _: Settings = crate::resource::settings(value)?;
    Ok(())
}

/// A capability source presented as a framework [`Source`].
pub struct HandleSource {
    handle: Handle,
}

impl HandleSource {
    /// Wrap a contract handle.
    #[must_use]
    pub fn new(handle: Handle) -> Self {
        Self { handle }
    }
}

#[async_trait]
impl Source for HandleSource {
    async fn tools(&self) -> Result<Vec<Arc<dyn Tool>>> {
        let definitions = self.handle.list().await?;
        Ok(definitions
            .into_iter()
            .map(|definition| {
                Arc::new(SourceTool {
                    handle: self.handle.clone(),
                    definition,
                }) as Arc<dyn Tool>
            })
            .collect())
    }
}

struct SourceTool {
    handle: Handle,
    definition: msg::ToolDefinition,
}

#[async_trait]
impl Tool for SourceTool {
    fn definition(&self) -> msg::ToolDefinition {
        self.definition.clone()
    }

    async fn execute(&self, _context: &CallContext, arguments: &str) -> Result<msg::Content> {
        self.handle.execute(&self.definition.name, arguments).await
    }
}

/// A registry presented as a capability source, for `tool.Endpoint`'s `source`
/// dep.
pub struct RegistrySource {
    registry: Arc<Registry>,
    executor: Arc<Executor>,
    calls: AtomicU64,
}

impl RegistrySource {
    /// Wrap a registry. Calls run through a bare executor (no middleware): the
    /// registry resource is the composition node, not a policy surface.
    #[must_use]
    pub fn new(registry: Arc<Registry>) -> Self {
        let executor = Arc::new(Executor::new(registry.clone(), Vec::new()));
        Self {
            registry,
            executor,
            calls: AtomicU64::new(0),
        }
    }
}

impl std::fmt::Debug for RegistrySource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RegistrySource")
            .field("tools", &self.registry.len())
            .finish()
    }
}

#[async_trait]
impl ToolSource for RegistrySource {
    async fn list(&self) -> Result<Vec<msg::ToolDefinition>> {
        // The wire hands ownership of its own copy: the shared snapshot stays
        // shared here.
        Ok(self.registry.definitions().to_vec())
    }

    async fn execute(&self, name: &str, arguments: &str) -> Result<msg::Content> {
        let id = format!("{name}#{}", self.calls.fetch_add(1, Ordering::Relaxed));
        let result = self
            .executor
            .execute(
                CallContext::default(),
                msg::ToolCall {
                    id,
                    name: name.to_owned(),
                    arguments: arguments.as_bytes().to_vec(),
                },
            )
            .await;
        if result.is_error {
            return Err(Error::internal(error_text(&result)));
        }
        result
            .content
            .ok_or_else(|| Error::internal("tool: registry returned no content"))
    }
}

fn error_text(result: &msg::ToolResult) -> String {
    let Some(content) = &result.content else {
        return "tool: registry returned an error with no content".to_owned();
    };
    // The first text part, not the whole body: this is one line in an error.
    crate::message::first_text(content)
        .map(str::to_owned)
        .unwrap_or_else(|| "tool: registry returned an error".to_owned())
}

/// The `tool.Registry/memory` factory.
pub struct RegistryFactory;

impl crate::resource::Factory for RegistryFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        let mut sources = require_many::<Handle>("tool");
        sources.contract = Some(CONTRACT);
        Spec {
            contract: Some(CONTRACT),
            imp: Some(IMPL),
            deps: vec![sources],
            phase: Phase::Main,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let handles = ctx.deps.get_many::<Handle>("tool")?;
            let sources: Vec<Arc<dyn Source>> = handles
                .into_iter()
                .map(|(_, handle)| {
                    Arc::new(HandleSource::new((*handle).clone())) as Arc<dyn Source>
                })
                .collect();
            let registry = Registry::from_sources(&sources, settings.conflict.into()).await?;
            let source = Arc::new(RegistrySource::new(registry));
            Ok(Arc::new(Handle::new(source)))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::tool::local::LocalSource;
    use crate::message::text_content;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    #[tokio::test]
    async fn a_capability_source_bridges_into_the_registry() {
        let local = LocalSource::new()
            .with(definition("echo"), Arc::new(|_| Ok(text_content("hi"))))
            .expect("tool");
        let handle = Handle::new(Arc::new(local));
        let source = HandleSource::new(handle);
        let tools = source.tools().await.expect("tools");
        assert_eq!(tools.len(), 1);
        assert_eq!(tools[0].definition().name, "echo");
        assert_eq!(
            tools[0]
                .execute(&CallContext::default(), "{}")
                .await
                .expect("execute"),
            text_content("hi")
        );
    }

    #[tokio::test]
    async fn a_registry_source_lists_and_executes() {
        let mut source = crate::caps::tool::registry::StaticSource::new();
        source.push(Arc::new(crate::caps::tool::callable::FuncTool::new(
            definition("echo"),
            Arc::new(|_context, arguments| Ok(text_content(arguments))),
        )));
        let registry = Registry::from_sources(&[Arc::new(source)], ConflictPolicy::Error)
            .await
            .expect("registry");
        let source = RegistrySource::new(registry);
        assert_eq!(source.list().await.expect("list").len(), 1);
        assert_eq!(
            source.execute("echo", "{}").await.expect("execute"),
            text_content("{}")
        );
        assert!(source.execute("missing", "{}").await.is_err());
    }
}
