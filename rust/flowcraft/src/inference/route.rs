//! Routing: which target a call runs against, and what happens when it fails.
//!
//! The design draws the boundary *above* routing — a client names a tier and the platform
//! decides — and this module is that decision. Four rules, each copied from the Go runtime
//! and each a place where a plausible implementation is subtly wrong:
//!
//! - **a tier is an ordered list of targets**, and the order is the deployment's. Selection
//!   is a lookup, not a score: a platform that ranks models by its own idea of quality has
//!   overruled the operator.
//! - **a retry is the same target, a fallback is a different one.** The ledger derives the
//!   distinction; this module decides *whether* to make either.
//! - **only a transient failure is retried** (`rate_limit`, `timeout`, `not_available`), and
//!   **never after output the caller could see**. A validation or permission error is the
//!   caller's to fix, and re-running a call that already streamed duplicates the answer.
//! - **a breaker is per target and process-wide.** It exists so that N callers do not each
//!   discover the same dead provider, which is exactly what N independent client-side
//!   breakers would do.
//!
//! Backoff is deterministic (seed × 2ⁿ, capped): the Go side adds full jitter because many
//! independent clients can synchronise on a dead provider, and here the breaker plays that
//! role for one daemon. A test can therefore assert the delay instead of sampling it.

use std::collections::BTreeMap;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::error::{Error, ErrorKind, Result};
use crate::inference::ledger::{Ledger, Outcome, Phase};
use crate::inference::pipeline::Generation;
use crate::inference::request::GenerateRequest;

/// The one operation routing needs from a provider.
///
/// Deliberately narrower than the capability's `Inference`: routing decides *which* target,
/// not what a provider serves or how a request compiles, and a trait that asked for models
/// and explain would make the route depend on the layer above it.
#[async_trait::async_trait]
pub trait Generator: Send + Sync {
    /// One unary call against the model the request names.
    async fn generate(&self, request: GenerateRequest) -> Result<Generation>;
}

/// What a caller asks for: the three strengths the Go runtime already has.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Choice {
    /// A routing class the deployment declared. The recommended path.
    Tier(String),
    /// An exact model, bypassing the tier's order.
    Model(String),
    /// A soft preference: ignored when it matches nothing declared.
    Hint {
        /// The preferred model.
        hint: String,
        /// The tier to use when the hint does not resolve.
        tier: String,
    },
}

/// One target a tier lists.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Target {
    /// The model a request will name.
    pub model: String,
}

/// How many times, and how far apart.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RetryPolicy {
    /// Attempts per target, including the first. Zero is read as one.
    pub max_attempts: u32,
    /// Move to the next target once a transient failure has exhausted its retries.
    ///
    /// Off by default, matching the Go runtime: a retry budget is about one target, and
    /// spending the next target's is a decision the deployment makes.
    pub fallback_on_retry_exhausted: bool,
    /// A cap on the attempts one call may spend across every target. Zero is no cap.
    pub max_total_attempts: u32,
    /// The first delay; doubled per attempt.
    pub seed: Duration,
    /// The longest delay.
    pub cap: Duration,
}

impl Default for RetryPolicy {
    fn default() -> Self {
        Self {
            max_attempts: 1,
            fallback_on_retry_exhausted: false,
            max_total_attempts: 0,
            seed: Duration::from_millis(100),
            cap: Duration::from_secs(2),
        }
    }
}

impl RetryPolicy {
    /// The delay before attempt `number` (1-based; the first attempt has none).
    #[must_use]
    pub fn backoff(&self, number: u32) -> Duration {
        if number <= 1 {
            return Duration::ZERO;
        }
        let mut delay = self.seed;
        for _ in 1..number {
            delay = delay.saturating_mul(2);
            if delay >= self.cap {
                return self.cap;
            }
        }
        delay.min(self.cap)
    }

    /// Whether a failure may be retried on the same target.
    ///
    /// The safety invariant comes first: output the caller could see is never retried, and a
    /// kind the caller must fix is not worth another call.
    #[must_use]
    pub fn retryable(&self, error: &Error, observable_output: bool) -> bool {
        if observable_output {
            return false;
        }
        matches!(
            error.kind(),
            ErrorKind::RateLimit | ErrorKind::Timeout | ErrorKind::NotAvailable
        )
    }
}

