//! Escalation: one approved retry *outside* the confine.
//!
//! Approval answers "may this run"; it never answers "under which boundary", so a
//! command the approver allowed and the OS sandbox then refused fails like any
//! other command. Escalation is the second, explicit question: the user is asked
//! whether this one command may leave the confine.
//!
//! Four rules shape the behaviour:
//!
//! - **Detection is a tripwire.** [`denial`] matches the fragments the confinement
//!   layers leave behind. A false positive only produces a prompt the user can
//!   decline; the OS backend stays the wall.
//! - **Fail closed.** A prompt that errors, a missing escalator, or a missing gate
//!   keeps the confined result.
//! - **The retry chain stands on its own.** `unconfined` is *not* "the confined
//!   chain minus the wrapper": nothing merged into `confined` — defaults, command
//!   gates, resource caps — applies to it. Build it with whatever floor must
//!   survive an escalation.
//! - **It is not a `Runner`.** Only a completed result tells a refusal from an
//!   ordinary failure, so the retry lives at the one-shot level and interactive
//!   sessions are never restarted behind the user's back.

use crate::error::Result;
use async_trait::async_trait;

use super::RunnerRef;
use super::allowlist::normalise_argv;
use super::approval::ExecRequest;
use crate::caps::sandbox::contract::SessionSpec;
use crate::caps::sandbox::exec::{ExecResult, exec};

/// Fragments the confinement layers leave behind when they refuse.
///
/// Plain `Permission denied` is deliberately absent: it is an ordinary file-mode
/// or lookup failure, and prompting for it would train users to approve noise.
/// So are the cannot-execute statuses 126/127.
const FRAGMENTS: &[&str] = &[
    "operation not permitted",
    "read-only file system",
    "access is denied",
    // PowerShell / .NET phrasings. Note the missing spaces: `permissiondenied`
    // is not the plain "Permission denied", which stays ignored on purpose.
    "access to the path",
    "unauthorizedaccessexception",
    "permissiondenied",
    "sandbox:",
];

/// How a command ended up running.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    /// It ran inside the confine (the normal case, including a refusal the user
    /// declined to escalate).
    Confined,
    /// The confine refused, the user approved, and it re-ran unconfined once.
    Escalated,
    /// A stored rule sent it straight to the unconfined chain.
    ///
    /// (The Go side calls these three `Refused` / `Approved` / `Remembered`;
    /// `Confined` is the same value as its `Refused`, renamed because "refused"
    /// described what happened to the *sandbox*, not to the command.)
    Remembered,
}

/// The user's answer to one [`EscalationRequest`].
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct EscalationDecision {
    /// Whether the command may run outside the confine this once.
    pub allow: bool,
    /// Whether to persist an "always run this without the sandbox" rule.
    pub remember: bool,
}

/// What the user is asked.
#[derive(Debug, Clone)]
pub struct EscalationRequest {
    /// The display line (argv re-quoted the way a shell reads it back).
    pub command: String,
    /// The normalised token prefix a "remember" answer persists.
    pub rule: String,
    /// The refusal fragment that triggered the question.
    pub reason: String,
    /// The output line the fragment came from.
    pub detail: String,
    /// The refused attempt, as a snapshot.
    pub exec: ExecRequest,
}

/// Asks the user whether one refused command may leave the confine.
#[async_trait]
pub trait Escalator: Send + Sync {
    /// Ask. An error keeps the confined result.
    async fn escalate(&self, request: EscalationRequest) -> Result<EscalationDecision>;
}

/// The read side of the persisted escalation rules.
pub trait EscalationRules: Send + Sync {
    /// Whether a stored rule covers `rule`.
    fn escalated_allowed(&self, rule: &str) -> bool;
}

/// Owns the meaning of "escalation is offerable here".
///
/// A session that already runs unconfined has no confine to leave, and a
/// read-only session must not trade its guarantee for a per-command approval.
pub trait EscalationGate: Send + Sync {
    /// Whether an escalation may be offered at all.
    fn escalation_available(&self) -> bool;
}

/// One recognised refusal.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Denial {
    /// The matched fragment.
    pub fragment: String,
    /// The (trimmed, bounded) output line it came from.
    pub excerpt: String,
}

