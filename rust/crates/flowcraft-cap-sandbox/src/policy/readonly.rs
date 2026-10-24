//! The read-only classifier: "may this be auto-approved without asking?".
//!
//! Deliberately conservative: `false` means **"not proven read-only"**, and the
//! answer to that is to route the call to the approver — never to deny it and
//! never to allow it. A false negative costs a prompt; a false positive costs the
//! guarantee.
//!
//! It pairs with the per-call write mode rather than replacing it:
//!
//! ```text
//! if request.opts.write == WriteMode::ReadOnly && classify_read_only(&request) { Approve }
//! ```
//!
//! Commands that can run other programs or touch non-file state are excluded on
//! purpose: `env`, `xargs`, `awk`, `tar`, interpreters, package managers, and
//! also `date` / `hostname` (`date -s` and `hostname newname` change system state
//! that no filesystem sandbox blocks).

use crate::contract::SessionSpec;

use super::allowlist::normalise;
use super::approval::ExecRequest;

/// Whether `request` is a known read-only command.
#[must_use]
pub fn classify_read_only(request: &ExecRequest) -> bool {
    classify_tokens(&request.tokens)
}

/// Whether `spec` is a known read-only command.
#[must_use]
pub fn classify_spec(spec: &SessionSpec) -> bool {
    classify_tokens(&normalise(spec))
}

fn classify_tokens(tokens: &[String]) -> bool {
    let Some(program) = tokens.first() else {
        return false;
    };
    let base = std::path::Path::new(program)
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or(program.as_str());
    let args = &tokens[1..];
    match base {
        "find" => find_is_read_only(args),
        "rg" | "ripgrep" => rg_is_read_only(args),
        "git" => git_is_read_only(args),
        "sed" => sed_is_read_only(args),
        "sort" => sort_is_read_only(args),
        other => is_read_only_base(other),
    }
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
            | "less"
            | "ls"
            | "man"
            | "md5sum"
            | "more"
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
fn git_is_read_only(args: &[String]) -> bool {
    let mut subcommand: Option<&str> = None;
    let mut rest: &[String] = &[];
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
            return false;
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
        subcommand = Some(arg);
        rest = &args[index + 1..];
        break;
    }
    // Bare `git` (or `git --version`) prints help/version.
    let Some(subcommand) = subcommand else {
        return true;
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
        return false;
    }
    !rest.iter().any(|arg| {
        arg == "--output"
            || arg.starts_with("--output=")
            || ["--ext-diff", "--textconv", "--exec"]
                .iter()
                .any(|flag| arg == flag || arg.starts_with(&format!("{flag}=")))
    })
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

    fn request(argv: &[&str]) -> ExecRequest {
        let spec = SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect());
        ExecRequest::from_spec(&spec)
    }

    #[test]
    fn plain_readers_are_read_only() {
        for argv in [
            vec!["ls", "-la"],
            vec!["cat", "file"],
            vec!["grep", "-n", "x", "f"],
            vec!["wc", "-l", "f"],
            vec!["/bin/pwd"],
        ] {
            assert!(classify_read_only(&request(&argv)), "{argv:?}");
        }
    }

    #[test]
    fn a_programme_that_can_run_or_write_is_not() {
        for argv in [
            vec!["env", "FOO=1", "rm"],
            vec!["xargs", "rm"],
            vec!["awk", "BEGIN{system(\"rm x\")}"],
            // A *simple* wrapped command is classified through the wrapper (see
            // the dedicated test); a composite one is not.
            vec!["sh", "-c", "ls && rm x"],
            vec!["tee", "f"],
            vec!["cp", "a", "b"],
            vec!["date", "-s", "now"],
            vec!["hostname", "newname"],
        ] {
            assert!(!classify_read_only(&request(&argv)), "{argv:?}");
        }
    }

    #[test]
    fn find_with_a_writing_action_is_not_read_only() {
        assert!(classify_read_only(&request(&[
            "find", ".", "-name", "*.rs"
        ])));
        for action in ["-delete", "-exec", "-execdir", "-ok", "-fls"] {
            assert!(
                !classify_read_only(&request(&["find", ".", action, "x"])),
                "{action}"
            );
        }
        assert!(!classify_read_only(&request(&[
            "find", ".", "-fprint", "out"
        ])));
    }

    #[test]
    fn ripgrep_with_a_preprocessor_is_not_read_only() {
        assert!(classify_read_only(&request(&["rg", "-n", "needle", "."])));
        assert!(!classify_read_only(&request(&["rg", "--pre", "./x", "y"])));
        assert!(!classify_read_only(&request(&["rg", "--generate", "man"])));
        // The CVE-2025-54558 class: rg running another program or a decompressor.
        assert!(!classify_read_only(&request(&[
            "rg",
            "--hostname-bin=./x",
            "a",
            "f"
        ])));
        assert!(!classify_read_only(&request(&["rg", "-z", "p", "f.zip"])));
        assert!(!classify_read_only(&request(&[
            "rg",
            "--search-zip",
            "p",
            "f"
        ])));
        assert!(!classify_read_only(&request(&["rg", "-nz", "p", "f"])));
    }

    #[test]
    fn git_is_read_only_only_for_read_subcommands() {
        for sub in ["status", "log", "diff", "show", "rev-parse", "ls-files"] {
            assert!(
                classify_read_only(&request(&["git", sub])),
                "git {sub} must classify as read-only"
            );
        }
        for sub in [
            "push", "commit", "add", "checkout", "clean", "reset", "stash", "config", "branch",
            "tag", "remote", "fetch",
        ] {
            assert!(
                !classify_read_only(&request(&["git", sub])),
                "git {sub} must not"
            );
        }
        assert!(!classify_read_only(&request(&["git", "log", "--output=f"])));
        // A subcommand that writes, and options that run a program.
        assert!(!classify_read_only(&request(&[
            "git",
            "symbolic-ref",
            "HEAD",
            "refs/heads/x"
        ])));
        assert!(!classify_read_only(&request(&[
            "git",
            "-c",
            "core.pager=x",
            "log"
        ])));
        assert!(!classify_read_only(&request(&[
            "git",
            "diff",
            "--ext-diff"
        ])));
        assert!(!classify_read_only(&request(&["git", "log", "--textconv"])));
        // A value-taking global option is skipped when locating the subcommand.
        assert!(classify_read_only(&request(&["git", "-C", "d", "status"])));
    }

    #[test]
    fn sort_to_a_file_is_not_read_only() {
        assert!(classify_read_only(&request(&["sort", "-u", "f"])));
        assert!(!classify_read_only(&request(&["sort", "-o", "out", "f"])));
        assert!(!classify_read_only(&request(&[
            "sort",
            "--output=out",
            "f"
        ])));
    }

    #[test]
    fn a_wrapped_simple_command_is_classified_through_the_wrapper() {
        assert!(classify_read_only(&request(&["sh", "-c", "ls -la"])));
        assert!(!classify_read_only(&request(&["sh", "-c", "ls; rm x"])));
    }
}