/// When a target is taken out of rotation, and for how long.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BreakerPolicy {
    /// Consecutive failures that open the breaker.
    pub threshold: u32,
    /// How long it stays open before one attempt is allowed through.
    pub cooldown: Duration,
}

impl Default for BreakerPolicy {
    fn default() -> Self {
        Self {
            threshold: 3,
            cooldown: Duration::from_secs(30),
        }
    }
}

/// The deployment's routing table.
#[derive(Debug, Clone, Default)]
pub struct Policy {
    /// Tiers, in the deployment's order.
    pub tiers: BTreeMap<String, Vec<Target>>,
    /// Retry behaviour.
    pub retry: RetryPolicy,
    /// Breaker behaviour.
    pub breaker: BreakerPolicy,
}

/// One target's breaker.
#[derive(Debug)]
struct Breaker {
    failures: u32,
    opened_at: Option<Instant>,
}

impl Breaker {
    fn new() -> Self {
        Self {
            failures: 0,
            opened_at: None,
        }
    }

    /// Whether an attempt may be made now, transitioning out of the open state when the
    /// cooldown has passed.
    fn allows(&mut self, now: Instant, policy: &BreakerPolicy) -> bool {
        let Some(opened) = self.opened_at else {
            return true;
        };
        if now.saturating_duration_since(opened) >= policy.cooldown {
            // Half-open: one attempt is allowed; a failure re-opens, a success resets.
            self.opened_at = None;
            self.failures = policy.threshold;
            return true;
        }
        false
    }

    fn record(&mut self, outcome: Outcome, now: Instant, policy: &BreakerPolicy) {
        match outcome {
            Outcome::Succeeded => {
                self.failures = 0;
                self.opened_at = None;
            }
            Outcome::Failed => {
                self.failures = self.failures.saturating_add(1);
                if self.failures >= policy.threshold {
                    self.opened_at = Some(now);
                }
            }
            // A refusal the breaker made is not evidence about the provider.
            Outcome::Opened | Outcome::Skipped => {}
        }
    }
}

/// A routing table with live breakers.
pub struct Route {
    policy: Policy,
    breakers: Mutex<BTreeMap<String, Breaker>>,
    /// The clock, so a test can move time instead of sleeping through a cooldown.
    clock: Box<dyn Fn() -> Instant + Send + Sync>,
}

impl std::fmt::Debug for Route {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("inference.Route")
            .field("tiers", &self.policy.tiers.keys().collect::<Vec<_>>())
            .finish_non_exhaustive()
    }
}

impl Route {
    /// Route with an injected clock.
    #[must_use]
    pub fn with_clock(policy: Policy, clock: Box<dyn Fn() -> Instant + Send + Sync>) -> Self {
        Self {
            policy,
            breakers: Mutex::new(BTreeMap::new()),
            clock,
        }
    }

    /// The targets a choice resolves to, in the order they will be tried.
    ///
    /// A hint is a *preference*: a target it names is moved to the front, and a hint that
    /// matches nothing is ignored rather than an error — matching the Go runtime's
    /// deliberately profile-blind hint.
    pub fn resolve(&self, choice: &Choice) -> Result<Vec<Target>> {
        match choice {
            Choice::Model(model) => Ok(vec![Target {
                model: model.clone(),
            }]),
            Choice::Tier(tier) => self.tier(tier),
            Choice::Hint { hint, tier } => {
                let mut targets = self.tier(tier)?;
                if let Some(at) = targets.iter().position(|target| &target.model == hint) {
                    let preferred = targets.remove(at);
                    targets.insert(0, preferred);
                }
                Ok(targets)
            }
        }
    }

    fn tier(&self, tier: &str) -> Result<Vec<Target>> {
        self.policy
            .tiers
            .get(tier)
            .filter(|targets| !targets.is_empty())
            .cloned()
            .ok_or_else(|| {
                Error::validation(format!(
                    "inference: tier {tier:?} is not declared with any target"
                ))
            })
    }

