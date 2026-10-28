//! Approval: the second question a boundary-crossing call gets.
//!
//! Approvers answer "may this run", never "under which boundary": an approval
//! passes the request through **unchanged**, so approval is never a channel for
//! widening policy. The complementary question — "may this one command leave the
//! confine" — lives in [`super::escalation`].
//!
//! Fail-closed is the default everywhere: a matched predicate with no approver
//! is a denial, a failing approver is a denial, and a poisoned lock approves
//! nothing.

use std::collections::BTreeSet;
use std::path::PathBuf;
use std::sync::Arc;

use crate::error::{Error, Result};
use async_trait::async_trait;

use super::allowlist::{Allowlist, normalise_argv};
use super::plan::{self, HostPolicy, PlannedCommand};
use super::{Layer, RunnerRef, guard};
use crate::caps::sandbox::contract::{ExecOptions, NetMode, SessionSpec, WriteMode};

/// A snapshot of one start request, shared by predicates and approvers.
///
/// A snapshot rather than the live spec, so a predicate can inspect what will
/// run but cannot rewrite it: only the layers may tighten a request.
///
/// The snapshot carries two accounts of the same call, and they are not
/// interchangeable: [`Self::command`] and [`Self::args`] are what the caller
/// *wrote* (display, audit, prompts), while [`Self::planned`] is what will
/// actually be *executed* — the resolved file and the environment it gets. A
/// predicate may only conclude from the plan.
#[derive(Debug, Clone)]
pub struct ExecRequest {
    /// The programme as written.
    pub command: String,
    /// The arguments as written.
    pub args: Vec<String>,
    /// The options that will be applied.
    pub opts: ExecOptions,
    /// Whether the caller asked for a pty.
    pub tty: bool,
    /// Whether the child gets an input side.
    pub stdin: bool,
    /// What the executor will run.
    pub planned: PlannedCommand,
    /// The normalised tokens an allowlist rule matches, computed on first ask.
    ///
    /// Tokenising means shell unwrapping and a split, and it is only ever needed
    /// when a allowlist is configured — computing it eagerly for every start was
    /// work nobody read.
    tokens: std::sync::OnceLock<Vec<String>>,
}

impl ExecRequest {
    /// Snapshot `spec`, resolving `argv[0]` against the default host policy.
    ///
    /// The plan is computed here and nowhere else, so every predicate and
    /// approver of one check judges the same resolution.
    #[must_use]
    #[allow(deprecated)]
    pub fn from_spec(spec: &SessionSpec) -> Self {
        Self::with_host(spec, &HostPolicy::default())
    }

    /// Snapshot `spec`, resolving `argv[0]` against `host`.
    #[must_use]
    #[allow(deprecated)]
    pub fn with_host(spec: &SessionSpec, host: &HostPolicy) -> Self {
        Self {
            command: spec.argv.first().cloned().unwrap_or_default(),
            args: spec.argv.iter().skip(1).cloned().collect(),
            opts: spec.opts.clone(),
            tty: spec.tty,
            stdin: spec.wants_stdin(),
            tokens: std::sync::OnceLock::new(),
            planned: plan::plan(spec, host),
        }
    }

    /// The normalised tokens a rule is matched against.
    ///
    /// Computed once, on the first call, from the argv this request was built from.
    #[must_use]
    pub fn normalised_tokens(&self) -> &[String] {
        self.tokens
            .get_or_init(|| normalise_argv(&self.planned.argv))
    }

    /// The full command line.
    #[must_use]
    pub fn argv(&self) -> Vec<String> {
        let mut argv = Vec::with_capacity(self.args.len() + 1);
        argv.push(self.command.clone());
        argv.extend(self.args.iter().cloned());
        argv
    }

    /// A one-line rendering for a prompt.
    #[must_use]
    pub fn display(&self) -> String {
        self.argv()
            .iter()
            .map(|token| quote(token))
            .collect::<Vec<_>>()
            .join(" ")
    }
}

/// Quote a token the way a POSIX shell reads it back, for display only.
fn quote(token: &str) -> String {
    let plain = !token.is_empty()
        && token
            .chars()
            .all(|ch| ch.is_ascii_alphanumeric() || "_-./:=@+,%^".contains(ch));
    if plain {
        return token.to_owned();
    }
    format!("'{}'", token.replace('\'', "'\\''"))
}

/// A tripwire: does this call cross a boundary?
pub trait Predicate: Send + Sync {
    /// `Some(reason)` when it does. The reason is shown to the approver and, if
    /// denied, ends up in the error message.
    fn matches(&self, request: &ExecRequest) -> Option<String>;
}

/// The approver's verdict.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Decision {
    /// Let it run with exactly the options it asked for.
    Allow,
    /// Refuse it.
    Deny,
}

/// What the approver is shown.
#[derive(Debug, Clone)]
pub struct ApprovalRequest {
    /// The call that wants to cross a boundary.
    pub exec: ExecRequest,
    /// Why the first matching predicate flagged it.
    pub reason: String,
}

