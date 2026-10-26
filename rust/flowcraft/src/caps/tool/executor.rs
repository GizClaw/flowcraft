//! Tool dispatch: a catalog lookup plus a middleware chain.
//!
//! Ported from `core/tool/executor.go`. Every failure — unknown tool, tool
//! error, panicking tool — becomes a `ToolResult` with `is_error`, never an
//! out-of-band error, because callers may be fan-out tasks with nowhere to
//! return to.

use std::panic::AssertUnwindSafe;
use std::sync::Arc;

use crate::proto::message as msg;
use crate::resource::BoxFuture;
use futures_util::FutureExt;

use crate::caps::tool::callable::CallContext;
use crate::caps::tool::middleware::error_result;
use crate::caps::tool::registry::Catalog;
use crate::message::text_content;

/// One call, resolving to its result.
pub type Dispatch =
    Arc<dyn Fn(CallContext, msg::ToolCall) -> BoxFuture<'static, msg::ToolResult> + Send + Sync>;

/// Wraps a [`Dispatch`]. The first middleware is outermost.
pub type Middleware = Arc<dyn Fn(Dispatch) -> Dispatch + Send + Sync>;

/// The execution contract consumers depend on.
pub trait Dispatcher: Send + Sync {
    /// Run one call.
    fn execute(&self, context: CallContext, call: msg::ToolCall) -> BoxFuture<'_, msg::ToolResult>;

    /// Run every call concurrently, returning results in input order.
    fn execute_all(
        &self,
        context: CallContext,
        calls: Vec<msg::ToolCall>,
    ) -> BoxFuture<'_, Vec<msg::ToolResult>>;
}

/// Dispatches calls against a catalog through a middleware chain.
pub struct Executor {
    dispatch: Dispatch,
}

impl std::fmt::Debug for Executor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Executor")
    }
}

impl Executor {
    /// Build an executor over `catalog`. Middleware are applied in order: the
    /// first sees the call first and the result last.
    #[must_use]
    pub fn new(catalog: Arc<dyn Catalog>, middleware: Vec<Middleware>) -> Self {
        let mut dispatch = core_dispatch(catalog);
        for layer in middleware.into_iter().rev() {
            dispatch = layer(dispatch);
        }
        Self { dispatch }
    }

    /// Build from an already-composed dispatch, for tests.
    #[must_use]
    pub fn from_dispatch(dispatch: Dispatch) -> Self {
        Self { dispatch }
    }
}

impl Dispatcher for Executor {
    fn execute(&self, context: CallContext, call: msg::ToolCall) -> BoxFuture<'_, msg::ToolResult> {
        let dispatch = self.dispatch.clone();
        Box::pin(run(dispatch, context, call))
    }

    fn execute_all(
        &self,
        context: CallContext,
        calls: Vec<msg::ToolCall>,
    ) -> BoxFuture<'_, Vec<msg::ToolResult>> {
        let dispatch = self.dispatch.clone();
        Box::pin(async move {
            let count = calls.len();
            let mut set = tokio::task::JoinSet::new();
            for (index, call) in calls.into_iter().enumerate() {
                let dispatch = dispatch.clone();
                let context = context.clone();
                set.spawn(async move { (index, run(dispatch, context, call).await) });
            }
            let mut results: Vec<Option<msg::ToolResult>> =
                std::iter::repeat_with(|| None).take(count).collect();
            while let Some(joined) = set.join_next().await {
                if let Ok((index, result)) = joined {
                    results[index] = Some(result);
                }
            }
            results
                .into_iter()
                .map(|result| {
                    result.unwrap_or_else(|| {
                        error_result("", "tool: dispatch task did not produce a result")
                    })
                })
                .collect()
        })
    }
}

async fn run(dispatch: Dispatch, context: CallContext, call: msg::ToolCall) -> msg::ToolResult {
    let id = call.id.clone();
    let name = call.name.clone();
    let future = AssertUnwindSafe(async move { dispatch(context, call).await });
    match future.catch_unwind().await {
        Ok(result) => result,
        Err(payload) => error_result(
            &id,
            format!(
                "tool {name:?} panicked: {}",
                panic_message(payload.as_ref())
            ),
        ),
    }
}

pub(crate) fn panic_message(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(message) = payload.downcast_ref::<&str>() {
        (*message).to_owned()
    } else if let Some(message) = payload.downcast_ref::<String>() {
        message.clone()
    } else {
        "unknown panic".to_owned()
    }
}

