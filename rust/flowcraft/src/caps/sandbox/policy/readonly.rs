//! The read-only classifier: "may this be auto-approved without asking?".
//!
//! Deliberately conservative: anything that is not *proven* read-only is
//! [`Verdict::NotReadOnly`], and the answer to that is to route the call to the
//! approver — never to allow it. A false negative costs a prompt; a false
//! positive costs the guarantee.
//!
//! # Classification is half of a promise
//!
//! A verdict of [`Verdict::ReadOnly`] is only worth something together with the
//! rewrite [`harden_read_only`] performs: a command can read files and still run
//! another programme (`LESSOPEN`, `GIT_PAGER`, `MANPAGER`, `PAGER`, a pager's
//! open-file flag), so the same layer that classifies also strips those switches
//! from the environment and hardens the argv (`git --no-pager -c
//! core.fsmonitor=false`, `man -P cat`). Classification without hardening is
//! still a string check.
//!
//! What is judged is never the caller's spelling: [`super::plan::plan`] has
//! already turned `argv[0]` into the file that will be exec'd and recorded the
//! effective environment, and [`classify`] decides about that. A command the plan
//! cannot resolve, or whose argv names a programme the layer cannot replace, is
//! [`Verdict::Refuse`]: an approver shown a hardened-but-different argv would be
//! approving something that does not run.
//!
//! Commands that can run other programs or touch non-file state are excluded on
//! purpose: `env`, `xargs`, `awk`, `tar`, interpreters, package managers,
//! `less`/`more` (a pager is an executor), and also `date` / `hostname` (`date
//! -s` and `hostname newname` change system state that no filesystem sandbox
//! blocks).

use std::ffi::OsStr;
use std::path::Path;
use std::sync::Arc;

use crate::caps::sandbox::contract::SessionSpec;
use crate::error::Error;

use super::allowlist::normalise_argv;
use super::approval::ExecRequest;
use super::plan::{HostPolicy, PlannedCommand, plan};
use super::{Layer, RunnerRef, guard};

/// Environment variables that name a programme some tools run on their own.
///
/// A read-only command with one of these set is not read-only: the value is an
/// argv this layer never sees.
pub const EXECUTION_SWITCHES: &[&str] = &[
    "BROWSER",
    "GIT_EDITOR",
    "GIT_PAGER",
    "LESSKEY",
    "LESSCLOSE",
    "LESSOPEN",
    "MANPAGER",
    "MORE",
    "PAGER",
];

/// What a command is, as far as an auto-approval may rely on it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Verdict {
    /// Proven read-only, and [`harden_read_only`] can remove every switch it
    /// could still use to run something.
    ReadOnly,
    /// Not proven read-only: the call belongs in front of the approver.
    NotReadOnly,
    /// Not judgeable: the programme does not resolve, or its argv names a
    /// programme this layer cannot replace with a reader. Not something an
    /// approval may then allow.
    Refuse(String),
}

/// Classify `planned`.
#[must_use]
pub fn classify(planned: &PlannedCommand) -> Verdict {
    let Some(program) = planned.program() else {
        return Verdict::Refuse(format!(
            "sandbox: {:?} does not resolve to an executable file, so nothing can be proven read-only",
            planned.argv0
        ));
    };
    if normalise_argv(&planned.argv).first() != planned.argv.first() {
        // The plan resolved the *wrapper*; the command the wrapper runs is
        // resolved by the child's environment, which no verdict here covers.
        return Verdict::NotReadOnly;
    }
    let base = program
        .file_name()
        .and_then(OsStr::to_str)
        .unwrap_or_default();
    let args = &planned.argv[1..];
    match base {
        "find" => verdict_of(find_is_read_only(args)),
        "rg" | "ripgrep" => verdict_of(rg_is_read_only(args)),
        "git" => git_verdict(args),
        "man" => man_verdict(args),
        "sed" => verdict_of(sed_is_read_only(args)),
        "sort" => verdict_of(sort_is_read_only(args)),
        other if is_read_only_base(other) => Verdict::ReadOnly,
        _ => Verdict::NotReadOnly,
    }
}