/// Decides a boundary-crossing call.
#[async_trait]
pub trait Approver: Send + Sync {
    /// Approve or refuse. An error is a refusal.
    async fn approve(&self, request: ApprovalRequest) -> Result<Decision>;
}

/// A synchronous approver, for hosts with a blocking prompt.
pub struct ApproverFn<F>(F);

impl<F> ApproverFn<F> {
    /// Wrap a closure.
    pub fn new(approve: F) -> Self {
        Self(approve)
    }
}

#[async_trait]
impl<F> Approver for ApproverFn<F>
where
    F: Fn(&ApprovalRequest) -> Decision + Send + Sync,
{
    async fn approve(&self, request: ApprovalRequest) -> Result<Decision> {
        Ok((self.0)(&request))
    }
}

// ---------------------------------------------------------------------------
// Built-in predicates
// ---------------------------------------------------------------------------

/// Ask when the working directory resolves outside `root`.
#[must_use]
pub fn workdir_outside_root(root: impl Into<PathBuf>) -> Arc<dyn Predicate> {
    Arc::new(WorkdirOutsideRoot { root: root.into() })
}

struct WorkdirOutsideRoot {
    root: PathBuf,
}

impl Predicate for WorkdirOutsideRoot {
    fn matches(&self, request: &ExecRequest) -> Option<String> {
        let dir = request.opts.workdir.as_ref()?;
        // A relative workdir is resolved against the runner root, so it can never
        // escape it: only an absolute one is worth asking about.
        if !dir.is_absolute() {
            return None;
        }
        // Lexical normalisation, so `/root/../etc` is flagged rather than read as
        // "starts with /root".
        let cleaned = crate::caps::sandbox::confinement::path::normalize(dir);
        if cleaned.starts_with(&self.root) {
            None
        } else {
            Some(format!(
                "workdir {} is outside {}",
                cleaned.display(),
                self.root.display()
            ))
        }
    }
}

/// Match a `*`/`?` glob against `text` (the Go `filepath.Match` subset the
/// predicate has always promised).
fn glob_match(pattern: &str, text: &str) -> bool {
    let pattern: Vec<char> = pattern.chars().collect();
    let text: Vec<char> = text.chars().collect();
    let (mut p, mut t) = (0usize, 0usize);
    let (mut star, mut resume) = (None::<usize>, 0usize);
    while t < text.len() {
        match pattern.get(p) {
            Some('*') => {
                star = Some(p);
                resume = t;
                p += 1;
            }
            Some('?') => {
                p += 1;
                t += 1;
            }
            Some(ch) if *ch == text[t] => {
                p += 1;
                t += 1;
            }
            _ => match star {
                Some(at) => {
                    p = at + 1;
                    resume += 1;
                    t = resume;
                }
                None => return false,
            },
        }
    }
    pattern[p..].iter().all(|ch| *ch == '*')
}

/// Ask for any network posture other than the host default.
#[must_use]
pub fn net_non_default() -> Arc<dyn Predicate> {
    struct Predicate_;
    impl Predicate for Predicate_ {
        fn matches(&self, request: &ExecRequest) -> Option<String> {
            let mode = request.opts.net.mode;
            (mode != NetMode::Default).then(|| format!("net mode {}", mode.as_str()))
        }
    }
    Arc::new(Predicate_)
}

/// Ask when the command's base name matches one of `patterns`.
pub fn command_patterns(patterns: impl IntoIterator<Item = String>) -> Result<Arc<dyn Predicate>> {
    let mut names = BTreeSet::new();
    for pattern in patterns {
        let pattern = pattern.trim().to_owned();
        if pattern.is_empty() {
            return Err(Error::validation(
                "sandbox: a sensitive command pattern is empty",
            ));
        }
        if pattern.chars().any(char::is_control) {
            return Err(Error::validation(
                "sandbox: a sensitive command pattern contains a control character",
            ));
        }
        names.insert(pattern);
    }
    Ok(Arc::new(Patterns { names }))
}

struct Patterns {
    names: BTreeSet<String>,
}

impl Predicate for Patterns {
    fn matches(&self, request: &ExecRequest) -> Option<String> {
        let base = std::path::Path::new(&request.command)
            .file_name()
            .and_then(|name| name.to_str())
            .unwrap_or(&request.command);
        if self
            .names
            .iter()
            .any(|pattern| glob_match(pattern, base) || glob_match(pattern, &request.command))
        {
            Some(format!("{base} matches a sensitive command pattern"))
        } else {
            None
        }
    }
}

/// Ask for any interactive (pty) start.
#[must_use]
pub fn interactive() -> Arc<dyn Predicate> {
    struct Interactive;
    impl Predicate for Interactive {
        fn matches(&self, request: &ExecRequest) -> Option<String> {
            request
                .tty
                .then(|| "an interactive session is a persistent command channel".to_owned())
        }
    }
    Arc::new(Interactive)
}

