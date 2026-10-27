//! Execution-shaping middleware: recovery, deadlines, concurrency, pacing,
//! session recording, and telemetry.

use std::collections::BTreeMap;
use std::panic::AssertUnwindSafe;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::error::{Error, Result};
use crate::proto::message as msg;
use futures_util::FutureExt;
use tracing::Instrument;

use super::error_result;
use crate::caps::tool::callable::CallContext;
use crate::caps::tool::executor::{Dispatch, Middleware};
use crate::caps::tool::registry::Catalog;

/// Turn a panicking tool (or inner middleware) into an error result.
///
/// [`crate::caps::tool::executor::Executor`] keeps its own net for chains built by hand, so
/// this middleware is about *attribution*: the panic is reported with the tool
/// name and becomes an ordinary failed result the outer layers can inspect.
pub fn recover() -> Middleware {
    Arc::new(move |next: Dispatch| {
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            Box::pin(async move {
                let id = call.id.clone();
                let name = call.name.clone();
                let future = AssertUnwindSafe(next(context, call));
                match future.catch_unwind().await {
                    Ok(result) => result,
                    Err(payload) => error_result(
                        &id,
                        format!(
                            "tool {name:?} panicked: {}",
                            crate::caps::tool::executor::panic_message(payload.as_ref())
                        ),
                    ),
                }
            })
        })
    })
}

/// Bound how long a call may run.
///
/// A tool can exempt itself with [`crate::caps::tool::callable::ToolMeta::self_timeout`], which
/// [`timeout_with_catalog`] reads; an explicit `per_tool` entry always wins. A
/// zero limit means "no deadline".
pub fn timeout(default: Duration, per_tool: BTreeMap<String, Duration>) -> Middleware {
    timeout_inner(None, default, Arc::new(per_tool))
}

/// [`timeout`] that honours each tool's `self_timeout` declaration.
pub fn timeout_with_catalog(
    catalog: Arc<dyn Catalog>,
    default: Duration,
    per_tool: BTreeMap<String, Duration>,
) -> Middleware {
    timeout_inner(Some(catalog), default, Arc::new(per_tool))
}

fn timeout_inner(
    catalog: Option<Arc<dyn Catalog>>,
    default: Duration,
    per_tool: Arc<BTreeMap<String, Duration>>,
) -> Middleware {
    Arc::new(move |next: Dispatch| {
        let catalog = catalog.clone();
        let per_tool = per_tool.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let catalog = catalog.clone();
            let per_tool = per_tool.clone();
            Box::pin(async move {
                let limit = match per_tool.get(&call.name).copied() {
                    Some(limit) => limit,
                    None => {
                        let self_timeout = catalog
                            .as_ref()
                            .and_then(|catalog| catalog.get(&call.name))
                            .map(|tool| tool.metadata().self_timeout)
                            .unwrap_or(false);
                        if self_timeout {
                            Duration::ZERO
                        } else {
                            default
                        }
                    }
                };
                if limit.is_zero() {
                    return next(context, call).await;
                }
                let id = call.id.clone();
                let name = call.name.clone();
                match tokio::time::timeout(limit, next(context, call)).await {
                    Ok(result) => result,
                    Err(_) => error_result(
                        &id,
                        format!("tool {name:?} exceeded its {limit:?} deadline"),
                    ),
                }
            })
        })
    })
}

/// Cap how many calls may be in flight at once; excess callers wait.
pub fn concurrency(limit: usize) -> Result<Middleware> {
    if limit == 0 {
        return Err(Error::validation(
            "middleware.concurrency: limit must be positive",
        ));
    }
    let permits = Arc::new(tokio::sync::Semaphore::new(limit));
    Ok(Arc::new(move |next: Dispatch| {
        let permits = permits.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let permits = permits.clone();
            Box::pin(async move {
                let id = call.id.clone();
                let name = call.name.clone();
                let _permit = match permits.acquire().await {
                    Ok(permit) => permit,
                    Err(_) => {
                        return error_result(&id, format!("tool {name:?}: limiter closed"));
                    }
                };
                next(context, call).await
            })
        })
    }))
}

