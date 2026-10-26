//! Assembly: the registry, the executor, and the injection policy as one value.
//!
//! Ported from `core/tool/assembly.go`, with one deliberate Rust difference:
//! the per-run session travels in [`CallContext`] instead of an ambient
//! context, so one long-lived assembly serves any number of runs and the
//! session recorder always sees the session that issued the call.

use std::sync::Arc;

use crate::error::Result;

use crate::caps::tool::executor::{Executor, Middleware};
use crate::caps::tool::exposure::{Exposure, Policy};
use crate::caps::tool::middleware::record_calls;
use crate::caps::tool::registry::{ConflictPolicy, Registry, Source, StaticSource};
use crate::caps::tool::session::{DynamicSession, Session, StaticSession};
use crate::caps::tool::tool_search::{SearchTool, TOOL_NAME};

/// How to build an [`Assembly`].
#[derive(Default)]
pub struct Options {
    /// Duplicate-name policy for the registry.
    pub conflict: ConflictPolicy,
    /// Extra middleware, outermost first. The session recorder is always
    /// appended as the innermost layer.
    pub middleware: Vec<Middleware>,
    /// Enable dynamic injection with this policy. `None` means a static
    /// assembly where every tool is visible.
    pub dynamic: Option<Policy>,
}

/// The standard tool value: read surface, execution surface, and policy.
pub struct Assembly {
    registry: Arc<Registry>,
    executor: Arc<Executor>,
    policy: Option<Policy>,
}

impl std::fmt::Debug for Assembly {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Assembly")
            .field("tools", &self.registry.len())
            .field("dynamic", &self.policy.is_some())
            .finish_non_exhaustive()
    }
}

impl Assembly {
    /// Build from sources. With dynamic injection enabled, `tool_search` is
    /// appended as a source so it is in the registry from construction.
    pub async fn new(sources: &[Arc<dyn Source>], options: Options) -> Result<Self> {
        let mut sources = sources.to_vec();
        let mut policy = None;

        if let Some(dynamic) = options.dynamic {
            let mut dynamic = dynamic.normalized();
            dynamic.validate()?;
            dynamic
                .exposures
                .entry(TOOL_NAME.to_owned())
                .or_insert(Exposure::Always);
            let mut source = StaticSource::new();
            source.push(Arc::new(SearchTool::new()));
            sources.push(Arc::new(source));
            policy = Some(dynamic);
        }

        let registry = Registry::from_sources(&sources, options.conflict).await?;
        let mut middleware = options.middleware;
        middleware.push(record_calls());
        let executor = Arc::new(Executor::new(registry.clone(), middleware));
        Ok(Self {
            registry,
            executor,
            policy,
        })
    }

    /// The aggregated read surface.
    #[must_use]
    pub fn registry(&self) -> &Arc<Registry> {
        &self.registry
    }

    /// The execution surface.
    #[must_use]
    pub fn executor(&self) -> &Arc<Executor> {
        &self.executor
    }

    /// A fresh per-run session. Dynamic when the assembly was built with a
    /// policy, static otherwise.
    pub fn session(&self) -> Result<Arc<dyn Session>> {
        match &self.policy {
            Some(policy) => Ok(Arc::new(DynamicSession::new(
                self.registry.clone(),
                policy.clone(),
            )?)),
            None => Ok(Arc::new(StaticSession::new(self.registry.clone()))),
        }
    }

    /// Close the registry, releasing deferred tools.
    pub fn close(&self) -> Result<()> {
        self.registry.close()
    }
}

#[cfg(test)]
mod tests {
    use crate::proto::message as msg;

    use super::*;
    use crate::caps::tool::callable::CallContext;
    use crate::caps::tool::callable::FuncTool;
    use crate::caps::tool::executor::Dispatcher;
    use crate::caps::tool::local::text_result;

    fn definition(name: &str, description: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: description.to_owned(),
            input_schema: Vec::new(),
        }
    }

    fn source() -> Arc<StaticSource> {
        let mut source = StaticSource::new();
        source.push(Arc::new(FuncTool::new(
            definition("grep", "search file contents"),
            Arc::new(|_context, _| Ok(text_result("ok"))),
        )));
        Arc::new(source)
    }

    fn context(session: Arc<dyn Session>) -> CallContext {
        CallContext {
            session: Some(session),
        }
    }

    #[tokio::test]
    async fn a_static_assembly_shows_every_tool() {
        let assembly = Assembly::new(&[source()], Options::default())
            .await
            .expect("assembly");
        let session = assembly.session().expect("session");
        assert_eq!(
            session
                .definitions()
                .iter()
                .map(|d| d.name.as_str())
                .collect::<Vec<_>>(),
            vec!["grep"]
        );
        assembly.close().expect("close");
    }

    #[tokio::test]
    async fn a_dynamic_assembly_registers_tool_search_and_records_calls() {
        let assembly = Assembly::new(
            &[source()],
            Options {
                dynamic: Some(Policy::default()),
                ..Options::default()
            },
        )
        .await
        .expect("assembly");
        let session = assembly.session().expect("session");

        // tool_search is always visible; grep is deferred until used.
        assert_eq!(
            session
                .definitions()
                .iter()
                .map(|d| d.name.as_str())
                .collect::<Vec<_>>(),
            vec![TOOL_NAME]
        );

        let result = assembly
            .executor()
            .execute(
                context(session.clone()),
                msg::ToolCall {
                    id: "c1".to_owned(),
                    name: "grep".to_owned(),
                    arguments: b"{}".to_vec(),
                },
            )
            .await;
        assert!(!result.is_error);

        // The recorder refreshed this session's discovery pool.
        assert!(session.definitions().iter().any(|d| d.name == "grep"));
        assembly.close().expect("close");
    }

    #[tokio::test]
    async fn tool_search_ranks_through_the_call_context() {
        let assembly = Assembly::new(
            &[source()],
            Options {
                dynamic: Some(Policy::default()),
                ..Options::default()
            },
        )
        .await
        .expect("assembly");
        let session = assembly.session().expect("session");
        let hits = session.search("search file", 8).await.expect("search");
        assert_eq!(hits[0].name, "grep");
        assembly.close().expect("close");
    }
}
