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

use async_trait::async_trait;
use flowcraft_error::{Error, Result};

use super::allowlist::{Allowlist, normalise};
use super::{Layer, RunnerRef, guard};
use crate::contract::{ExecOptions, NetMode, SessionSpec, WriteMode};

/// A snapshot of one start request, shared by predicates and approvers.
///
/// A snapshot rather than the live spec, so a predicate can inspect what will
/// run but cannot rewrite it: only the layers may tighten a request.
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
    /// The normalised tokens a rule is matched against.
    pub tokens: Vec<String>,
}

impl ExecRequest {
    /// Snapshot `spec`.
    #[must_use]
    pub fn from_spec(spec: &SessionSpec) -> Self {
        Self {
            command: spec.argv.first().cloned().unwrap_or_default(),
            args: spec.argv.iter().skip(1).cloned().collect(),
            opts: spec.opts.clone(),
            tty: spec.tty,
            stdin: spec.wants_stdin(),
            tokens: normalise(spec),
        }
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
        let cleaned = crate::confinement::path::normalize(dir);
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

/// Refuse every command whose name is not in `allowed`.
///
/// The name is matched literally and by base name, so `/usr/bin/git` is allowed
/// by `git`. It sits closest to the backend: approval cannot bypass it.
#[must_use]
pub fn allow_commands(allowed: impl IntoIterator<Item = String>) -> Layer {
    let allowed: BTreeSet<String> = allowed
        .into_iter()
        .map(|name| name.trim().to_owned())
        .filter(|name| !name.is_empty())
        .collect();
    Arc::new(move |inner| {
        let allowed = allowed.clone();
        guard(
            inner,
            Arc::new(move |spec: SessionSpec| {
                let allowed = allowed.clone();
                Box::pin(async move {
                    let Some(program) = spec.argv.first() else {
                        return Err(Error::validation("sandbox: argv is empty"));
                    };
                    let base = std::path::Path::new(program)
                        .file_name()
                        .and_then(|name| name.to_str())
                        .unwrap_or(program);
                    if allowed.contains(program) || allowed.contains(base) {
                        Ok(spec)
                    } else {
                        Err(Error::policy_denied(format!(
                            "sandbox: {program:?} is not in the command allow-list"
                        )))
                    }
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
    Arc::new(move |inner| {
        let approver = approver.clone();
        let allowlist = allowlist.clone();
        let predicates = predicates.clone();
        guard(
            inner,
            Arc::new(move |spec: SessionSpec| {
                let approver = approver.clone();
                let allowlist = allowlist.clone();
                let predicates = predicates.clone();
                Box::pin(async move {
                    let request = ExecRequest::from_spec(&spec);
                    if let Some(list) = &allowlist
                        && list.allows(&request.tokens)
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
    } = policy;
    let mut layers: Vec<Layer> = Vec::new();
    if !default_opts.eq(&ExecOptions::default()) {
        layers.push(defaults(default_opts));
    }
    if !predicates.is_empty() || approver.is_some() || allowlist.is_some() {
        layers.push(approval(approver, allowlist, predicates));
    }
    if let Some(allowed) = allowed_commands {
        layers.push(allow_commands(allowed));
    }
    super::Chain::of(layers).around(backend)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::local::LocalRunner;
    use std::sync::Mutex;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    #[tokio::test]
    async fn a_gated_command_is_refused_before_it_reaches_the_backend() {
        let runner = allow_commands(["sh".to_owned()])(Arc::new(LocalRunner::new()));
        let error = runner
            .start(spec(&["rm", "-rf", "/tmp/nope"]))
            .await
            .expect_err("gated");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::PolicyDenied);
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
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::PolicyDenied);
        runner.close().await.expect("close");
    }

    #[tokio::test]
    async fn a_flagged_call_without_an_approver_fails_closed() {
        let runner = approval(None, None, vec![net_non_default()])(Arc::new(LocalRunner::new()));
        let mut request = spec(&["true"]);
        request.opts.net.mode = NetMode::DenyAll;
        let error = runner.start(request).await.expect_err("denied");
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::PolicyDenied);
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
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::NotAvailable);
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
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::PolicyDenied);
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
        assert_eq!(error.kind(), flowcraft_error::ErrorKind::PolicyDenied);
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
            limits: crate::contract::ResourceLimits {
                max_output_bytes: Some(1024),
                ..crate::contract::ResourceLimits::default()
            },
            env: crate::contract::EnvPolicy {
                allow: Some(vec!["AWS_SECRET_ACCESS_KEY".to_owned()]),
                inject: Default::default(),
            },
            ..ExecOptions::default()
        };
        let daemon = ExecOptions {
            timeout: Some(std::time::Duration::from_secs(30)),
            limits: crate::contract::ResourceLimits {
                max_output_bytes: Some(4096),
                memory_bytes: Some(1 << 20),
                ..crate::contract::ResourceLimits::default()
            },
            env: crate::contract::EnvPolicy {
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
            net: crate::contract::NetPolicy {
                mode: NetMode::Proxy,
                proxy: Some("http://corp:3128".to_owned()),
                ..crate::contract::NetPolicy::default()
            },
            ..ExecOptions::default()
        };
        // Same mode, different parameters: the daemon's whole policy must win,
        // otherwise a caller swaps the upstream the rules were written for.
        let mut caller = ExecOptions {
            net: crate::contract::NetPolicy {
                mode: NetMode::Proxy,
                proxy: Some("http://attacker:3128".to_owned()),
                ..crate::contract::NetPolicy::default()
            },
            ..ExecOptions::default()
        };
        tighten(&mut caller, &daemon);
        assert_eq!(caller.net.proxy.as_deref(), Some("http://corp:3128"));

        // A caller may still tighten beyond a daemon that expressed no opinion.
        let mut caller = ExecOptions {
            net: crate::contract::NetPolicy {
                mode: NetMode::DenyAll,
                ..crate::contract::NetPolicy::default()
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
}
