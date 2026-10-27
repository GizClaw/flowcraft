//! The one way to run synchronous work from an asynchronous context.
//!
//! `async fn` promises not to block the thread it runs on, and this crate broke
//! that promise in several places at once (`std::fs` in a store's `lookup`, a
//! blocking `getaddrinfo` on every proxied request, a `thread::sleep` retry
//! loop). The individual fixes were small; the reason they kept being needed is
//! that the rule was never written down in one executable place.
//!
//! The rule is:
//!
//! - **a trait is asynchronous, its implementation is synchronous.** The
//!   `async_trait` method may not call `std::fs::*`, `std::net::ToSocketAddrs`,
//!   or `std::thread::sleep` directly;
//! - **cross the boundary with [`run`]**: `spawn_blocking`, one error mapping,
//!   one span;
//! - **CPU-bound work is the same problem**: `spawn_blocking`, not the worker.
//!
//! Three places may do synchronous I/O without this module, and each says so
//! with an `#[allow(clippy::disallowed_methods)]` and a comment: a capability's
//! synchronous builder (`BuildCtx::from_*`), `#[cfg(test)]`, and code that is
//! *already* inside a [`run`] closure.
//!
//! The gate is `clippy.toml`'s `disallowed-methods`, because a convention
//! without one degrades within a year. `tests/layering.rs` pins which modules
//! may name this one.

use crate::error::{Error, Result};

/// Run `work` on the blocking pool.
///
/// `label` is a `&'static str` on purpose: it names the operation for the span
/// (`flowcraft.blocking = "secret.keychain.lookup"`), so it must not carry user
/// data — a secret name or a path — and it must not allocate per call.
///
/// A `Err` from `work` keeps its kind: only the join failure (a panic in the
/// closure, or a shutdown that drops it) is turned into [`Error::internal`].
pub async fn run<T, F>(label: &'static str, work: F) -> Result<T>
where
    F: FnOnce() -> Result<T> + Send + 'static,
    T: Send + 'static,
{
    use tracing::Instrument as _;

    let span = tracing::debug_span!("blocking", flowcraft.blocking = label);
    tokio::task::spawn_blocking(work)
        .instrument(span)
        .await
        .map_err(|error| Error::internal(format!("blocking task {label} failed: {error}")))?
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// A one-thread runtime: four `run` calls must all finish, because none of
    /// them occupies the worker while it waits. Without the boundary they would
    /// serialize (or, worse, deadlock behind one that never returns).
    #[test]
    fn the_work_runs_off_the_worker() {
        let workers = tokio::runtime::Builder::new_current_thread()
            .max_blocking_threads(4)
            .enable_all()
            .build()
            .expect("runtime");
        workers.block_on(async {
            let done = Arc::new(AtomicUsize::new(0));
            let started = std::time::Instant::now();
            let mut handles = Vec::new();
            for _ in 0..4 {
                let done = done.clone();
                handles.push(tokio::spawn(async move {
                    run("test.sleep", move || {
                        std::thread::sleep(std::time::Duration::from_millis(100));
                        done.fetch_add(1, Ordering::SeqCst);
                        Ok(())
                    })
                    .await
                }));
            }
            for handle in handles {
                handle.await.expect("join").expect("run");
            }
            assert_eq!(done.load(Ordering::SeqCst), 4);
            // Four 100 ms sleeps finished in well under 400 ms: they overlapped.
            assert!(
                started.elapsed() < std::time::Duration::from_millis(300),
                "the calls did not overlap: {:?}",
                started.elapsed()
            );
        });
    }

    #[tokio::test]
    async fn a_failure_keeps_its_kind() {
        let error = run("test.fail", || Err::<(), _>(Error::validation("nope")))
            .await
            .expect_err("error");
        assert_eq!(error.kind(), crate::error::ErrorKind::Validation);
        assert!(error.message().contains("nope"), "{error}");
    }

    #[tokio::test]
    async fn a_panic_becomes_an_internal_error_naming_the_label() {
        let error = run::<(), _>("test.panic", || panic!("boom"))
            .await
            .expect_err("error");
        assert_eq!(error.kind(), crate::error::ErrorKind::Internal);
        assert!(error.message().contains("test.panic"), "{error}");
    }
}