fn verdict_of(read_only: bool) -> Verdict {
    if read_only {
        Verdict::ReadOnly
    } else {
        Verdict::NotReadOnly
    }
}

/// Whether `request` is a known read-only command.
///
/// Deprecated: a boolean cannot tell "not read-only" from "cannot judge", and it
/// says nothing about the argv/env rewrite the answer now depends on. Judge the
/// plan with [`classify`] and harden with [`harden_read_only`].
#[deprecated(note = "judge the plan: `classify(&request.planned)`")]
#[must_use]
pub fn classify_read_only(request: &ExecRequest) -> bool {
    matches!(classify(&request.planned), Verdict::ReadOnly)
}

/// An approver that answers for itself when the call is provably read-only.
///
/// This is the *only* shape an automatic approval may take: it consults the same
/// classifier the hardening layer uses, it requires the caller to have asked for a
/// read-only write mode (approving a `Workspace` write would grant more than was
/// requested), and it refuses to be the last word — anything it cannot prove goes to
/// the approver it wraps, which may be none, in which case it is a denial.
///
/// Ordering matters and is the composer's job: [`harden_read_only`] must run
/// *before* the approval layer, so what this approves is what will actually run.
#[must_use]
pub fn auto_approve(
    inner: Option<Arc<dyn super::approval::Approver>>,
) -> Arc<dyn super::approval::Approver> {
    Arc::new(ReadOnlyApprover { inner })
}

struct ReadOnlyApprover {
    inner: Option<Arc<dyn super::approval::Approver>>,
}

impl std::fmt::Debug for ReadOnlyApprover {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.ReadOnlyApprover")
    }
}

#[async_trait::async_trait]
impl super::approval::Approver for ReadOnlyApprover {
    async fn approve(
        &self,
        request: super::approval::ApprovalRequest,
    ) -> crate::error::Result<super::approval::Decision> {
        // The write mode first: it is the caller's promise about what the command
        // may do, and a read-only verdict is about the programme, not about that.
        let asked_for_read_only =
            request.exec.opts.write == crate::caps::sandbox::contract::WriteMode::ReadOnly;
        if asked_for_read_only && classify(&request.exec.planned) == Verdict::ReadOnly {
            return Ok(super::approval::Decision::Allow);
        }
        match &self.inner {
            Some(inner) => inner.approve(request).await,
            None => Err(Error::policy_denied(format!(
                "sandbox: {} needs approval, and no approver is configured",
                request.reason
            ))),
        }
    }
}

/// A layer that hardens what it can prove read-only, and refuses what it cannot.
///
/// `ReadOnly` requests come back with the execution switches removed from
/// [`super::approval::ExecRequest`]'s environment and with the argv hardened, and
/// that is exactly what an auto-approval may then pass on. `NotReadOnly`
/// requests are passed through untouched for the approver, and a [`Verdict::Refuse`]
/// is a [`Error::policy_denied`] — never a silent approval.
#[must_use]
pub fn harden_read_only(host: HostPolicy) -> Layer {
    Arc::new(move |inner: RunnerRef| {
        let host = host.clone();
        guard(
            inner,
            Arc::new(move |mut spec: SessionSpec| {
                let host = host.clone();
                Box::pin(async move {
                    let planned = plan(&spec, &host);
                    match classify(&planned) {
                        Verdict::NotReadOnly => Ok(spec),
                        Verdict::Refuse(reason) => Err(Error::policy_denied(reason)),
                        Verdict::ReadOnly => {
                            let stripped = harden(&mut spec, &planned);
                            if !stripped.is_empty() {
                                tracing::debug!(
                                    program = %planned.program().map_or_else(String::new, |path| path.display().to_string()),
                                    stripped = ?stripped,
                                    "sandbox: hardened a read-only command"
                                );
                            }
                            Ok(spec)
                        }
                    }
                })
            }),
        )
    })
}

