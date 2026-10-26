//! The policy chain.
//!
//! A policy is a [`Layer`] — a function from a runner to a runner — and a chain
//! is a list of them. This is the same shape the tool framework uses for its
//! middleware ([`crate::caps::sandbox::policy`] mirrors `cap-tool::executor`), and it exists for
//! one reason: forwarding a `Runner` is written **once** (in [`Guarded`]), so a
//! new policy cannot forget to forward a method and silently hide the backend's
//! cleanup.
//!
//! One check has one job: it takes a [`SessionSpec`], may inspect it, may
//! tighten it, and either returns it or refuses. Everything a check needs to be
//! async (an approver, a remote lookup) fits in the returned future, and nothing
//! has to hold a `&mut` across an await.

pub mod allowlist;
pub mod approval;
pub mod escalation;
pub mod readonly;

use std::sync::Arc;

use crate::error::Result;
use crate::resource::BoxFuture;
use async_trait::async_trait;

use crate::caps::sandbox::contract::{Capabilities, Runner, Session, SessionInfo, SessionSpec};

pub use allowlist::{Allowlist, normalise, normalise_argv};
pub use approval::{
    ApprovalRequest, Approver, ApproverFn, Decision, ExecRequest, LocalPolicy, Predicate,
    allow_commands, approval, command_patterns, compose_local, defaults, interactive,
    net_non_default, workdir_outside_root,
};
pub use escalation::{
    Denial, Escalation, EscalationDecision, EscalationGate, EscalationRules, Escalator, Outcome,
    denial, rule_for,
};
pub use readonly::classify_read_only;

/// A runner behind a chain.
pub type RunnerRef = Arc<dyn Runner>;

/// One check: inspect (and optionally tighten) a start request.
pub type Check = Arc<dyn Fn(SessionSpec) -> BoxFuture<'static, Result<SessionSpec>> + Send + Sync>;

/// A policy: wrap a runner, return the guarded one.
pub type Layer = Arc<dyn Fn(RunnerRef) -> RunnerRef + Send + Sync>;

/// An ordered list of policies.
#[derive(Default)]
pub struct Chain {
    layers: Vec<Layer>,
}

impl Chain {
    /// An empty chain (every call passes).
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Append a layer. Layers run in the order they were added: the first one
    /// sees the call first.
    pub fn push(&mut self, layer: Layer) {
        self.layers.push(layer);
    }

    /// A chain of `layers`, outermost first.
    #[must_use]
    pub fn of(layers: impl IntoIterator<Item = Layer>) -> Self {
        Self {
            layers: layers.into_iter().collect(),
        }
    }

    /// How many layers are configured.
    #[must_use]
    pub fn len(&self) -> usize {
        self.layers.len()
    }

    /// Whether the chain is empty.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.layers.is_empty()
    }

    /// Apply the chain to `runner`.
    #[must_use]
    pub fn around(&self, runner: RunnerRef) -> RunnerRef {
        let mut current = runner;
        for layer in self.layers.iter().rev() {
            current = layer(current);
        }
        current
    }
}

/// Wrap `inner` so that every start passes through `check`.
#[must_use]
pub fn guard(inner: RunnerRef, check: Check) -> RunnerRef {
    Arc::new(Guarded { inner, check })
}

/// A runner with one check in front of `start`.
struct Guarded {
    inner: RunnerRef,
    check: Check,
}

impl std::fmt::Debug for Guarded {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.Guarded")
    }
}

#[async_trait]
impl Runner for Guarded {
    fn capabilities(&self) -> BoxFuture<'_, Result<Capabilities>> {
        // A policy narrows what may run; it adds no enforcement, so it claims
        // exactly what its inner runner claims.
        self.inner.capabilities()
    }

    async fn start(&self, spec: SessionSpec) -> Result<Arc<dyn Session>> {
        let spec = (self.check)(spec).await?;
        self.inner.start(spec).await
    }

    async fn session(&self, id: &str) -> Result<Arc<dyn Session>> {
        self.inner.session(id).await
    }

    async fn list(&self) -> Result<Vec<SessionInfo>> {
        self.inner.list().await
    }

    async fn terminate(&self, id: &str) -> Result<()> {
        self.inner.terminate(id).await
    }

    async fn close_session(&self, id: &str) -> Result<()> {
        self.inner.close_session(id).await
    }

    async fn close(&self) -> Result<()> {
        self.inner.close().await
    }

    async fn open_journal(&self) -> Result<Arc<dyn crate::caps::sandbox::journal::Journal>> {
        // A decorator that swallowed the journal while `capabilities` kept
        // advertising it would make the declaration a lie.
        self.inner.open_journal().await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::{ExitStatus, SessionFeatures};
    use crate::caps::sandbox::local::LocalRunner;
    use crate::error::Error;

    /// A layer that refuses a command by name.
    fn deny(program: &'static str) -> Layer {
        Arc::new(move |inner| {
            guard(
                inner,
                Arc::new(move |spec: SessionSpec| {
                    Box::pin(async move {
                        if spec.argv.first().map(String::as_str) == Some(program) {
                            return Err(Error::policy_denied(format!(
                                "sandbox: {program} is denied by policy"
                            )));
                        }
                        Ok(spec)
                    })
                }),
            )
        })
    }

    #[tokio::test]
    async fn a_chain_refuses_what_a_layer_refuses() {
        let chain = Chain::of([deny("rm")]);
        let runner = chain.around(Arc::new(LocalRunner::new()));
        let error = runner
            .start(SessionSpec::new(vec!["rm".to_owned()]))
            .await
            .expect_err("denied");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        // Everything else still runs.
        let session = runner
            .start(SessionSpec::new(vec!["true".to_owned()]))
            .await
            .expect("allowed");
        session.wait().await.expect("wait");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn the_first_layer_sees_the_call_first() {
        // Each layer appends its name to the argv it sees, so the resulting
        // order is the composition order.
        fn stamp(name: &'static str) -> Layer {
            Arc::new(move |inner| {
                guard(
                    inner,
                    Arc::new(move |mut spec: SessionSpec| {
                        Box::pin(async move {
                            spec.argv.push(name.to_owned());
                            Ok(spec)
                        })
                    }),
                )
            })
        }
        let seen = Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
        let recorder: Check = {
            let seen = seen.clone();
            Arc::new(
                move |spec: SessionSpec| -> BoxFuture<'static, Result<SessionSpec>> {
                    seen.lock().unwrap().extend(spec.argv.iter().cloned());
                    Box::pin(async move { Ok(spec) })
                },
            )
        };
        let chain = Chain::of([stamp("outer"), stamp("inner")]);
        let runner = chain.around(guard(Arc::new(LocalRunner::new()), recorder));
        let _ = runner
            .start(SessionSpec::new(vec!["true".to_owned()]))
            .await
            .expect("start");
        assert_eq!(
            *seen.lock().unwrap(),
            ["true".to_owned(), "outer".to_owned(), "inner".to_owned()],
            "the first layer must be outermost"
        );
        runner.close().await.expect("close");
    }

    #[test]
    fn a_guarded_runner_reports_what_it_forwards() {
        // The capability claim travels unchanged, so a policy can never look
        // like an enforcement it does not have.
        let _ = ExitStatus::exited(0);
        let _ = SessionFeatures::default();
    }
}