/// Recognise a sandbox refusal in captured output.
///
/// Both streams are searched: the shells that phrase a refusal disagree about
/// which one they use.
#[must_use]
pub fn denial(result: &ExecResult) -> Option<Denial> {
    // A command that succeeded was not refused: escalating it would re-run it, and
    // its side effects would happen twice — outside the confine.
    if result.exit_code == 0 || result.exit_code == 126 || result.exit_code == 127 {
        return None;
    }
    for fragment in FRAGMENTS {
        for stream in [&result.stdout, &result.stderr] {
            if let Some(index) = find_ci(stream, fragment) {
                return Some(Denial {
                    fragment: (*fragment).trim().to_owned(),
                    excerpt: excerpt(stream, index),
                });
            }
        }
    }
    None
}

/// Case-insensitive search returning a byte offset into `haystack`.
///
/// The fragments are ASCII, so a byte-window comparison never splits a character:
/// a multi-byte character can never equal an ASCII byte sequence.
fn find_ci(haystack: &str, needle: &str) -> Option<usize> {
    let needle = needle.as_bytes();
    if needle.is_empty() || haystack.len() < needle.len() {
        return None;
    }
    haystack
        .as_bytes()
        .windows(needle.len())
        .position(|window| window.eq_ignore_ascii_case(needle))
}

/// The line around `index`, trimmed and bounded.
fn excerpt(stream: &str, index: usize) -> String {
    let start = stream[..index].rfind('\n').map_or(0, |at| at + 1);
    let end = stream[index..]
        .find('\n')
        .map_or(stream.len(), |at| index + at);
    let line = stream[start..end].trim();
    let mut bounded: String = line.chars().take(200).collect();
    if line.chars().count() > 200 {
        bounded.push('…');
    }
    bounded
}

/// The persisted key for a command: the normalised token prefix.
#[must_use]
pub fn rule_for(tokens: &[String]) -> String {
    tokens.join(" ")
}

/// The retry front end.
pub struct Escalation {
    /// The chain that runs inside the confine.
    pub confined: RunnerRef,
    /// The chain an approved retry uses. Independent of `confined`.
    pub unconfined: RunnerRef,
    /// Who to ask.
    pub escalator: Option<std::sync::Arc<dyn Escalator>>,
    /// Remembered rules, consulted before the first attempt.
    pub rules: Option<std::sync::Arc<dyn EscalationRules>>,
    /// The mode owner's half: is a retry offerable here?
    pub gate: Option<std::sync::Arc<dyn EscalationGate>>,
}

impl std::fmt::Debug for Escalation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.Escalation")
            .field("confined", &self.confined)
            .field("unconfined", &self.unconfined)
            .finish_non_exhaustive()
    }
}

impl Escalation {
    /// A front end with no escalator, no rules and no gate: a refusal stays a
    /// plain command failure.
    #[must_use]
    pub fn new(confined: RunnerRef, unconfined: RunnerRef) -> Self {
        Self {
            confined,
            unconfined,
            escalator: None,
            rules: None,
            gate: None,
        }
    }

    /// Whether an escalation may be offered here.
    ///
    /// A missing gate offers nothing: escalation is opt-in, and a deployment that
    /// never configured it must not start prompting.
    #[must_use]
    pub fn available(&self) -> bool {
        self.gate
            .as_ref()
            .is_some_and(|gate| gate.escalation_available())
    }