/// Pace calls per tool according to [`crate::caps::tool::callable::ToolMeta::rate_limit`].
///
/// Each call takes the next slot at `1/rate` intervals (no bursting). Metadata
/// is resolved once per tool; re-registering with different metadata needs a new
/// executor, because chains are immutable by design.
pub fn rate_limit(catalog: Arc<dyn Catalog>) -> Middleware {
    let pacers: Arc<Mutex<BTreeMap<String, Option<Arc<Pacer>>>>> =
        Arc::new(Mutex::new(BTreeMap::new()));
    Arc::new(move |next: Dispatch| {
        let pacers = pacers.clone();
        let catalog = catalog.clone();
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            let pacers = pacers.clone();
            let catalog = catalog.clone();
            Box::pin(async move {
                if let Some(pacer) = pacer_for(&pacers, &catalog, &call.name) {
                    let wait = pacer.reserve();
                    if !wait.is_zero() {
                        tokio::time::sleep(wait).await;
                    }
                }
                next(context, call).await
            })
        })
    })
}

struct Pacer {
    interval: Duration,
    next: Mutex<Instant>,
}

/// The longest interval one rate-limit slot may impose.
///
/// A rate slow enough to mean more than an hour per call is a configuration
/// mistake; capping it bounds the sleep without ever making the limiter looser
/// than the tool asked for.
const MAX_INTERVAL: Duration = Duration::from_secs(3600);

impl Pacer {
    fn reserve(&self) -> Duration {
        let Ok(mut next) = self.next.lock() else {
            return Duration::ZERO;
        };
        let now = Instant::now();
        let start = (*next).max(now);
        *next = start + self.interval;
        start.saturating_duration_since(now)
    }
}

fn pacer_for(
    pacers: &Mutex<BTreeMap<String, Option<Arc<Pacer>>>>,
    catalog: &Arc<dyn Catalog>,
    name: &str,
) -> Option<Arc<Pacer>> {
    let Ok(mut map) = pacers.lock() else {
        return None;
    };
    if let Some(entry) = map.get(name) {
        return entry.clone();
    }
    let pacer = catalog.get(name).and_then(|tool| {
        let rate = tool.metadata().rate_limit;
        // Anything that is not a finite positive rate is not a claim: NaN, an
        // infinity, zero and negatives all mean "no pacing".
        if !rate.is_finite() || rate <= 0.0 {
            return None;
        }
        let seconds = 1.0 / rate;
        // `Duration::from_secs_f64` panics on an overflowing value, and a tool
        // that declares a rate of `1e-300` is a metadata mistake rather than a
        // reason to take the daemon down. Cap instead, which keeps the limiter
        // honest: a caller is never paced *faster* than it asked for.
        let interval = if seconds.is_finite() && seconds <= MAX_INTERVAL.as_secs_f64() {
            Duration::from_secs_f64(seconds)
        } else {
            tracing::warn!(
                tool = name,
                rate,
                max_secs = MAX_INTERVAL.as_secs(),
                "tool rate limit is slower than the cap; using the cap"
            );
            MAX_INTERVAL
        };
        Some(Arc::new(Pacer {
            interval,
            next: Mutex::new(Instant::now()),
        }))
    });
    map.insert(name.to_owned(), pacer.clone());
    pacer
}

/// Record every dispatched call in the session carried by the context, so
/// active use refreshes the discovery pool.
pub fn record_calls() -> Middleware {
    Arc::new(move |next: Dispatch| {
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            Box::pin(async move {
                let result = next(context.clone(), call.clone()).await;
                if let Some(session) = context.session.as_ref() {
                    session.record_call(&call.name);
                }
                result
            })
        })
    })
}

