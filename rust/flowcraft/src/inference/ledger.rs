//! The attempt ledger: what happened, in order, with the numbers attached.
//!
//! Ported from `core/inference/route/router.go`'s `Attempt` and the usage folding in
//! `core/inference/usage/`. A routed call is not one thing — it is a selection, then
//! one attempt per candidate, then a retry or a fallback — and the only honest way to
//! report that is a list of attempts plus a sum.
//!
//! Two rules make the sum usable, and both came from the Go side:
//!
//! - **usage accumulates across every attempt that produced tokens.** A failed
//!   attempt that already streamed a partial answer still cost money; dropping it from
//!   the total under-reports the bill, which is the direction nobody forgives.
//! - **an unreported counter stays unreported** (see [`super::usage::Usage::add`]).
//!   A budget that charges for a silence is worse than one that knows it does not know.

use std::time::Duration;

use serde::{Deserialize, Serialize};

use super::usage::Usage;

/// Where in the pipeline an attempt was made.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    /// Compiling and checking, before anything is sent.
    Preflight,
    /// The provider call itself.
    Execute,
    /// A circuit breaker refused before the call happened.
    Open,
}

/// What caused this attempt.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Trigger {
    /// The first attempt for the chosen target.
    Selection,
    /// A retry of the same target.
    Retry,
    /// A hop to the next target, after this one failed.
    Fallback,
}

/// How an attempt ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    /// It produced an answer.
    Succeeded,
    /// It did not.
    Failed,
    /// The breaker opened, so nothing was sent.
    Opened,
    /// It was not tried (the budget was spent, or the target was already known bad).
    Skipped,
}

/// One attempt.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Attempt {
    /// The target it was made against.
    pub target: String,
    /// Where in the pipeline.
    pub phase: Phase,
    /// What caused it.
    pub trigger: Trigger,
    /// How it ended.
    pub outcome: Outcome,
    /// The canonical error kind, when it failed. A string here rather than an enum
    /// because the kind vocabulary is the error taxonomy's, and this record travels
    /// with a report that may be read by a version that does not know every kind.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error_kind: Option<String>,
    /// Whether the caller could observe output: a failed attempt that streamed is not
    /// retried, because the answer was already partly delivered.
    #[serde(default)]
    pub observable_output: bool,
    /// 1-based attempt number within one target: the initial attempt is 1.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub number: u64,
    /// The delay slept before this attempt, in milliseconds. Zero for the first.
    #[serde(default, skip_serializing_if = "is_zero")]
    pub backoff_millis: u64,
}

fn is_zero(value: &u64) -> bool {
    *value == 0
}

/// The record of one routed call.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Ledger {
    /// Every attempt, in the order they happened.
    pub attempts: Vec<Attempt>,
    /// The sum of what the attempts cost.
    pub usage: Usage,
}

impl Ledger {
    /// An empty ledger.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Record an attempt, and the usage it produced.
    ///
    /// `usage` is added even for a failed attempt: tokens that were generated were
    /// billed, whatever the caller got to see.
    pub fn record(&mut self, attempt: Attempt, usage: Option<&Usage>) {
        if let Some(usage) = usage {
            self.usage = self.usage.add(usage);
        }
        self.attempts.push(attempt);
    }

    /// Begin an attempt, returning its index in [`Self::attempts`].
    ///
    /// Three things are derived here rather than left to the caller, because each is easy to
    /// get wrong once routing exists:
    ///
    /// - **`number` counts attempts within one target.** A global counter makes a fallback
    ///   look like the Nth retry of a target that was tried once, and the per-target backoff
    ///   history becomes unreadable.
    /// - **`trigger` follows from the target.** The first attempt for a target is
    ///   `Selection`; another attempt against the *same* target is `Retry`; a different
    ///   target is `Fallback`. The rule is stated once here instead of in every policy.
    /// - **the phase is the caller's**, because only the policy knows whether it is
    ///   preflighting, executing, or reporting a breaker that refused to open a connection.
    pub fn begin(&mut self, target: &str, phase: Phase, backoff: Duration) -> usize {
        let previous = self.attempts.last().map(|attempt| attempt.target.as_str());
        let trigger = match previous {
            None => Trigger::Selection,
            Some(previous) if previous == target => Trigger::Retry,
            Some(_) => Trigger::Fallback,
        };
        let number = self
            .attempts
            .iter()
            .filter(|attempt| attempt.target == target)
            .count() as u64
            + 1;
        self.attempts.push(Attempt {
            target: target.to_owned(),
            phase,
            trigger,
            // Filled in by `complete`: an attempt that has begun has not ended.
            outcome: Outcome::Skipped,
            error_kind: None,
            observable_output: false,
            number,
            backoff_millis: u64::try_from(backoff.as_millis()).unwrap_or(u64::MAX),
        });
        self.attempts.len() - 1
    }

    /// Close the attempt at `index`, folding in what it cost.
    ///
    /// Returns `false` when the index is not an attempt — a ledger is diagnostics, so a
    /// stale index is ignored rather than a panic in the middle of a provider call.
    pub fn complete(
        &mut self,
        index: usize,
        outcome: Outcome,
        error_kind: Option<&str>,
        usage: Option<&Usage>,
        observable_output: bool,
    ) -> bool {
        let Some(attempt) = self.attempts.get_mut(index) else {
            return false;
        };
        attempt.outcome = outcome;
        attempt.error_kind = error_kind.map(str::to_owned);
        attempt.observable_output = observable_output;
        if let Some(usage) = usage {
            // Tokens that were generated were billed, whatever the caller got to see.
            self.usage = self.usage.add(usage);
        }
        true
    }