    /// Run `spec` through the confine, escalating once if it is refused and the
    /// user approves.
    pub async fn exec(&self, spec: SessionSpec) -> Result<(ExecResult, Outcome)> {
        let request = ExecRequest::from_spec(&spec);
        let rule = rule_for(&normalise_argv(&request.argv()));

        // A remembered rule skips the doomed first attempt entirely.
        if self.available()
            && let Some(rules) = &self.rules
            && rules.escalated_allowed(&rule)
        {
            let result = exec(&*self.unconfined, spec).await?;
            return Ok((result, Outcome::Remembered));
        }

        let result = exec(&*self.confined, spec.clone()).await?;
        let Some(denial) = denial(&result) else {
            return Ok((result, Outcome::Confined));
        };
        if !self.available() {
            return Ok((result, Outcome::Confined));
        }
        let Some(escalator) = &self.escalator else {
            return Ok((result, Outcome::Confined));
        };
        let answer = escalator
            .escalate(EscalationRequest {
                command: request.display(),
                rule,
                reason: denial.fragment,
                detail: denial.excerpt,
                exec: request,
            })
            .await;
        match answer {
            Ok(decision) if decision.allow => {
                // Byte-identical retry: the snapshot above cannot have changed it.
                let retried = exec(&*self.unconfined, spec).await?;
                Ok((retried, Outcome::Escalated))
            }
            // A refusal, an error, or a timeout keeps the confined result.
            _ => Ok((result, Outcome::Confined)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::confinement::{ConfinedRunner, Sandbox, SandboxConfig};
    use crate::caps::sandbox::local::LocalRunner;
    use std::sync::Arc;

    fn result(stdout: &str, stderr: &str, code: i32) -> ExecResult {
        ExecResult {
            exit_code: code,
            stdout: stdout.to_owned(),
            stderr: stderr.to_owned(),
        }
    }

    struct Answer(EscalationDecision);
    #[async_trait]
    impl Escalator for Answer {
        async fn escalate(&self, _request: EscalationRequest) -> Result<EscalationDecision> {
            Ok(self.0)
        }
    }

    struct Gate(bool);
    impl EscalationGate for Gate {
        fn escalation_available(&self) -> bool {
            self.0
        }
    }

    struct Rules(Vec<String>);
    impl EscalationRules for Rules {
        fn escalated_allowed(&self, rule: &str) -> bool {
            self.0.iter().any(|stored| stored == rule)
        }
    }

    #[test]
    fn a_refusal_is_recognised_in_either_stream() {
        assert!(denial(&result("", "bwrap: Read-only file system", 1)).is_some());
        assert!(denial(&result("Operation not permitted", "", 1)).is_some());
        assert!(denial(&result("", "Sandbox: sh(1) deny file-write", 1)).is_some());
        // Case-insensitive, and the Windows / PowerShell spellings.
        assert!(denial(&result("", "operation not permitted", 1)).is_some());
        assert!(denial(&result("", "UnauthorizedAccessException: nope", 1)).is_some());
        assert!(denial(&result("", "Access to the path '/x' is denied.", 1)).is_some());
    }

    #[test]
    fn a_command_that_succeeded_was_not_refused() {
        // Re-running it would double its side effects, outside the confine.
        assert!(denial(&result("Sandbox: hello", "", 0)).is_none());
        // 126/127 are cannot-execute statuses, not sandbox refusals.
        assert!(denial(&result("", "bwrap: Read-only file system", 126)).is_none());
        assert!(denial(&result("", "Operation not permitted", 127)).is_none());
    }

    #[test]
    fn an_ordinary_failure_is_not_a_refusal() {
        // A plain permission error is a file-mode problem, not a boundary.
        assert!(denial(&result("", "cat: f: Permission denied", 1)).is_none());
        assert!(denial(&result("", "ls: f: No such file or directory", 2)).is_none());
        assert!(denial(&result("", "", 127)).is_none());
    }

    #[test]
    fn the_excerpt_is_the_offending_line() {
        let output = result("", "line one\nbwrap: Read-only file system\nline three", 1);
        let denial = denial(&output).expect("denial");
        assert_eq!(denial.excerpt, "bwrap: Read-only file system");
    }

    #[test]
    fn the_rule_is_the_normalised_prefix() {
        let tokens =
            normalise_argv(&["sh".to_owned(), "-c".to_owned(), "pip install x".to_owned()]);
        assert_eq!(rule_for(&tokens), "pip install x");
    }

    #[tokio::test]
    async fn without_a_gate_nothing_is_offered() {
        let front = Escalation::new(Arc::new(LocalRunner::new()), Arc::new(LocalRunner::new()));
        assert!(!front.available(), "escalation is opt-in");
        let (result, outcome) = front
            .exec(SessionSpec::new(vec!["true".to_owned()]))
            .await
            .expect("run");
        assert_eq!(outcome, Outcome::Confined);
        assert_eq!(result.exit_code, 0);
    }

    #[tokio::test]
    async fn a_remembered_rule_skips_the_confined_attempt() {
        // The confined chain would refuse (`false`), the unconfined one succeeds:
        // seeing success proves the first attempt never ran.
        let confined = Arc::new(LocalRunner::new()) as RunnerRef;
        let unconfined = Arc::new(LocalRunner::new()) as RunnerRef;
        let mut front = Escalation::new(confined, unconfined);
        front.gate = Some(Arc::new(Gate(true)));
        front.rules = Some(Arc::new(Rules(vec!["true".to_owned()])));
        let (result, outcome) = front
            .exec(SessionSpec::new(vec!["true".to_owned()]))
            .await
            .expect("run");
        assert_eq!(outcome, Outcome::Remembered);
        assert_eq!(result.exit_code, 0);
    }

    #[tokio::test]
    async fn a_gate_that_says_no_suppresses_the_shortcut() {
        let confined = Arc::new(LocalRunner::new()) as RunnerRef;
        let unconfined = Arc::new(LocalRunner::new()) as RunnerRef;
        let mut front = Escalation::new(confined, unconfined);
        front.gate = Some(Arc::new(Gate(false)));
        front.rules = Some(Arc::new(Rules(vec!["true".to_owned()])));
        let (_, outcome) = front
            .exec(SessionSpec::new(vec!["true".to_owned()]))
            .await
            .expect("run");
        assert_eq!(outcome, Outcome::Confined);
    }

    #[tokio::test]
    async fn a_real_seatbelt_refusal_escalates_once() {
        if !std::path::Path::new("/usr/bin/sandbox-exec").exists() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root = dir.path().join("root");
        std::fs::create_dir(&root).expect("root");
        let outside = dir.path().join("outside.txt");
        let confined = Arc::new(ConfinedRunner::new(SandboxConfig::new(
            Sandbox::Seatbelt,
            &root,
        ))) as RunnerRef;
        let unconfined = Arc::new(LocalRunner::new()) as RunnerRef;

        let mut front = Escalation::new(confined, unconfined);
        front.gate = Some(Arc::new(Gate(true)));
        front.escalator = Some(Arc::new(Answer(EscalationDecision {
            allow: true,
            remember: false,
        })));
        let spec = SessionSpec::new(vec![
            "sh".to_owned(),
            "-c".to_owned(),
            format!("echo x > {}", outside.display()),
        ]);
        let (result, outcome) = front.exec(spec).await.expect("run");
        assert_eq!(outcome, Outcome::Escalated, "{result:?}");
        assert_eq!(result.exit_code, 0, "the retry ran unconfined");
        assert!(outside.exists(), "the retry really wrote the file");
    }

    #[tokio::test]
    async fn a_declined_prompt_keeps_the_confined_result() {
        if !std::path::Path::new("/usr/bin/sandbox-exec").exists() {
            return;
        }
        let dir = tempfile::tempdir().expect("tempdir");
        let root = dir.path().join("root");
        std::fs::create_dir(&root).expect("root");
        let outside = dir.path().join("denied.txt");
        let confined = Arc::new(ConfinedRunner::new(SandboxConfig::new(
            Sandbox::Seatbelt,
            &root,
        ))) as RunnerRef;
        let unconfined = Arc::new(LocalRunner::new()) as RunnerRef;
        let mut front = Escalation::new(confined, unconfined);
        front.gate = Some(Arc::new(Gate(true)));
        front.escalator = Some(Arc::new(Answer(EscalationDecision {
            allow: false,
            remember: false,
        })));
        let spec = SessionSpec::new(vec![
            "sh".to_owned(),
            "-c".to_owned(),
            format!("echo x > {}", outside.display()),
        ]);
        let (result, outcome) = front.exec(spec).await.expect("run");
        assert_eq!(outcome, Outcome::Confined);
        assert_ne!(result.exit_code, 0);
        assert!(!outside.exists(), "the declined retry must not run");
    }
}
