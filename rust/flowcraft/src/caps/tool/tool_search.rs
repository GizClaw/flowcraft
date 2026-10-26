//! The built-in `tool_search`: the model's discovery path.

use crate::error::{Error, Result};
use crate::proto::message as msg;
use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use crate::caps::tool::callable::{CallContext, Tool};
use crate::caps::tool::search::SearchHit;
use crate::caps::tool::session::DiscoverReason;

/// The built-in discovery tool name.
pub const TOOL_NAME: &str = "tool_search";

/// The model-facing discovery tool.
///
/// It reads the session from [`CallContext`], the Rust counterpart of Go's
/// context-attached session, so one instance serves every run.
#[derive(Debug, Default)]
pub struct SearchTool;

impl SearchTool {
    /// A stateless search tool.
    #[must_use]
    pub fn new() -> Self {
        Self
    }
}

#[derive(Debug, Deserialize)]
struct Arguments {
    query: String,
    #[serde(default)]
    limit: usize,
}

#[derive(Debug, Serialize)]
struct Failure {
    name: String,
    reason: &'static str,
}

#[derive(Debug, Serialize)]
struct Output {
    query: String,
    hits: Vec<SearchHit>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    exposed: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    failed: Vec<Failure>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    evicted: Vec<String>,
}

#[async_trait]
impl Tool for SearchTool {
    fn definition(&self) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: TOOL_NAME.to_owned(),
            description: "Search the tool catalog for tools relevant to the current task. \
                Matching tools are loaded and exposed to the model starting from the next round \
                when they fit that round's tool budget; over-budget matches come back as failed \
                with reason visible_budget. Exposed tools stay available while they are used and \
                are evicted after idle rounds or when the discovery budget is full."
                .to_owned(),
            input_schema: serde_json::to_vec(&serde_json::json!({
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": "natural-language or keyword query describing the capability to find"
                    },
                    "limit": {
                        "type": "integer",
                        "description": "maximum number of hits to return and expose; capped at the discovery pool size",
                        "default": crate::caps::tool::search::DEFAULT_SEARCH_LIMIT
                    }
                },
                "required": ["query"]
            }))
            .unwrap_or_default(),
        }
    }

    async fn execute(&self, context: &CallContext, arguments: &str) -> Result<msg::Content> {
        let session = context
            .session
            .as_ref()
            .ok_or_else(|| Error::not_available(format!("tool: {TOOL_NAME} requires a session")))?;
        let arguments: Arguments = serde_json::from_str(arguments).map_err(|error| {
            Error::validation(format!("tool: {TOOL_NAME}: parse arguments: {error}"))
        })?;
        if arguments.query.trim().is_empty() {
            return Err(Error::validation(format!(
                "tool: {TOOL_NAME}: query is required"
            )));
        }

        let hits = session.search(&arguments.query, arguments.limit).await?;
        let mut loaded = Vec::new();
        let mut failed = Vec::new();
        for hit in &hits {
            match session.ensure_loaded(&[hit.name.as_str()]).await {
                Ok(()) => loaded.push(hit.name.clone()),
                Err(_) => failed.push(Failure {
                    name: hit.name.clone(),
                    reason: "load_failed",
                }),
            }
        }
        let borrowed: Vec<&str> = loaded.iter().map(String::as_str).collect();
        let outcome = session.discover(&borrowed);
        let mut exposed = Vec::new();
        for result in &outcome.results {
            if result.exposed {
                exposed.push(result.name.clone());
                continue;
            }
            if let Some(reason) = result.reason
                && reason != DiscoverReason::Unknown
            {
                failed.push(Failure {
                    name: result.name.clone(),
                    reason: reason_name(reason),
                });
            }
        }

        let output = Output {
            query: arguments.query,
            hits,
            exposed,
            failed,
            evicted: outcome.evicted,
        };
        // The result rides as structured data, not prose, so redaction and
        // result limiting treat it like any other part.
        let value = serde_json::to_vec(&output).map_err(|error| {
            Error::internal(format!("tool: {TOOL_NAME}: encode result: {error}"))
        })?;
        Ok(msg::Content {
            parts: vec![msg::Part {
                kind: Some(msg::part::Kind::Data(msg::DataPart {
                    media_type: "application/json".to_owned(),
                    value,
                })),
            }],
        })
    }
}

fn reason_name(reason: DiscoverReason) -> &'static str {
    match reason {
        DiscoverReason::Unknown => "unknown",
        DiscoverReason::NotDiscoverable => "not_discoverable",
        DiscoverReason::OverBudget => "over_budget",
        DiscoverReason::VisibleBudget => "visible_budget",
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::caps::tool::callable::FuncTool;
    use crate::caps::tool::exposure::{Exposure, Policy};
    use crate::caps::tool::registry::{ConflictPolicy, Registry};
    use crate::caps::tool::session::{DynamicSession, Session};
    use crate::message::text_content;

    fn definition(name: &str, description: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: description.to_owned(),
            input_schema: Vec::new(),
        }
    }

    async fn session() -> Arc<dyn Session> {
        let registry = Registry::from_sources(&[], ConflictPolicy::Error)
            .await
            .expect("registry");
        registry
            .add(Arc::new(FuncTool::new(
                definition("grep", "search file contents"),
                Arc::new(|_context, _| Ok(text_content("ok"))),
            )))
            .expect("add");
        registry
            .add(Arc::new(FuncTool::new(
                definition("clock", "current time"),
                Arc::new(|_context, _| Ok(text_content("ok"))),
            )))
            .expect("add");
        Arc::new(
            DynamicSession::new(
                registry,
                Policy {
                    default: Exposure::Deferred,
                    ..Policy::default()
                },
            )
            .expect("session"),
        )
    }

    fn context(session: Arc<dyn Session>) -> CallContext {
        CallContext {
            session: Some(session),
        }
    }

    #[tokio::test]
    async fn it_exposes_the_top_hit() {
        let session = session().await;
        let tool = SearchTool::new();
        let content = tool
            .execute(&context(session.clone()), r#"{"query": "search files"}"#)
            .await
            .expect("run");
        let msg::part::Kind::Data(data) = content.parts[0].kind.as_ref().expect("kind") else {
            panic!("expected a data part");
        };
        let value: serde_json::Value = serde_json::from_slice(&data.value).expect("json");
        assert_eq!(value["hits"][0]["name"], "grep");
        assert_eq!(value["exposed"][0], "grep");
        assert_eq!(session.definitions()[0].name, "grep");
    }

    #[tokio::test]
    async fn it_refuses_an_empty_query_and_a_missing_session() {
        let tool = SearchTool::new();
        assert!(
            tool.execute(&CallContext::default(), r#"{"query": "x"}"#)
                .await
                .is_err()
        );

        let context = context(session().await);
        assert!(tool.execute(&context, r#"{"query": " "}"#).await.is_err());
        assert!(tool.execute(&context, "not json").await.is_err());
    }
}