/// Remove the switches a read-only verdict promised to remove.
///
/// Returns the environment names it removed, for the audit log. Injection is the
/// only thing a layer can *unset*: a variable the policy merely passes through
/// (`EnvPolicy::allow == None` inherits everything) can only be shadowed, which
/// is why the argv hardening has to make the remaining switches inert —
/// `--no-pager` for git, `-P cat` for man.
fn harden(spec: &mut SessionSpec, planned: &PlannedCommand) -> Vec<String> {
    let mut stripped = Vec::new();
    for name in EXECUTION_SWITCHES {
        if spec.opts.env.inject.remove(*name).is_some() {
            stripped.push((*name).to_owned());
        }
        if let Some(allow) = &mut spec.opts.env.allow
            && allow.iter().any(|candidate| candidate == name)
        {
            allow.retain(|candidate| candidate != name);
            stripped.push((*name).to_owned());
        }
    }
    let base = planned
        .program()
        .and_then(Path::file_name)
        .and_then(OsStr::to_str)
        .unwrap_or_default();
    match base {
        "git" => {
            let _ = spec.argv.splice(
                1..1,
                ["--no-pager", "-c", "core.fsmonitor=false"].map(str::to_owned),
            );
        }
        "man" if man_pager(&spec.argv[1..]) != Some("cat") => {
            let _ = spec.argv.splice(1..1, ["-P", "cat"].map(str::to_owned));
        }
        _ => {}
    }
    stripped
}

/// Commands treated as read-only in every form.
///
/// A command that can execute another programme or write a file is absent by
/// construction: an unrecognised command is a false negative that goes to the
/// approver, which is safe.
fn is_read_only_base(base: &str) -> bool {
    matches!(
        base,
        "basename"
            | "cat"
            | "cmp"
            | "comm"
            | "cut"
            | "df"
            | "diff"
            | "dirname"
            | "du"
            | "echo"
            | "false"
            | "fgrep"
            | "file"
            | "grep"
            | "egrep"
            | "groups"
            | "head"
            | "hexdump"
            | "id"
            | "ls"
            | "man"
            | "md5sum"
            | "nl"
            | "od"
            | "printenv"
            | "printf"
            | "pwd"
            | "readlink"
            | "realpath"
            | "seq"
            | "sha1sum"
            | "sha256sum"
            | "stat"
            | "strings"
            | "tac"
            | "tail"
            | "tr"
            | "true"
            | "uniq"
            | "wc"
            | "which"
            | "whoami"
    )
}

/// `find` can delete, execute, and write reports.
fn find_is_read_only(args: &[String]) -> bool {
    !args.iter().any(|arg| {
        matches!(
            arg.as_str(),
            "-delete" | "-exec" | "-execdir" | "-ok" | "-okdir" | "-fls"
        ) || arg.starts_with("-fprint")
    })
}

/// `ripgrep` can execute a helper, decompress archives and write artefacts.
///
/// `--hostname-bin` and `-z` are the CVE-2025-54558 class: both make rg run
/// another program, so they must never be auto-approved.
fn rg_is_read_only(args: &[String]) -> bool {
    for arg in args {
        let long = arg.as_str();
        if matches!(
            long,
            "-z" | "--null-data" | "--pre" | "--pre-glob" | "--search-zip" | "--hostname-bin"
        ) || long.starts_with("--pre=")
            || long.starts_with("--pre-glob=")
            || long.starts_with("--hostname-bin=")
            || long.starts_with("--generate")
        {
            return false;
        }
        // A short group that contains `z` (`-az`, `-zf`) enables the same path.
        if arg.starts_with('-') && !arg.starts_with("--") && arg.contains('z') {
            return false;
        }
    }
    true
}