/// One span per call, plus a warning log for failures.
///
/// The Go version also records metrics; the Rust telemetry crate exposes spans
/// and logs, so metrics stay with whatever sink the host installs.
pub fn telemetry() -> Middleware {
    Arc::new(move |next: Dispatch| {
        Arc::new(move |context: CallContext, call: msg::ToolCall| {
            let next = next.clone();
            Box::pin(async move {
                let span = tracing::info_span!(
                    "tool.execute",
                    tool = %call.name,
                    call_id = %call.id,
                );
                let result = next(context, call).instrument(span).await;
                if result.is_error {
                    tracing::warn!(
                        tool_call_id = %result.call_id,
                        "tool execution failed"
                    );
                }
                result
            })
        })
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::message::text_content;

    fn call(name: &str) -> msg::ToolCall {
        msg::ToolCall {
            id: "call-1".to_owned(),
            name: name.to_owned(),
            arguments: b"{}".to_vec(),
        }
    }

    fn ok() -> Dispatch {
        Arc::new(|_context, _call| {
            Box::pin(async {
                msg::ToolResult {
                    call_id: "call-1".to_owned(),
                    content: Some(text_content("ok")),
                    is_error: false,
                }
            })
        })
    }

    #[tokio::test]
    async fn recover_turns_a_panic_into_an_error_result() {
        let chain = recover()(Arc::new(|_context, _call| {
            Box::pin(async { panic!("kaboom") })
        }));
        let result = chain(CallContext::default(), call("boom")).await;
        assert!(result.is_error);
        assert!(super::super::result_text(&result).contains("kaboom"));
    }

    #[tokio::test]
    async fn timeout_replaces_a_slow_call() {
        let chain =
            timeout(Duration::from_millis(10), BTreeMap::new())(Arc::new(|_context, _call| {
                Box::pin(async {
                    tokio::time::sleep(Duration::from_secs(5)).await;
                    msg::ToolResult {
                        call_id: "call-1".to_owned(),
                        content: Some(text_content("late")),
                        is_error: false,
                    }
                })
            }));
        let result = chain(CallContext::default(), call("slow")).await;
        assert!(result.is_error);
    }

    #[tokio::test]
    async fn a_zero_timeout_imposes_nothing() {
        let chain = timeout(Duration::ZERO, BTreeMap::new())(ok());
        let result = chain(CallContext::default(), call("fast")).await;
        assert!(!result.is_error);
    }

    #[tokio::test]
    async fn concurrency_rejects_zero_and_passes_calls() {
        assert!(concurrency(0).is_err());
        let chain = concurrency(2).expect("limiter")(ok());
        assert!(!chain(CallContext::default(), call("a")).await.is_error);
    }

    /// A tool's declared rate is host-supplied metadata, so the limiter must
    /// survive anything it can hand over: `Duration::from_secs_f64` panics on an
    /// overflowing value, and a panic here would sit inside the call path.
    #[tokio::test]
    async fn an_absurd_rate_is_capped_instead_of_panicking() {
        for rate in [1e-300, f64::MIN_POSITIVE / 4.0, 1e-12] {
            let pacer = pacer(rate).expect("a capped pacer");
            assert_eq!(
                pacer.interval, MAX_INTERVAL,
                "rate {rate} must be capped, not panicked on"
            );
        }
        // Not a claim at all: NaN fails `> 0.0`, and so do zero and negatives.
        for rate in [f64::NAN, 0.0, -1.0] {
            assert!(pacer(rate).is_none(), "rate {rate} must not pace");
        }
        // A rate that fits keeps exactly its interval.
        assert_eq!(
            pacer(4.0).expect("paced").interval,
            Duration::from_millis(250)
        );
    }

    /// Build a pacer for a tool that declares `rate`.
    fn pacer(rate: f64) -> Option<Arc<Pacer>> {
        struct Stub(f64);

        #[async_trait::async_trait]
        impl crate::caps::tool::callable::Tool for Stub {
            fn definition(&self) -> msg::ToolDefinition {
                msg::ToolDefinition {
                    name: "paced".to_owned(),
                    description: String::new(),
                    input_schema: Vec::new(),
                }
            }

            async fn execute(
                &self,
                _context: &CallContext,
                _arguments: &str,
            ) -> Result<msg::Content> {
                Ok(text_content("ok"))
            }

            fn metadata(&self) -> crate::caps::tool::callable::ToolMeta {
                crate::caps::tool::callable::ToolMeta {
                    rate_limit: self.0,
                    ..crate::caps::tool::callable::ToolMeta::default()
                }
            }
        }

        struct One(Arc<Stub>);

        impl Catalog for One {
            fn get(&self, name: &str) -> Option<Arc<dyn crate::caps::tool::callable::Tool>> {
                (name == "paced")
                    .then(|| self.0.clone() as Arc<dyn crate::caps::tool::callable::Tool>)
            }

            fn definitions(&self) -> Arc<[msg::ToolDefinition]> {
                Arc::from(Vec::new())
            }
        }

        let catalog: Arc<dyn Catalog> = Arc::new(One(Arc::new(Stub(rate))));
        let pacers = Mutex::new(BTreeMap::new());
        pacer_for(&pacers, &catalog, "paced")
    }
}