    /// Run one call through the table.
    ///
    /// The ledger is the caller's, for the same reason the pipeline's is: it has to survive a
    /// failure, and a caller that told you "every target failed" should be able to say which
    /// ones and what they cost.
    pub async fn generate(
        &self,
        provider: &dyn Generator,
        choice: Choice,
        request: GenerateRequest,
        ledger: &mut Ledger,
    ) -> Result<Generation> {
        let targets = self.resolve(&choice)?;
        let mut last: Option<Error> = None;
        let mut total: u32 = 0;
        for target in targets {
            let max = self.policy.retry.max_attempts.max(1);
            for number in 1..=max {
                if self.policy.retry.max_total_attempts != 0
                    && total >= self.policy.retry.max_total_attempts
                {
                    return Err(last.unwrap_or_else(|| {
                        Error::not_available(
                            "inference: the call spent its total attempt budget without an answer",
                        )
                    }));
                }
                let backoff = self.policy.retry.backoff(number);
                let now = (self.clock)();
                if !self.breaker_allows(&target.model, now) {
                    let index = ledger.begin(&target.model, Phase::Open, Duration::ZERO);
                    ledger.complete(index, Outcome::Opened, None, None, false);
                    // An open breaker is a "come back later", never a terminal verdict: the
                    // next target may be healthy.
                    last = Some(Error::not_available(format!(
                        "inference: {target:?} is tripped open after repeated failures",
                        target = target.model
                    )));
                    break;
                }
                if !backoff.is_zero() {
                    tokio::time::sleep(backoff).await;
                }
                total += 1;
                let index = ledger.begin(&target.model, Phase::Execute, backoff);
                let mut attempt = request.clone();
                attempt.model = target.model.clone();
                match provider.generate(attempt).await {
                    Ok(generation) => {
                        ledger.complete(
                            index,
                            Outcome::Succeeded,
                            None,
                            Some(&generation.response.usage),
                            false,
                        );
                        self.breaker_record(&target.model, Outcome::Succeeded, (self.clock)());
                        return Ok(generation);
                    }
                    Err(error) => {
                        let retryable = self
                            .policy
                            .retry
                            .retryable(&error, ledger.observed_output());
                        ledger.complete(
                            index,
                            Outcome::Failed,
                            Some(error.kind().as_str()),
                            None,
                            false,
                        );
                        self.breaker_record(&target.model, Outcome::Failed, (self.clock)());
                        last = Some(error);
                        if !retryable {
                            break;
                        }
                        if number == max && !self.policy.retry.fallback_on_retry_exhausted {
                            break;
                        }
                    }
                }
            }
        }
        Err(last.unwrap_or_else(|| {
            Error::not_available("inference: no target was tried (the tier is empty)")
        }))
    }

    fn breaker_allows(&self, target: &str, now: Instant) -> bool {
        let Ok(mut breakers) = self.breakers.lock() else {
            return true;
        };
        breakers
            .entry(target.to_owned())
            .or_insert_with(Breaker::new)
            .allows(now, &self.policy.breaker)
    }

    fn breaker_record(&self, target: &str, outcome: Outcome, now: Instant) {
        let Ok(mut breakers) = self.breakers.lock() else {
            return;
        };
        breakers
            .entry(target.to_owned())
            .or_insert_with(Breaker::new)
            .record(outcome, now, &self.policy.breaker);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inference::model::{CompileReport, Operation};
    use crate::inference::request::{GenerateResponse, TextIntent};
    use crate::inference::usage::Usage;
    use async_trait::async_trait;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU32, Ordering};

    /// A provider that answers per model: `Ok` for a listed model, `Err` otherwise.
    struct Stub {
        healthy: Vec<String>,
        /// The kind the stub fails with. A kind rather than an `Error`: `Error` is not
        /// `Clone` (it carries a boxed source), and a stub that had to clone one would be
        /// testing the stub.
        failure: ErrorKind,
        calls: AtomicU32,
    }

