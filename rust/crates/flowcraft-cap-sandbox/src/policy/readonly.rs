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
//! if request.opts.write == WriteOnly && classify_read_only(&request) { Approve }
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

/// `ripgrep` can run a preprocessor and write generated artefacts.
fn rg_is_read_only(args: &[String]) -> bool {
    !args
        .iter()
        .any(|arg| arg.starts_with("--pre") || arg.starts_with("--generate"))
}

/// `git` read subcommands, minus the ones with a writing flag.
fn git_is_read_only(args: &[String]) -> bool {
    let Some(subcommand) = args.iter().find(|arg| !arg.starts_with('-')) else {
        return false;
    };
    let subcommand = subcommand.as_str();
    let allowed = matches!(
        subcommand,
        "status"
            | "log"
            | "diff"
            | "show"
            | "rev-parse"
            | "rev-list"
            | "ls-files"
            | "ls-tree"
            | "blame"
            | "grep"
            | "describe"
            | "cat-file"
            | "shortlog"
            | "show-ref"
            | "name-rev"
            | "merge-base"
            | "whatchanged"
            | "symbolic-ref"
            | "for-each-ref"
    );
    allowed && !args.iter().any(|arg| arg.starts_with("--output"))
}

/// `sed` can edit in place and write files from a script.
fn sed_is_read_only(args: &[String]) -> bool {
    for arg in args {
        if arg.starts_with("-i") || arg.starts_with("--in-place") {
            return false;
        }
        if arg == "-e" || arg == "--expression" {
            continue;
        }
        if sed_script_writes(arg) {
            return false;
        }
    }
    true
}

/// Whether a `sed` script contains the `w` (write file) or `e` (execute)
/// command. Split on `;` and newlines, then look at the first token of each part.
fn sed_script_writes(script: &str) -> bool {
    script.split([';', '\n', '}']).any(|part| {
        let part = part.trim_start();
        part.starts_with("w ")
            || part == "w"
            || part.starts_with("w/")
            || part.starts_with("e ")
            || part == "e"
            || part.contains("/w ")
    })
}

/// `sort` can write its result to a file.
fn sort_is_read_only(args: &[String]) -> bool {
    !args
        .iter()
        .any(|arg| arg == "-o" || arg.starts_with("-o") || arg.starts_with("--output"))
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
    }

    #[test]
    fn sed_in_place_and_script_writes_are_not_read_only() {
        assert!(classify_read_only(&request(&["sed", "-n", "1,5p", "f"])));
        assert!(!classify_read_only(&request(&["sed", "-i", "s/a/b/", "f"])));
        assert!(!classify_read_only(&request(&[
            "sed",
            "-n",
            "w out.txt",
            "f"
        ])));
        assert!(!classify_read_only(&request(&["sed", "e whoami", "f"])));
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