// ---------------------------------------------------------------------------
// Layers
// ---------------------------------------------------------------------------

/// Refuse every command the host's own PATH does not resolve to an allowed
/// programme.
///
/// The rule is compared with the **planned** programme
/// ([`PlannedCommand::names`]): a rule containing `/` is a pinned path, a bare
/// rule names a programme the host's PATH resolves, and neither ever looks at
/// the command as the caller spelled it. So `git` approves the host's `git`,
/// while `/tmp/x/git` — a binary the caller dropped somewhere — is refused, even
/// though its base name is `git`.
///
/// It sits closest to the backend: approval cannot bypass it.
#[must_use]
pub fn allow_commands(allowed: impl IntoIterator<Item = String>) -> Layer {
    allow_commands_with_host(allowed, HostPolicy::default())
}

/// [`allow_commands`] for a host whose programme directories are not the
/// default (a deployment with its own `path_dirs`, or a test with a temporary
/// one).
#[must_use]
pub fn allow_commands_with_host(
    allowed: impl IntoIterator<Item = String>,
    host: HostPolicy,
) -> Layer {
    let allowed: BTreeSet<String> = allowed
        .into_iter()
        .map(|name| name.trim().to_owned())
        .filter(|name| !name.is_empty())
        .collect();
    Arc::new(move |inner| {
        let allowed = allowed.clone();
        let host = host.clone();
        guard(
            inner,
            Arc::new(move |spec: SessionSpec| {
                let allowed = allowed.clone();
                let host = host.clone();
                Box::pin(async move {
                    if spec.argv.is_empty() {
                        return Err(Error::validation("sandbox: argv is empty"));
                    }
                    let planned = plan::plan(&spec, &host);
                    let Some(program) = planned.program() else {
                        return Err(Error::policy_denied(format!(
                            "sandbox: {:?} does not resolve to an executable file, so no rule can approve it",
                            planned.argv0
                        )));
                    };
                    if allowed.iter().any(|rule| planned.names(rule)) {
                        return Ok(spec);
                    }
                    Err(Error::policy_denied(format!(
                        "sandbox: {:?} ({}) is not in the command allow-list",
                        planned.argv0,
                        program.display()
                    )))
                })
            }),
        )
    })
}

/// Merge daemon-owned defaults into every request.
///
/// A default can only **tighten** what the caller asked for: a shorter timeout, a
/// stricter write or net mode, a smaller budget, an additional environment
/// allow-list. It never widens anything, and it never rewrites the command.
#[must_use]
pub fn defaults(defaults: ExecOptions) -> Layer {
    Arc::new(move |inner| {
        let defaults = defaults.clone();
        guard(
            inner,
            Arc::new(move |mut spec: SessionSpec| {
                let defaults = defaults.clone();
                Box::pin(async move {
                    tighten(&mut spec.opts, &defaults);
                    Ok(spec)
                })
            }),
        )
    })
}

/// Merge `defaults` into `opts`, keeping the stricter side of every pair.
fn tighten(opts: &mut ExecOptions, defaults: &ExecOptions) {
    if opts.workdir.is_none() {
        opts.workdir = defaults.workdir.clone();
    }
    if opts.stdin.is_none() {
        opts.stdin = defaults.stdin.clone();
    }
    opts.timeout = match (opts.timeout, defaults.timeout) {
        (Some(caller), Some(default)) => Some(caller.min(default)),
        (caller, default) => caller.or(default),
    };
    if opts.write != WriteMode::ReadOnly && defaults.write == WriteMode::ReadOnly {
        opts.write = WriteMode::ReadOnly;
    }
    // The network posture is sandbox-level policy: when the daemon has one it
    // replaces the caller's *entirely* — mode and parameters, so a same-mode
    // caller cannot swap the upstream or the allow-list. A caller may still ask
    // for something stricter than a daemon that expressed no opinion.
    if defaults.net.mode != NetMode::Default {
        opts.net = defaults.net.clone();
    }
    // Caps belong to the daemon: "narrow a cap per call" is not actionable for a
    // local backend, so mixing the two sides would only invent semantics.
    opts.limits = defaults.limits.clone();
    // The host-environment allow-list is daemon policy; a caller that could widen
    // it would defeat the sandbox, so the default wins even when it is `None`.
    opts.env.allow = defaults.env.allow.clone();
    // Injection is the one place a caller may layer per-call context, so the
    // caller wins on a collision (RUN_ID, REQUEST_ID, ...).
    let mut inject = defaults.env.inject.clone();
    inject.extend(opts.env.inject.clone());
    opts.env.inject = inject;
}