fn core_dispatch(catalog: Arc<dyn Catalog>) -> Dispatch {
    Arc::new(move |context: CallContext, call: msg::ToolCall| {
        let catalog = catalog.clone();
        Box::pin(async move {
            // `ToolCall.arguments` is bytes on the wire; a tool handler takes
            // text. Refuse an invalid sequence instead of silently turning it
            // into replacement characters the provider would then parse.
            let arguments = match std::str::from_utf8(&call.arguments) {
                Ok(arguments) => arguments.to_owned(),
                Err(_) => {
                    return error_result(&call.id, "tool arguments are not valid UTF-8");
                }
            };
            let Some(tool) = catalog.get(&call.name) else {
                return error_result(&call.id, format!("tool {:?} not found", call.name));
            };
            match tool.execute(&context, &arguments).await {
                Ok(mut content) => {
                    if content.parts.is_empty() {
                        // A partless result is legal at the tool boundary but
                        // invalid as a message payload; normalize it here.
                        content = text_content("");
                    }
                    if let Err(error) = crate::message::validate_content(&content) {
                        return error_result(
                            &call.id,
                            format!("tool {:?} returned invalid content: {error}", call.name),
                        );
                    }
                    msg::ToolResult {
                        call_id: call.id,
                        content: Some(content),
                        is_error: false,
                    }
                }
                Err(error) => error_result(&call.id, error.to_string()),
            }
        })
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::tool::callable::FuncTool;
    use crate::caps::tool::registry::{ConflictPolicy, Registry};
    use crate::message::text_content;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    fn call(name: &str) -> msg::ToolCall {
        msg::ToolCall {
            id: format!("call-{name}"),
            name: name.to_owned(),
            arguments: b"{}".to_vec(),
        }
    }

    async fn catalog() -> Arc<Registry> {
        let registry = Registry::from_sources(&[], ConflictPolicy::Error)
            .await
            .expect("registry");
        registry
            .add(Arc::new(FuncTool::new(
                definition("echo"),
                Arc::new(|_context, args| Ok(text_content(args))),
            )))
            .expect("add");
        registry
            .add(Arc::new(FuncTool::new(
                definition("boom"),
                Arc::new(|_context, _| Err(crate::error::Error::validation("boom"))),
            )))
            .expect("add");
        registry
            .add(Arc::new(FuncTool::new(
                definition("empty"),
                Arc::new(|_context, _| Ok(msg::Content { parts: Vec::new() })),
            )))
            .expect("add");
        registry
            .add(Arc::new(FuncTool::new(
                definition("panics"),
                Arc::new(|_context, _| panic!("kaboom")),
            )))
            .expect("add");
        registry
    }

    fn text_of(result: &msg::ToolResult) -> String {
        let content = result.content.as_ref().expect("content");
        match content.parts[0].kind.as_ref().expect("kind") {
            msg::part::Kind::Text(text) => text.text.clone(),
            other => panic!("unexpected part {other:?}"),
        }
    }

    #[tokio::test]
    async fn it_runs_a_known_tool() {
        let executor = Executor::new(catalog().await, Vec::new());
        let result = executor.execute(CallContext::default(), call("echo")).await;
        assert!(!result.is_error);
        assert_eq!(result.call_id, "call-echo");
        assert_eq!(text_of(&result), "{}");
    }

    #[tokio::test]
    async fn failures_are_results_not_errors() {
        let executor = Executor::new(catalog().await, Vec::new());
        for name in ["missing", "boom", "panics"] {
            let result = executor.execute(CallContext::default(), call(name)).await;
            assert!(result.is_error, "{name} must be an error result");
        }
        let result = executor
            .execute(CallContext::default(), call("panics"))
            .await;
        assert!(text_of(&result).contains("kaboom"), "{}", text_of(&result));
    }

    #[tokio::test]
    async fn invalid_utf8_arguments_become_an_error_result() {
        let executor = Executor::new(catalog().await, Vec::new());
        let mut call = call("echo");
        call.arguments = vec![0xff, 0xfe];
        let result = executor.execute(CallContext::default(), call).await;
        assert!(result.is_error);
        assert!(text_of(&result).contains("UTF-8"), "{}", text_of(&result));
    }

    #[tokio::test]
    async fn a_partless_result_is_normalized_to_empty_text() {
        let executor = Executor::new(catalog().await, Vec::new());
        let result = executor
            .execute(CallContext::default(), call("empty"))
            .await;
        assert!(!result.is_error);
        assert_eq!(text_of(&result), "");
    }

    #[tokio::test]
    async fn middleware_wraps_in_order() {
        let tag: Middleware = Arc::new(|next: Dispatch| {
            Arc::new(move |context: CallContext, call: msg::ToolCall| {
                let next = next.clone();
                Box::pin(async move {
                    let mut result = next(context, call).await;
                    if let Some(content) = result.content.as_mut()
                        && let Some(msg::part::Kind::Text(text)) = content
                            .parts
                            .first_mut()
                            .and_then(|part| part.kind.as_mut())
                    {
                        text.text.push_str(":outer");
                    }
                    result
                })
            })
        });
        let inner: Middleware = Arc::new(|next: Dispatch| {
            Arc::new(move |context: CallContext, call: msg::ToolCall| {
                let next = next.clone();
                Box::pin(async move {
                    let mut result = next(context, call).await;
                    if let Some(content) = result.content.as_mut()
                        && let Some(msg::part::Kind::Text(text)) = content
                            .parts
                            .first_mut()
                            .and_then(|part| part.kind.as_mut())
                    {
                        text.text.push_str(":inner");
                    }
                    result
                })
            })
        });
        let executor = Executor::new(catalog().await, vec![tag, inner]);
        let result = executor.execute(CallContext::default(), call("echo")).await;
        assert_eq!(text_of(&result), "{}:inner:outer");
    }

    #[tokio::test]
    async fn execute_all_keeps_input_order() {
        let executor = Executor::new(catalog().await, Vec::new());
        let results = executor
            .execute_all(
                CallContext::default(),
                vec![call("echo"), call("missing"), call("boom")],
            )
            .await;
        assert_eq!(results.len(), 3);
        assert_eq!(results[0].call_id, "call-echo");
        assert!(results[1].is_error);
        assert!(results[2].is_error);
    }
}