    /// Whether any attempt succeeded.
    #[must_use]
    pub fn succeeded(&self) -> bool {
        self.attempts
            .iter()
            .any(|attempt| attempt.outcome == Outcome::Succeeded)
    }

    /// How many targets were tried.
    #[must_use]
    pub fn targets_tried(&self) -> usize {
        let mut names: Vec<&str> = self.attempts.iter().map(|a| a.target.as_str()).collect();
        names.sort_unstable();
        names.dedup();
        names.len()
    }

    /// Whether an attempt already delivered output the caller could see.
    ///
    /// The one question a retry policy must never get wrong: re-running a call that
    /// streamed part of an answer duplicates the output, and the design notes in
    /// `core/inference/errors.go` say so twice for `ProviderTruncated` and
    /// `ProviderResponseUnusable`.
    #[must_use]
    pub fn observed_output(&self) -> bool {
        self.attempts
            .iter()
            .any(|attempt| attempt.observable_output)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn attempt(target: &str, trigger: Trigger, outcome: Outcome) -> Attempt {
        Attempt {
            target: target.to_owned(),
            phase: Phase::Execute,
            trigger,
            outcome,
            error_kind: None,
            observable_output: false,
            number: 1,
            backoff_millis: 0,
        }
    }

    /// A failed attempt that produced tokens is still on the bill.
    #[test]
    fn usage_accumulates_across_attempts_including_failures() {
        let mut ledger = Ledger::new();
        ledger.record(
            attempt("a", Trigger::Selection, Outcome::Failed),
            Some(&Usage {
                input_tokens: 10,
                output_tokens: 0,
                total_tokens: 10,
                ..Usage::default()
            }),
        );
        ledger.record(
            attempt("b", Trigger::Fallback, Outcome::Succeeded),
            Some(&Usage {
                input_tokens: 10,
                output_tokens: 4,
                total_tokens: 14,
                reasoning_tokens: Some(2),
                ..Usage::default()
            }),
        );
        assert_eq!(ledger.usage.input_tokens, 20);
        assert_eq!(ledger.usage.total_tokens, 24);
        assert_eq!(ledger.usage.reasoning_tokens, Some(2));
        assert_eq!(ledger.targets_tried(), 2);
        assert!(ledger.succeeded());
    }

    /// The numbering is per target: a fallback starts again at 1, so a policy can read a
    /// target's own backoff history without filtering a global sequence.
    #[test]
    fn a_fallback_restarts_the_attempt_number() {
        let mut ledger = Ledger::new();
        let a1 = ledger.begin("a", Phase::Execute, Duration::ZERO);
        ledger.complete(a1, Outcome::Failed, Some("not_available"), None, false);
        let a2 = ledger.begin("a", Phase::Execute, Duration::from_millis(50));
        ledger.complete(a2, Outcome::Failed, Some("not_available"), None, false);
        let b1 = ledger.begin("b", Phase::Execute, Duration::ZERO);
        ledger.complete(b1, Outcome::Succeeded, None, None, false);

        let numbers: Vec<(String, u64, Trigger)> = ledger
            .attempts
            .iter()
            .map(|attempt| (attempt.target.clone(), attempt.number, attempt.trigger))
            .collect();
        assert_eq!(
            numbers,
            vec![
                ("a".to_owned(), 1, Trigger::Selection),
                ("a".to_owned(), 2, Trigger::Retry),
                ("b".to_owned(), 1, Trigger::Fallback),
            ]
        );
    }

    /// Once an attempt delivered output, the ledger says so for the rest of the call: that is
    /// the question a retry policy must never get wrong, and it does not stop being true
    /// because the last attempt happened not to stream.
    #[test]
    fn observable_output_is_sticky_across_attempts() {
        let mut ledger = Ledger::new();
        let first = ledger.begin("a", Phase::Execute, Duration::ZERO);
        ledger.complete(
            first,
            Outcome::Failed,
            Some("timeout"),
            Some(&Usage {
                input_tokens: 4,
                ..Usage::default()
            }),
            true,
        );
        assert!(ledger.observed_output());
        let second = ledger.begin("b", Phase::Execute, Duration::ZERO);
        ledger.complete(second, Outcome::Failed, Some("not_available"), None, false);
        assert!(
            ledger.observed_output(),
            "an earlier streamed attempt still means the caller saw output"
        );
        // And the failed attempt's tokens are on the bill even though it streamed and failed.
        assert_eq!(ledger.usage.input_tokens, 4);
    }

    #[test]
    fn closing_an_attempt_that_does_not_exist_is_ignored() {
        let mut ledger = Ledger::new();
        assert!(!ledger.complete(7, Outcome::Failed, None, None, false));
        assert!(ledger.attempts.is_empty());
    }

    /// The retry question: output the caller could see must stop a re-run.
    #[test]
    fn an_attempt_that_delivered_output_is_visible_to_the_policy() {
        let mut ledger = Ledger::new();
        ledger.record(attempt("a", Trigger::Selection, Outcome::Failed), None);
        assert!(!ledger.observed_output());
        let mut streamed = attempt("a", Trigger::Retry, Outcome::Failed);
        streamed.observable_output = true;
        ledger.record(streamed, None);
        assert!(
            ledger.observed_output(),
            "a streamed attempt must be visible: re-running it duplicates output"
        );
    }

    #[test]
    fn an_attempt_without_a_reported_usage_leaves_the_total_alone() {
        let mut ledger = Ledger::new();
        ledger.record(attempt("a", Trigger::Selection, Outcome::Failed), None);
        assert!(ledger.usage.is_unreported());
        assert!(!ledger.succeeded());
        assert_eq!(ledger.targets_tried(), 1);
    }
}