    fn provider(healthy: &[&str], failure: ErrorKind) -> Arc<Stub> {
        Arc::new(Stub {
            healthy: healthy.iter().map(|name| (*name).to_owned()).collect(),
            failure,
            calls: AtomicU32::new(0),
        })
    }

    #[async_trait]
    impl Generator for Stub {
        async fn generate(&self, request: GenerateRequest) -> Result<Generation> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            if self.healthy.contains(&request.model) {
                return Ok(Generation {
                    response: GenerateResponse {
                        parts: Vec::new(),
                        finish: crate::inference::request::FinishReason::Stop,
                        usage: Usage {
                            input_tokens: 1,
                            output_tokens: 1,
                            total_tokens: 2,
                            ..Usage::default()
                        },
                    },
                    report: CompileReport::new(Operation::Generate, Vec::new()),
                });
            }
            Err(Error::new(self.failure, "the stub refused"))
        }
    }

    fn request() -> GenerateRequest {
        GenerateRequest {
            model: String::new(),
            messages: Vec::new(),
            intent: TextIntent::default(),
        }
    }

    fn route(targets: &[&str], retry: RetryPolicy) -> Route {
        let mut tiers = BTreeMap::new();
        tiers.insert(
            "standard".to_owned(),
            targets
                .iter()
                .map(|model| Target {
                    model: (*model).to_owned(),
                })
                .collect(),
        );
        Route::with_clock(
            Policy {
                tiers,
                retry,
                ..Policy::default()
            },
            Box::new(Instant::now),
        )
    }

    #[tokio::test]
    async fn a_transient_failure_falls_back_to_the_next_target() {
        let provider = provider(&["b"], ErrorKind::NotAvailable);
        let route = route(&["a", "b"], RetryPolicy::default());
        let mut ledger = Ledger::new();
        let generation = route
            .generate(
                provider.as_ref(),
                Choice::Tier("standard".to_owned()),
                request(),
                &mut ledger,
            )
            .await
            .expect("the second target answers");
        assert_eq!(generation.response.usage.total_tokens, 2);
        let seen: Vec<(String, u64, crate::inference::ledger::Trigger, Outcome)> = ledger
            .attempts
            .iter()
            .map(|attempt| {
                (
                    attempt.target.clone(),
                    attempt.number,
                    attempt.trigger,
                    attempt.outcome,
                )
            })
            .collect();
        use crate::inference::ledger::Trigger;
        assert_eq!(
            seen,
            vec![
                ("a".to_owned(), 1, Trigger::Selection, Outcome::Failed),
                ("b".to_owned(), 1, Trigger::Fallback, Outcome::Succeeded),
            ]
        );
        assert_eq!(
            ledger.usage.total_tokens, 2,
            "the success's usage is folded in"
        );
    }

    /// A failure the caller has to fix is not retried on the same target — but it does move
    /// to the next one, because a different model may be able to express the request. That is
    /// the Go runtime's pre-resilience fallback boundary, and it is why `max_attempts` is a
    /// per-target budget rather than a per-call one.
    #[tokio::test]
    async fn a_failure_that_cannot_be_retried_is_not_retried_on_the_same_target() {
        let provider = provider(&[], ErrorKind::Validation);
        let route = route(
            &["a", "b"],
            RetryPolicy {
                max_attempts: 3,
                ..RetryPolicy::default()
            },
        );
        let mut ledger = Ledger::new();
        let error = route
            .generate(
                provider.as_ref(),
                Choice::Tier("standard".to_owned()),
                request(),
                &mut ledger,
            )
            .await
            .expect_err("refused");
        assert_eq!(error.kind(), ErrorKind::Validation);
        assert_eq!(
            provider.calls.load(Ordering::SeqCst),
            2,
            "one attempt per target, not three per target"
        );
        use crate::inference::ledger::Trigger;
        let triggers: Vec<Trigger> = ledger.attempts.iter().map(|a| a.trigger).collect();
        assert_eq!(triggers, vec![Trigger::Selection, Trigger::Fallback]);
    }

    #[tokio::test]
    async fn a_transient_failure_spends_its_retry_budget_then_moves_on_when_allowed() {
        let provider = provider(&[], ErrorKind::NotAvailable);
        let route = route(
            &["a"],
            RetryPolicy {
                max_attempts: 3,
                fallback_on_retry_exhausted: true,
                seed: Duration::from_millis(1),
                cap: Duration::from_millis(2),
                ..RetryPolicy::default()
            },
        );
        let mut ledger = Ledger::new();
        route
            .generate(
                provider.as_ref(),
                Choice::Tier("standard".to_owned()),
                request(),
                &mut ledger,
            )
            .await
            .expect_err("every attempt fails");
        assert_eq!(provider.calls.load(Ordering::SeqCst), 3);
        use crate::inference::ledger::Trigger;
        assert_eq!(ledger.attempts.len(), 3);
        assert_eq!(ledger.attempts[0].trigger, Trigger::Selection);
        assert_eq!(ledger.attempts[1].trigger, Trigger::Retry);
        assert_eq!(ledger.attempts[2].trigger, Trigger::Retry);
        // The backoff is recorded, and it doubles (capped at 2ms here).
        assert_eq!(ledger.attempts[1].backoff_millis, 2);
        assert_eq!(ledger.attempts[2].backoff_millis, 2);
    }

    /// A breaker is per target: the dead one is skipped, and the healthy one still answers.
    /// A breaker is per target: the dead one is skipped, and the healthy one still answers.
    #[tokio::test]
    async fn an_open_breaker_skips_its_target_and_the_next_one_answers() {
        let provider = provider(&["b"], ErrorKind::NotAvailable);
        let mut tiers = BTreeMap::new();
        tiers.insert(
            "standard".to_owned(),
            ["a", "b"]
                .iter()
                .map(|model| Target {
                    model: (*model).to_owned(),
                })
                .collect(),
        );
        // Two failures trip `a`, so the second call never calls it again.
        let route = Route::with_clock(
            Policy {
                tiers,
                retry: RetryPolicy {
                    max_attempts: 2,
                    fallback_on_retry_exhausted: true,
                    seed: Duration::from_millis(1),
                    cap: Duration::from_millis(1),
                    ..RetryPolicy::default()
                },
                breaker: BreakerPolicy {
                    threshold: 2,
                    cooldown: Duration::from_secs(30),
                },
            },
            Box::new(Instant::now),
        );
        let mut ledger = Ledger::new();
        route
            .generate(
                provider.as_ref(),
                Choice::Tier("standard".to_owned()),
                request(),
                &mut ledger,
            )
            .await
            .expect("b answers");
        let after_first = provider.calls.load(Ordering::SeqCst);

        // Second call: `a` is skipped without a call, so the count grows by exactly one.
        let mut ledger = Ledger::new();
        route
            .generate(
                provider.as_ref(),
                Choice::Tier("standard".to_owned()),
                request(),
                &mut ledger,
            )
            .await
            .expect("b answers");
        assert_eq!(
            provider.calls.load(Ordering::SeqCst),
            after_first + 1,
            "the tripped target must not be called"
        );
        assert_eq!(ledger.attempts[0].outcome, Outcome::Opened);
        assert_eq!(ledger.attempts[0].phase, Phase::Open);
    }

    #[tokio::test]
    async fn a_hint_moves_a_target_to_the_front_and_an_unknown_one_is_ignored() {
        let route = route(&["a", "b"], RetryPolicy::default());
        let ordered = route
            .resolve(&Choice::Hint {
                hint: "b".to_owned(),
                tier: "standard".to_owned(),
            })
            .expect("hint");
        assert_eq!(ordered[0].model, "b");
        let ignored = route
            .resolve(&Choice::Hint {
                hint: "zzz".to_owned(),
                tier: "standard".to_owned(),
            })
            .expect("unknown hint");
        assert_eq!(
            ignored[0].model, "a",
            "an unknown hint keeps the tier's order"
        );
        assert!(route.resolve(&Choice::Tier("missing".to_owned())).is_err());
    }
}