/// Ask the approver about any call a predicate flags.
///
/// `allowlist` pre-approves in-bounds calls, so a human is only consulted for the
/// rest. With no approver, a flagged call is refused.
#[must_use]
pub fn approval(
    approver: Option<Arc<dyn Approver>>,
    allowlist: Option<Arc<Allowlist>>,
    predicates: Vec<Arc<dyn Predicate>>,
) -> Layer {
    approval_with_host(HostPolicy::default(), approver, allowlist, predicates)
}

/// [`approval`] over a deployment's programme directories.
///
/// The predicates see a resolution, so the list has to be the runner's: judging
/// with the default PATH while the runner resolves elsewhere is how a rule ends up
/// about a file that never runs.
pub fn approval_with_host(
    host: HostPolicy,
    approver: Option<Arc<dyn Approver>>,
    allowlist: Option<Arc<Allowlist>>,
    predicates: Vec<Arc<dyn Predicate>>,
) -> Layer {
    Arc::new(move |inner| {
        let host = host.clone();
        let approver = approver.clone();
        let allowlist = allowlist.clone();
        let predicates = predicates.clone();
        guard(
            inner,
            Arc::new(move |spec: SessionSpec| {
                let approver = approver.clone();
                let allowlist = allowlist.clone();
                let predicates = predicates.clone();
                let host = host.clone();
                Box::pin(async move {
                    let request = ExecRequest::with_host(&spec, &host);
                    if let Some(list) = &allowlist
                        && list.allows(request.normalised_tokens())
                    {
                        return Ok(spec);
                    }
                    let flagged = predicates
                        .iter()
                        .find_map(|predicate| predicate.matches(&request));
                    let Some(reason) = flagged.or_else(|| {
                        // A configured allowlist is a gate: anything it does not
                        // approve still needs a human, otherwise the list quietly
                        // degrades from "pre-approve, ask about the rest" to
                        // "skip predicates for a few commands".
                        allowlist
                            .as_ref()
                            .map(|_| "command is not in the sandbox allowlist".to_owned())
                    }) else {
                        return Ok(spec);
                    };
                    let Some(approver) = approver else {
                        return Err(Error::policy_denied(format!(
                            "sandbox: {reason} needs approval, and no approver is configured"
                        )));
                    };
                    match approver
                        .approve(ApprovalRequest {
                            exec: request,
                            reason: reason.clone(),
                        })
                        .await
                    {
                        Ok(Decision::Allow) => Ok(spec),
                        Ok(Decision::Deny) => Err(Error::policy_denied(format!(
                            "sandbox: the approver denied this call ({reason})"
                        ))),
                        Err(error) => Err(Error::policy_denied(format!(
                            "sandbox: approval failed ({reason}): {error}"
                        ))),
                    }
                })
            }),
        )
    })
}

// ---------------------------------------------------------------------------
// Composition
// ---------------------------------------------------------------------------

/// The daemon-owned policy used by [`compose_local`].
///
/// Zero values are conservative: no command gate, no pre-approval, no
/// predicates (so no approvals are ever requested).
#[derive(Default)]
pub struct LocalPolicy {
    /// Daemon defaults, merged before the approval check runs.
    pub defaults: ExecOptions,
    /// A hard command-name gate (`None` installs none; an empty list blocks all).
    pub allowed_commands: Option<Vec<String>>,
    /// Commands that skip the approver.
    pub allowlist: Option<Arc<Allowlist>>,
    /// Who to ask.
    pub approver: Option<Arc<dyn Approver>>,
    /// What is worth asking about.
    pub predicates: Vec<Arc<dyn Predicate>>,
    /// Where this deployment's programmes live. The default is the host PATH;
    /// a runner built from settings should pass its own.
    pub host: HostPolicy,
    /// Install the read-only classifier, harden what it proves, and let a proven
    /// read-only call answer for itself instead of reaching the approver.
    ///
    /// One flag rather than two on purpose: classification without hardening is
    /// still a string check (the hardening is what makes the verdict true), so a
    /// composer must not be able to install half of it.
    pub auto_approve_read_only: bool,
}