/// `git` subcommands whose every form is read-only once their options are checked.
///
/// Subcommands with a writing variant (`branch`, `tag`, `remote`, `config`,
/// `stash`, `reflog`, `symbolic-ref`, ...) are absent on purpose: `symbolic-ref`
/// rewrites `.git/HEAD`.
fn git_verdict(args: &[String]) -> Verdict {
    let mut index = 0usize;
    while index < args.len() {
        let arg = args[index].as_str();
        if arg == "-c"
            || arg.starts_with("-c")
            || arg == "--config-env"
            || arg.starts_with("--config-env=")
        {
            // `-c core.pager=…` / `core.fsmonitor` / `core.sshCommand` are run by
            // git on read-only subcommands, so no `-c` form is auto-approved.
            return Verdict::NotReadOnly;
        }
        if arg.starts_with("--") {
            if matches!(
                arg,
                "--exec-path" | "--git-dir" | "--namespace" | "--work-tree"
            ) && index + 1 < args.len()
            {
                index += 1;
            }
            index += 1;
            continue;
        }
        if arg.starts_with('-') {
            // `-C` takes a directory value; the rest are boolean flags.
            if arg == "-C" && index + 1 < args.len() {
                index += 1;
            }
            index += 1;
            continue;
        }
        break;
    }
    let subcommand = args.get(index).map(String::as_str);
    let rest: &[String] = if index < args.len() {
        &args[index + 1..]
    } else {
        &[]
    };
    // `--paginate` is legal before or after the subcommand and re-enables the
    // pager that hardening's `--no-pager` removes.
    let pager = args
        .iter()
        .map(String::as_str)
        .find(|arg| *arg == "--paginate")
        .or_else(|| open_files_in_pager(subcommand, rest));
    if let Some(flag) = pager {
        // The pager is a programme of the caller's choosing, and no argv
        // rewrite can replace an explicit open-file flag.
        return Verdict::Refuse(format!(
            "sandbox: `git {} {flag}` runs another programme as a pager",
            subcommand.unwrap_or_default()
        ));
    }
    // Bare `git` (or `git --version`) prints help/version.
    let Some(subcommand) = subcommand else {
        return Verdict::ReadOnly;
    };
    let known = matches!(
        subcommand,
        "blame"
            | "cat-file"
            | "check-attr"
            | "check-ignore"
            | "check-ref-format"
            | "count-objects"
            | "describe"
            | "diff"
            | "for-each-ref"
            | "grep"
            | "help"
            | "log"
            | "ls-files"
            | "ls-tree"
            | "name-rev"
            | "rev-parse"
            | "shortlog"
            | "show"
            | "status"
            | "verify-commit"
            | "verify-tag"
            | "whatchanged"
    );
    if !known {
        return Verdict::NotReadOnly;
    }
    let writes = rest.iter().any(|arg| {
        arg == "--output"
            || arg.starts_with("--output=")
            || ["--ext-diff", "--textconv", "--exec"]
                .iter()
                .any(|flag| arg == flag || arg.starts_with(&format!("{flag}=")))
    });
    verdict_of(!writes)
}

/// The open-files-in-pager flag a `git` argv names, when it names one.
///
/// `-O` is `git grep`'s open-files-in-pager flag; on `log`/`diff` the same
/// spelling is an order *file*, which is data.
fn open_files_in_pager<'a>(subcommand: Option<&str>, args: &'a [String]) -> Option<&'a str> {
    if subcommand != Some("grep") {
        return None;
    }
    args.iter()
        .map(String::as_str)
        .find(|arg| arg.starts_with("-O") || arg.starts_with("--open-files-in-pager"))
}

/// `man` is read-only only when its pager is a reader.
///
/// `-P pager` / `MANPAGER` / `PAGER` name a programme; `-H`/`--html` and `-B`
/// open a browser, and `-X` runs gxditview. Anything but `cat` is refused, not
/// referred: the approver would be shown the argv *after* hardening.
fn man_verdict(args: &[String]) -> Verdict {
    match man_pager(args) {
        Some("cat") | None => {}
        Some(pager) => {
            return Verdict::Refuse(format!(
                "sandbox: `man -P {pager}` runs another programme as a pager"
            ));
        }
    }
    if let Some(flag) = args
        .iter()
        .find(|arg| matches!(arg.as_str(), "-H" | "--html" | "-B" | "--browser" | "-X"))
    {
        return Verdict::Refuse(format!("sandbox: `man {flag}` runs another programme"));
    }
    Verdict::ReadOnly
}