/// The recommended local chain:
///
/// ```text
/// defaults → approval → allow_commands → backend
/// ```
///
/// `defaults` is outermost so the approver sees the **effective** policy rather
/// than the caller's raw request, and `allow_commands` is innermost so an
/// approval can never talk its way past a command gate.
#[must_use]
pub fn compose_local(backend: RunnerRef, policy: LocalPolicy) -> RunnerRef {
    let LocalPolicy {
        defaults: default_opts,
        allowed_commands,
        allowlist,
        approver,
        predicates,
        host,
        auto_approve_read_only,
    } = policy;
    let mut layers: Vec<Layer> = Vec::new();
    if !default_opts.eq(&ExecOptions::default()) {
        layers.push(defaults(default_opts));
    }
    // Hardening runs before approval, so the approver — and the automatic one —
    // sees the spec that will actually be executed.
    if auto_approve_read_only {
        layers.push(super::readonly::harden_read_only(host.clone()));
    }
    if !predicates.is_empty() || approver.is_some() || allowlist.is_some() {
        let approver = if auto_approve_read_only {
            Some(super::readonly::auto_approve(approver))
        } else {
            approver
        };
        layers.push(approval_with_host(
            host.clone(),
            approver,
            allowlist,
            predicates,
        ));
    }
    if let Some(allowed) = allowed_commands {
        layers.push(allow_commands_with_host(allowed, host.clone()));
    }
    super::Chain::of(layers).around(backend)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::local::LocalRunner;
    use crate::caps::sandbox::policy::stub::Sink;
    // The tests that use this are `#[cfg(unix)]`: on Windows the resolved-programme
    // assertion has no `OsStr` to read, so the import would be unused.
    #[cfg_attr(not(unix), allow(unused_imports))]
    use std::ffi::OsStr;
    use std::sync::Mutex;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    /// The whole point of `auto_approve_read_only`: a call the classifier proved
    /// read-only, and that asked for a read-only write mode, does not need a human.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_proven_read_only_call_answers_for_itself() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ls = program(dir.path(), "ls");
        let host = host(dir.path());
        let asked = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let counter = asked.clone();
        let approver = ApproverFn::new(move |_: &ApprovalRequest| {
            counter.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Decision::Deny
        });
        let sink = Arc::new(Sink::default());
        let runner = compose_local(
            sink.clone(),
            LocalPolicy {
                auto_approve_read_only: true,
                approver: Some(Arc::new(approver)),
                // Something the approver would otherwise be asked about.
                predicates: vec![net_non_default()],
                host: host.clone(),
                ..LocalPolicy::default()
            },
        );
        let mut spec = SessionSpec::new(vec![ls.display().to_string()]);
        spec.opts.write = crate::caps::sandbox::contract::WriteMode::ReadOnly;
        spec.opts.net.mode = crate::caps::sandbox::contract::NetMode::DenyAll;
        // The sink refuses to spawn — it is a recording stub — so the assertion is
        // about what the chain did, not about the session.
        let _ = runner.start(spec).await;
        assert_eq!(
            asked.load(std::sync::atomic::Ordering::SeqCst),
            0,
            "the approver must not be consulted for a proven read-only call"
        );
        assert!(sink.take().is_some(), "and the call must reach the backend");
    }

    /// Auto-approval is about a *read-only* call. A caller that asked to write is
    /// asking for something wider than the verdict covers.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_read_only_programme_that_asked_to_write_is_not_auto_approved() {
        let dir = tempfile::tempdir().expect("tempdir");
        let ls = program(dir.path(), "ls");
        let asked = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let counter = asked.clone();
        let approver = ApproverFn::new(move |_: &ApprovalRequest| {
            counter.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Decision::Allow
        });
        let sink = Arc::new(Sink::default());
        let runner = compose_local(
            sink.clone(),
            LocalPolicy {
                auto_approve_read_only: true,
                approver: Some(Arc::new(approver)),
                predicates: vec![net_non_default()],
                host: host(dir.path()),
                ..LocalPolicy::default()
            },
        );
        let mut spec = SessionSpec::new(vec![ls.display().to_string()]);
        spec.opts.write = crate::caps::sandbox::contract::WriteMode::Workspace;
        spec.opts.net.mode = crate::caps::sandbox::contract::NetMode::DenyAll;
        let _ = runner.start(spec).await;
        assert_eq!(
            asked.load(std::sync::atomic::Ordering::SeqCst),
            1,
            "a write request must still reach the approver"
        );
        assert!(sink.take().is_some(), "the approver allowed it through");
    }

    /// A writer is never auto-approved, whatever the write mode says.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_writer_never_answers_for_itself() {
        let dir = tempfile::tempdir().expect("tempdir");
        let rm = program(dir.path(), "rm");
        let sink = Arc::new(Sink::default());
        let runner = compose_local(
            sink.clone(),
            LocalPolicy {
                auto_approve_read_only: true,
                predicates: vec![net_non_default()],
                host: host(dir.path()),
                ..LocalPolicy::default()
            },
        );
        let mut spec = SessionSpec::new(vec![rm.display().to_string()]);
        spec.opts.write = crate::caps::sandbox::contract::WriteMode::ReadOnly;
        spec.opts.net.mode = crate::caps::sandbox::contract::NetMode::DenyAll;
        let error = runner.start(spec).await.expect_err("denied");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(sink.take().is_none(), "it must not reach the backend");
    }

    #[tokio::test]
    async fn a_gated_command_is_refused_before_it_reaches_the_backend() {
        let runner = allow_commands(["sh".to_owned()])(Arc::new(LocalRunner::new()));
        let error = runner
            .start(spec(&["rm", "-rf", "/tmp/nope"]))
            .await
            .expect_err("gated");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(error.to_string().contains("allow-list"), "{error}");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn approval_cannot_bypass_the_command_gate() {
        let approver = Arc::new(ApproverFn::new(|_: &ApprovalRequest| Decision::Allow));
        let chain = super::super::Chain::of([
            approval(Some(approver), None, vec![net_non_default()]),
            allow_commands(["sh".to_owned()]),
        ]);
        let runner = chain.around(Arc::new(LocalRunner::new()));
        let mut request = spec(&["rm", "-rf", "/tmp/nope"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = runner.start(request).await.expect_err("gated");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_flagged_call_without_an_approver_fails_closed() {
        let runner = approval(None, None, vec![net_non_default()])(Arc::new(LocalRunner::new()));
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = runner.start(request).await.expect_err("denied");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(error.to_string().contains("no approver"), "{error}");
    }

    #[tokio::test]
    async fn an_allowed_call_skips_the_approver() {
        let asked = Arc::new(Mutex::new(0usize));
        let counter = asked.clone();
        let approver = Arc::new(ApproverFn::new(move |_: &ApprovalRequest| {
            *counter.lock().unwrap() += 1;
            Decision::Deny
        }));
        let allowlist = Arc::new(Allowlist::parse(["true".to_owned()]).expect("allowlist"));
        let runner = approval(Some(approver), Some(allowlist), vec![net_non_default()])(Arc::new(
            LocalRunner::new(),
        ));
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        // The call is refused by the backend (the local runner cannot enforce
        // that mode), not by the approver: the allowlist pre-approved it.
        let error = runner.start(request).await.expect_err("backend");
        assert_eq!(error.kind(), crate::error::ErrorKind::NotAvailable);
        assert_eq!(*asked.lock().unwrap(), 0, "the approver must not be asked");
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_denied_approval_is_a_policy_denial() {
        let approver = Arc::new(ApproverFn::new(|_: &ApprovalRequest| Decision::Deny));
        let runner =
            approval(Some(approver), None, vec![net_non_default()])(Arc::new(LocalRunner::new()));
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = runner.start(request).await.expect_err("denied");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(error.to_string().contains("denied this call"), "{error}");
    }

    #[tokio::test]
    async fn an_approver_error_is_a_refusal() {
        struct Broken;
        #[async_trait]
        impl Approver for Broken {
            async fn approve(&self, _request: ApprovalRequest) -> Result<Decision> {
                Err(Error::internal("prompt crashed"))
            }
        }
        let runner = approval(Some(Arc::new(Broken)), None, vec![net_non_default()])(Arc::new(
            LocalRunner::new(),
        ));
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = runner.start(request).await.expect_err("denied");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
    }

    #[tokio::test]
    async fn the_approver_sees_the_effective_policy() {
        let seen = Arc::new(Mutex::new(None::<ExecOptions>));
        let slot = seen.clone();
        let approver = Arc::new(ApproverFn::new(move |request: &ApprovalRequest| {
            *slot.lock().unwrap() = Some(request.exec.opts.clone());
            Decision::Allow
        }));
        let default_opts = ExecOptions {
            timeout: Some(std::time::Duration::from_secs(5)),
            write: WriteMode::ReadOnly,
            ..ExecOptions::default()
        };
        let chain = super::super::Chain::of([
            defaults(default_opts),
            approval(Some(approver), None, vec![net_non_default()]),
        ]);
        let runner = chain.around(Arc::new(LocalRunner::new()));
        let mut request = spec(&["sh", "-c", "true"]);
        request.opts.net.mode = NetMode::DenyAll;
        request.opts.timeout = Some(std::time::Duration::from_secs(60));
        let _ = runner.start(request).await;
        let opts = seen.lock().unwrap().clone().expect("approver called");
        assert_eq!(
            opts.timeout,
            Some(std::time::Duration::from_secs(5)),
            "the shorter timeout wins"
        );
        assert_eq!(opts.write, WriteMode::ReadOnly, "the stricter mode wins");
        runner.close().await.expect("close");
    }

    #[test]
    fn the_daemon_owns_the_caps_and_the_environment_allow_list() {
        let mut caller = ExecOptions {
            timeout: Some(std::time::Duration::from_secs(1)),
            write: WriteMode::ReadOnly,
            limits: crate::caps::sandbox::contract::ResourceLimits {
                max_output_bytes: Some(1024),
                ..crate::caps::sandbox::contract::ResourceLimits::default()
            },
            env: crate::caps::sandbox::contract::EnvPolicy {
                allow: Some(vec!["AWS_SECRET_ACCESS_KEY".to_owned()]),
                inject: Default::default(),
            },
            ..ExecOptions::default()
        };
        let daemon = ExecOptions {
            timeout: Some(std::time::Duration::from_secs(30)),
            limits: crate::caps::sandbox::contract::ResourceLimits {
                max_output_bytes: Some(4096),
                memory_bytes: Some(1 << 20),
                ..crate::caps::sandbox::contract::ResourceLimits::default()
            },
            env: crate::caps::sandbox::contract::EnvPolicy {
                allow: Some(vec!["PATH".to_owned()]),
                inject: Default::default(),
            },
            ..ExecOptions::default()
        };
        tighten(&mut caller, &daemon);
        assert_eq!(
            caller.timeout,
            Some(std::time::Duration::from_secs(1)),
            "the caller's shorter timeout survives"
        );
        assert_eq!(caller.write, WriteMode::ReadOnly, "read-only is pinned");
        // Go: `Resources: r.defaults.Resources`, `Env.Allow: defaults wins entirely`.
        assert_eq!(caller.limits.max_output_bytes, Some(4096));
        assert_eq!(caller.limits.memory_bytes, Some(1 << 20));
        assert_eq!(
            caller.env.allow,
            Some(vec!["PATH".to_owned()]),
            "a caller must not be able to widen the host-environment allow-list"
        );
    }

    #[test]
    fn the_daemon_owns_the_network_posture() {
        let daemon = ExecOptions {
            net: crate::caps::sandbox::contract::NetPolicy {
                mode: NetMode::Proxy,
                proxy: Some("http://corp:3128".to_owned()),
                ..crate::caps::sandbox::contract::NetPolicy::default()
            },
            ..ExecOptions::default()
        };
        // Same mode, different parameters: the daemon's whole policy must win,
        // otherwise a caller swaps the upstream the rules were written for.
        let mut caller = ExecOptions {
            net: crate::caps::sandbox::contract::NetPolicy {
                mode: NetMode::Proxy,
                proxy: Some("http://attacker:3128".to_owned()),
                ..crate::caps::sandbox::contract::NetPolicy::default()
            },
            ..ExecOptions::default()
        };
        tighten(&mut caller, &daemon);
        assert_eq!(caller.net.proxy.as_deref(), Some("http://corp:3128"));

        // A caller may still tighten beyond a daemon that expressed no opinion.
        let mut caller = ExecOptions {
            net: crate::caps::sandbox::contract::NetPolicy {
                mode: NetMode::DenyAll,
                ..crate::caps::sandbox::contract::NetPolicy::default()
            },
            ..ExecOptions::default()
        };
        tighten(&mut caller, &ExecOptions::default());
        assert_eq!(caller.net.mode, NetMode::DenyAll);
    }

    #[test]
    fn injected_variables_are_a_union_the_caller_wins() {
        let mut caller = ExecOptions::default();
        caller
            .env
            .inject
            .insert("PATH".to_owned(), "/caller".to_owned());
        caller
            .env
            .inject
            .insert("RUN_ID".to_owned(), "42".to_owned());
        let mut daemon = ExecOptions::default();
        daemon
            .env
            .inject
            .insert("PATH".to_owned(), "/daemon".to_owned());
        daemon
            .env
            .inject
            .insert("STATIC".to_owned(), "1".to_owned());
        tighten(&mut caller, &daemon);
        // Go: "union; caller entries override defaults on key collision" — the one
        // place a caller may layer per-call context.
        assert_eq!(
            caller.env.inject.get("PATH").map(String::as_str),
            Some("/caller")
        );
        assert_eq!(
            caller.env.inject.get("RUN_ID").map(String::as_str),
            Some("42")
        );
        assert_eq!(
            caller.env.inject.get("STATIC").map(String::as_str),
            Some("1")
        );
    }

    #[test]
    fn a_workdir_predicate_only_flags_paths_outside_the_root() {
        let predicate = workdir_outside_root("/srv/box");
        let mut request = ExecRequest::from_spec(&spec(&["true"]));
        request.opts.workdir = Some(PathBuf::from("/srv/box/sub"));
        assert!(predicate.matches(&request).is_none());
        request.opts.workdir = Some(PathBuf::from("/srv/box2"));
        assert!(predicate.matches(&request).is_some());
    }

    #[test]
    fn a_sensitive_pattern_is_a_glob() {
        let predicate = command_patterns(["git-*".to_owned()]).expect("patterns");
        let request = ExecRequest::from_spec(&spec(&["/usr/bin/git-upload-pack"]));
        assert!(
            predicate.matches(&request).is_some(),
            "a glob must match, not silently disable the tripwire"
        );
        let exact = command_patterns(["git-upload-pack".to_owned()]).expect("patterns");
        assert!(exact.matches(&request).is_some());
        assert!(
            exact
                .matches(&ExecRequest::from_spec(&spec(&["git"])))
                .is_none()
        );
    }

    #[test]
    fn a_traversing_or_relative_workdir_is_classified() {
        let predicate = workdir_outside_root("/srv/box");
        let mut request = ExecRequest::from_spec(&spec(&["true"]));
        request.opts.workdir = Some(PathBuf::from("/srv/box/../etc"));
        assert!(
            predicate.matches(&request).is_some(),
            "a lexical escape must be flagged"
        );
        request.opts.workdir = Some(PathBuf::from("sub"));
        assert!(
            predicate.matches(&request).is_none(),
            "a relative workdir stays under the root"
        );
    }

    #[test]
    fn sensitive_patterns_match_by_base_name() {
        let predicate = command_patterns(["curl".to_owned(), "wget".to_owned()]).expect("patterns");
        let request = ExecRequest::from_spec(&spec(&["/usr/bin/curl", "https://x"]));
        assert!(predicate.matches(&request).is_some());
        let request = ExecRequest::from_spec(&spec(&["ls"]));
        assert!(predicate.matches(&request).is_none());
    }

    #[test]
    fn an_empty_pattern_is_rejected() {
        assert!(command_patterns([" ".to_owned()]).is_err());
    }

    /// A host policy whose only programme directory is `dir`.
    // Every caller is a `#[cfg(unix)]` test (they build a real programme tree).
    #[cfg_attr(not(unix), allow(dead_code))]
    fn host(dir: &std::path::Path) -> HostPolicy {
        HostPolicy {
            path_dirs: vec![std::fs::canonicalize(dir).expect("canonicalize")],
        }
    }

    /// A fake `dir/name` executable; returns its canonical path.
    #[cfg(unix)]
    fn program(dir: &std::path::Path, name: &str) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let path = dir.join(name);
        std::fs::write(&path, "#!/bin/sh\nexit 0\n").expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        std::fs::canonicalize(path).expect("canonicalize")
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn the_command_gate_judges_the_planned_programme() {
        let host_dir = tempfile::tempdir().expect("tempdir");
        let smuggled_dir = tempfile::tempdir().expect("tempdir");
        let real = program(host_dir.path(), "git");
        let smuggled = program(smuggled_dir.path(), "git");
        let sink = Arc::new(Sink::default());
        let runner =
            allow_commands_with_host(["git".to_owned()], host(host_dir.path()))(sink.clone());

        // A bare name that the *host* resolves is allowed, whatever the request's
        // PATH points at.
        let mut allowed = spec(&["git", "status"]);
        allowed
            .opts
            .env
            .inject
            .insert("PATH".to_owned(), smuggled_dir.path().display().to_string());
        let _ = runner.start(allowed).await;
        assert!(
            sink.take().is_some(),
            "the gate must pass a host programme on to the backend"
        );

        // A binary the caller dropped somewhere is refused although its base
        // name is `git`.
        let error = runner
            .start(spec(&[smuggled.to_str().expect("utf8"), "status"]))
            .await
            .expect_err("refused");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(error.to_string().contains("allow-list"), "{error}");
        assert!(sink.take().is_none(), "the backend must not be reached");

        // The same file spelt the way the host spells it is the same file.
        let _ = runner
            .start(spec(&[real.to_str().expect("utf8"), "status"]))
            .await;
        assert!(
            sink.take().is_some(),
            "a pinned rule and a host rule must agree about one file"
        );
    }

    #[tokio::test]
    async fn a_programme_that_does_not_resolve_is_refused() {
        let sink = Arc::new(Sink::default());
        let runner = allow_commands(["ls".to_owned()])(sink.clone());
        let error = runner
            .start(spec(&["definitely-not-a-sandbox-programme"]))
            .await
            .expect_err("refused");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(error.to_string().contains("does not resolve"), "{error}");
        assert!(
            sink.take().is_none(),
            "an unresolved programme must never fall through to exec"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn every_predicate_and_the_approver_judge_one_plan() {
        let seen = Arc::new(Mutex::new(Vec::<PathBuf>::new()));

        struct Watcher(Arc<Mutex<Vec<PathBuf>>>);
        impl Predicate for Watcher {
            fn matches(&self, request: &ExecRequest) -> Option<String> {
                if let Some(program) = request.planned.program() {
                    self.0.lock().unwrap().push(program.to_path_buf());
                }
                Some("watched".to_owned())
            }
        }

        let recorded = seen.clone();
        let approver = Arc::new(ApproverFn::new(move |request: &ApprovalRequest| {
            if let Some(program) = request.exec.planned.program() {
                recorded.lock().unwrap().push(program.to_path_buf());
            }
            Decision::Deny
        }));
        let chain = super::super::Chain::of([approval(
            Some(approver),
            None,
            vec![Arc::new(Watcher(seen.clone()))],
        )]);
        let runner = chain.around(Arc::new(LocalRunner::new()));
        let _ = runner.start(spec(&["git", "status"])).await;
        let recorded = seen.lock().unwrap().clone();
        assert_eq!(recorded.len(), 2, "one predicate, one approver");
        assert_eq!(
            recorded[0], recorded[1],
            "the predicate and the approver must see the same resolution"
        );
        assert_eq!(
            recorded[0].file_name().and_then(OsStr::to_str),
            Some("git"),
            "and it must be the resolved file, not the spelling"
        );
        runner.close().await.expect("close");
    }
}