/// The pager `man`'s argv asks for, if any (`-P cat`, `-Pcat`, `--pager=cat`).
fn man_pager(args: &[String]) -> Option<&str> {
    let mut index = 0usize;
    while index < args.len() {
        let arg = args[index].as_str();
        if arg == "-P" || arg == "--pager" {
            return Some(args.get(index + 1).map_or("", String::as_str));
        }
        if let Some(value) = arg.strip_prefix("--pager=") {
            return Some(value);
        }
        if let Some(value) = arg.strip_prefix("-P").filter(|value| !value.is_empty()) {
            return Some(value);
        }
        index += 1;
    }
    None
}

/// `sed` can edit in place, run a script file, or write/execute from a script
/// (`s///w file`, `s///e`). Nothing but a bounded print is provably read-only.
fn sed_is_read_only(args: &[String]) -> bool {
    // Go's shape: `sed -n <Np|N,Mp> [file]`.
    if args.len() < 2 || args.len() > 3 {
        return false;
    }
    if args[0] != "-n" {
        return false;
    }
    is_sed_print_address(&args[1])
}

/// `Np` or `N,Mp`, the only `sed` program treated as read-only.
fn is_sed_print_address(script: &str) -> bool {
    let Some(core) = script.strip_suffix('p') else {
        return false;
    };
    if core.is_empty() {
        return false;
    }
    match core.split_once(',') {
        None => core.chars().all(|ch| ch.is_ascii_digit()),
        Some((start, end)) => {
            !start.is_empty()
                && !end.is_empty()
                && start.chars().all(|ch| ch.is_ascii_digit())
                && end.chars().all(|ch| ch.is_ascii_digit())
        }
    }
}

/// `sort` can write its result to a file, including via a grouped short flag
/// (`-ro out` means `-r -o out`).
fn sort_is_read_only(args: &[String]) -> bool {
    for arg in args {
        if arg == "--output" || arg.starts_with("--output=") {
            return false;
        }
        if arg.starts_with('-') && !arg.starts_with("--") && arg.contains('o') {
            return false;
        }
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    // The tests that reach for these are `#[cfg(unix)]`: they write a real
    // programme into a temporary directory.
    #[cfg_attr(not(unix), allow(unused_imports))]
    use crate::caps::sandbox::contract::{ExecOptions, WriteMode};
    #[cfg_attr(not(unix), allow(unused_imports))]
    use crate::caps::sandbox::policy::stub::Sink;
    use std::path::PathBuf;

    /// A plan for `argv` whose programme resolves to `program`.
    ///
    /// Classification only reads the resolved path, so a synthetic one keeps
    /// these tests independent of what the host has installed.
    fn planned(program: &str, argv: &[&str], env: &[(&str, &str)]) -> PlannedCommand {
        PlannedCommand {
            argv0: argv.first().copied().unwrap_or_default().to_owned(),
            argv: argv.iter().map(|arg| (*arg).to_owned()).collect(),
            resolution: super::super::plan::Resolution::Path {
                program: PathBuf::from(program),
                path_dir: Some(PathBuf::from("/usr/bin")),
            },
            // The policy the test asked for: injected values are what the
            // classifier reads, and injection needs no host lookup.
            env: crate::caps::sandbox::contract::EnvPolicy {
                allow: Some(Vec::new()),
                inject: env
                    .iter()
                    .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
                    .collect(),
            },
        }
    }

    fn verdict(argv: &[&str]) -> Verdict {
        let program = format!("/usr/bin/{}", argv[0]);
        classify(&planned(&program, argv, &[]))
    }

    #[test]
    fn plain_readers_are_read_only() {
        for argv in [
            vec!["ls", "-la"],
            vec!["cat", "file"],
            vec!["grep", "-n", "x", "f"],
            vec!["wc", "-l", "f"],
            vec!["pwd"],
            vec!["man", "ls"],
        ] {
            assert_eq!(verdict(&argv), Verdict::ReadOnly, "{argv:?}");
        }
    }

    #[test]
    fn a_programme_that_can_run_or_write_is_not() {
        for argv in [
            vec!["env", "FOO=1", "rm"],
            vec!["xargs", "rm"],
            vec!["awk", "BEGIN{system(\"rm x\")}"],
            // A *simple* wrapped command is not judged either: the plan resolved
            // the wrapper, not the command it runs (see the dedicated test).
            vec!["sh", "-c", "ls && rm x"],
            vec!["tee", "f"],
            vec!["cp", "a", "b"],
            vec!["date", "-s", "now"],
            vec!["hostname", "newname"],
            // A pager is an executor: `LESSOPEN`, `!cmd`, `-O`.
            vec!["less", "f"],
            vec!["more", "f"],
        ] {
            assert_eq!(verdict(&argv), Verdict::NotReadOnly, "{argv:?}");
        }
    }

    #[test]
    fn a_wrapped_command_is_not_judged() {
        // `sh -c 'ls -la'` executes `ls` from the child's PATH, which the plan
        // never resolved, so there is no inode this verdict could be about.
        assert_eq!(verdict(&["sh", "-c", "ls -la"]), Verdict::NotReadOnly);
    }

    #[test]
    fn find_with_a_writing_action_is_not_read_only() {
        assert_eq!(verdict(&["find", ".", "-name", "*.rs"]), Verdict::ReadOnly);
        for action in ["-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls"] {
            assert_eq!(
                verdict(&["find", ".", action, "x"]),
                Verdict::NotReadOnly,
                "{action}"
            );
        }
        assert_eq!(
            verdict(&["find", ".", "-fprint", "out"]),
            Verdict::NotReadOnly
        );
    }

    #[test]
    fn ripgrep_with_a_preprocessor_is_not_read_only() {
        assert_eq!(verdict(&["rg", "-n", "needle", "."]), Verdict::ReadOnly);
        assert_eq!(verdict(&["rg", "--pre", "./x", "y"]), Verdict::NotReadOnly);
        assert_eq!(verdict(&["rg", "--generate", "man"]), Verdict::NotReadOnly);
        // The CVE-2025-54558 class: rg running another program or a decompressor.
        for argv in [
            vec!["rg", "--hostname-bin=./x", "a", "f"],
            vec!["rg", "-z", "p", "f.zip"],
            vec!["rg", "--search-zip", "p", "f"],
            vec!["rg", "-nz", "p", "f"],
        ] {
            assert_eq!(verdict(&argv), Verdict::NotReadOnly, "{argv:?}");
        }
    }

    #[test]
    fn git_is_read_only_only_for_read_subcommands() {
        for sub in ["status", "log", "diff", "show", "rev-parse", "ls-files"] {
            assert_eq!(
                verdict(&["git", sub]),
                Verdict::ReadOnly,
                "git {sub} must classify as read-only"
            );
        }
        for sub in [
            "push", "commit", "add", "checkout", "clean", "reset", "stash", "config", "branch",
            "tag", "remote", "fetch",
        ] {
            assert_eq!(
                verdict(&["git", sub]),
                Verdict::NotReadOnly,
                "git {sub} must not"
            );
        }
        assert_eq!(verdict(&["git", "log", "--output=f"]), Verdict::NotReadOnly);
        assert_eq!(
            verdict(&["git", "symbolic-ref", "HEAD", "refs/heads/x"]),
            Verdict::NotReadOnly
        );
        assert_eq!(
            verdict(&["git", "-c", "core.pager=x", "log"]),
            Verdict::NotReadOnly
        );
        assert_eq!(
            verdict(&["git", "diff", "--ext-diff"]),
            Verdict::NotReadOnly
        );
        assert_eq!(verdict(&["git", "log", "--textconv"]), Verdict::NotReadOnly);
        // A value-taking global option is skipped when locating the subcommand.
        assert_eq!(verdict(&["git", "-C", "d", "status"]), Verdict::ReadOnly);
    }

    #[test]
    fn a_git_pager_option_is_refused_not_referred() {
        // `-O` here opens files in a pager, and `--no-pager` cannot replace it:
        // an approval would be for the hardened argv, which is not this one.
        for argv in [
            vec!["git", "grep", "-O", "sh", "x"],
            vec!["git", "grep", "--open-files-in-pager", "sh", "x"],
            vec!["git", "grep", "--open-files-in-pager=sh", "x"],
            vec!["git", "log", "--paginate"],
        ] {
            assert!(
                matches!(verdict(&argv), Verdict::Refuse(_)),
                "{argv:?} must be refused"
            );
        }
        // On `log`, `-O` is an order file: data, not a programme.
        assert_eq!(
            verdict(&["git", "log", "-O", "orderfile"]),
            Verdict::ReadOnly
        );
    }

    #[test]
    fn a_man_pager_is_refused_unless_it_is_a_reader() {
        assert_eq!(verdict(&["man", "ls"]), Verdict::ReadOnly);
        assert_eq!(verdict(&["man", "-P", "cat", "ls"]), Verdict::ReadOnly);
        for argv in [
            vec!["man", "-P", "sh", "x"],
            vec!["man", "-Psh", "x"],
            vec!["man", "--pager=sh", "x"],
            vec!["man", "-H", "ls"],
            vec!["man", "--html", "ls"],
        ] {
            assert!(
                matches!(verdict(&argv), Verdict::Refuse(_)),
                "{argv:?} must be refused"
            );
        }
    }

    #[test]
    fn sort_to_a_file_is_not_read_only() {
        assert_eq!(verdict(&["sort", "-u", "f"]), Verdict::ReadOnly);
        assert_eq!(verdict(&["sort", "-o", "out", "f"]), Verdict::NotReadOnly);
        assert_eq!(
            verdict(&["sort", "--output=out", "f"]),
            Verdict::NotReadOnly
        );
    }

    #[test]
    fn sed_beyond_a_bounded_print_is_not_read_only() {
        assert_eq!(verdict(&["sed", "-n", "1,2p", "f"]), Verdict::ReadOnly);
        assert_eq!(verdict(&["sed", "-i", "s/a/b/", "f"]), Verdict::NotReadOnly);
    }

    #[test]
    fn an_unresolved_programme_is_refused() {
        // Never `NotReadOnly`: there is no inode to approve, and letting the
        // call through would hand the executor a decision the plan could not
        // make.
        let planned = plan(
            &SessionSpec::new(vec!["definitely-not-a-sandbox-programme".to_owned()]),
            &HostPolicy::default(),
        );
        assert_eq!(planned.resolution, super::super::plan::Resolution::NotFound);
        assert!(matches!(classify(&planned), Verdict::Refuse(_)));
    }

    /// The alias forwards to [`classify`], whose verdict needs a programme the host
    /// policy can resolve — so the test builds one instead of naming `ls`, which
    /// does not exist under the default path directories on Windows.
    #[cfg(unix)]
    #[test]
    fn the_deprecated_alias_still_classifies() {
        let dir = tempfile::tempdir().expect("tempdir");
        let host = host(dir.path());
        let ls = program(dir.path(), "ls");
        #[allow(deprecated)]
        let read_only = classify_read_only(&ExecRequest::with_host(
            &SessionSpec::new(vec![ls.display().to_string()]),
            &host,
        ));
        assert!(read_only, "a read-only programme classifies as one");

        let rm = program(dir.path(), "rm");
        #[allow(deprecated)]
        let read_only = classify_read_only(&ExecRequest::with_host(
            &SessionSpec::new(vec![rm.display().to_string()]),
            &host,
        ));
        assert!(!read_only, "and a writer does not");
    }

    /// A host policy whose only programme directory is `dir`.
    #[cfg_attr(not(unix), allow(dead_code))]
    fn host(dir: &Path) -> HostPolicy {
        HostPolicy {
            path_dirs: vec![std::fs::canonicalize(dir).expect("canonicalize")],
        }
    }

    #[cfg(unix)]
    fn program(dir: &Path, name: &str) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let path = dir.join(name);
        std::fs::write(&path, "#!/bin/sh\nexit 0\n").expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        std::fs::canonicalize(path).expect("canonicalize")
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_read_only_command_is_stripped_of_its_execution_switches() {
        let dir = tempfile::tempdir().expect("tempdir");
        program(dir.path(), "git");
        let sink = Arc::new(Sink::default());
        let runner = harden_read_only(host(dir.path()))(sink.clone());

        let mut spec = SessionSpec::new(vec!["git".to_owned(), "log".to_owned()]);
        spec.opts.write = WriteMode::ReadOnly;
        for name in ["LESSOPEN", "GIT_PAGER", "PAGER"] {
            spec.opts
                .env
                .inject
                .insert(name.to_owned(), "sh /tmp/evil.sh".to_owned());
        }
        spec.opts
            .env
            .inject
            .insert("RUN_ID".to_owned(), "7".to_owned());

        // The sink refuses to start, so the layer's own decision is observable
        // only through the spec it handed on.
        let _ = runner.start(spec).await;
        let hardened = sink.take().expect("the layer must pass the spec on");
        assert_eq!(
            hardened.argv,
            [
                "git".to_owned(),
                "--no-pager".to_owned(),
                "-c".to_owned(),
                "core.fsmonitor=false".to_owned(),
                "log".to_owned(),
            ]
        );
        for name in ["LESSOPEN", "GIT_PAGER", "PAGER"] {
            assert!(
                !hardened.opts.env.inject.contains_key(name),
                "{name} must be gone, not empty"
            );
        }
        assert_eq!(
            hardened.opts.env.inject.get("RUN_ID").map(String::as_str),
            Some("7"),
            "hardening removes switches, not the caller's context"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_man_pager_is_replaced_by_a_reader() {
        let dir = tempfile::tempdir().expect("tempdir");
        program(dir.path(), "man");
        let sink = Arc::new(Sink::default());
        let runner = harden_read_only(host(dir.path()))(sink.clone());
        let _ = runner
            .start(SessionSpec::new(vec!["man".to_owned(), "ls".to_owned()]))
            .await;
        let hardened = sink.take().expect("the layer must pass the spec on");
        assert_eq!(
            hardened.argv,
            [
                "man".to_owned(),
                "-P".to_owned(),
                "cat".to_owned(),
                "ls".to_owned(),
            ]
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_refused_command_never_reaches_the_runner() {
        let dir = tempfile::tempdir().expect("tempdir");
        program(dir.path(), "git");
        let sink = Arc::new(Sink::default());
        let runner = harden_read_only(host(dir.path()))(sink.clone());
        let error = runner
            .start(SessionSpec::new(vec![
                "git".to_owned(),
                "grep".to_owned(),
                "-O".to_owned(),
                "sh".to_owned(),
                "x".to_owned(),
            ]))
            .await
            .expect_err("refused");
        assert_eq!(error.kind(), crate::error::ErrorKind::PolicyDenied);
        assert!(sink.take().is_none(), "the runner must not be reached");
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn a_command_that_is_not_read_only_is_passed_through_untouched() {
        let dir = tempfile::tempdir().expect("tempdir");
        program(dir.path(), "git");
        let sink = Arc::new(Sink::default());
        let runner = harden_read_only(host(dir.path()))(sink.clone());
        let mut spec = SessionSpec::new(vec!["git".to_owned(), "push".to_owned()]);
        spec.opts
            .env
            .inject
            .insert("GIT_PAGER".to_owned(), "cat".to_owned());
        let _ = runner.start(spec.clone()).await;
        let passed = sink.take().expect("the layer must pass the spec on");
        assert_eq!(
            passed, spec,
            "an unproven command is the approver's, not this layer's"
        );
        // The write mode is not this layer's business either.
        let mut write_only = SessionSpec::new(vec!["git".to_owned(), "push".to_owned()]);
        write_only.opts = ExecOptions {
            write: WriteMode::Workspace,
            ..ExecOptions::default()
        };
        let _ = runner.start(write_only.clone()).await;
        let passed = sink.take().expect("the layer must pass the spec on");
        assert_eq!(passed.opts.write, WriteMode::Workspace);
    }
}
