//! Command rules: what may run without asking a human.
//!
//! A rule is a whitespace-separated token prefix of the **normalised** command,
//! with an optional trailing `*`:
//!
//! ```text
//! go run *      matches "go run main.go", "go run test", and "go run"
//! go *          matches any "go" invocation
//! git status    matches only "git status"
//! ```
//!
//! The first token is compared against the command's base name as well as its
//! literal form, so `/usr/bin/go` satisfies `go *`.
//!
//! Normalisation is deliberately conservative: a shell wrapper is unwrapped only
//! in its exact form (`sh -c`, `cmd /c`, `pwsh -NoProfile -Command`) and only
//! when the script is a single simple command. Anything else — a login shell,
//! `cmd /k`, an expansion, a pipeline, a redirect — stays raw and goes to the
//! approver. Like every predicate here, the allowlist is the tripwire, not the
//! wall: backend enforcement remains the boundary.

use std::sync::RwLock;

use flowcraft_error::{Error, Result};

use crate::contract::SessionSpec;

/// The shells whose exact `-c` form is unwrapped.
const SHELLS: &[&str] = &["sh", "bash", "dash", "zsh", "ksh", "ash"];

/// One parsed rule.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Rule {
    tokens: Vec<String>,
    /// Whether a trailing `*` allows extra arguments.
    wildcard: bool,
}

impl Rule {
    fn parse(raw: &str) -> Result<Self> {
        if raw.chars().any(char::is_control) {
            return Err(Error::validation(format!(
                "sandbox: allowlist rule {raw:?} contains a control character"
            )));
        }
        let mut tokens: Vec<&str> = raw.split_whitespace().collect();
        if tokens.is_empty() {
            return Err(Error::validation("sandbox: allowlist rule is empty"));
        }
        let wildcard = tokens.last() == Some(&"*");
        if wildcard {
            tokens.pop();
        }
        if tokens.is_empty() {
            // A bare `*` would approve everything; refuse instead of being
            // silently permissive.
            return Err(Error::validation(
                "sandbox: allowlist rule \"*\" matches every command",
            ));
        }
        if tokens.contains(&"*") {
            return Err(Error::validation(format!(
                "sandbox: allowlist rule {raw:?}: '*' is only allowed as the last token"
            )));
        }
        Ok(Self {
            tokens: tokens.into_iter().map(str::to_owned).collect(),
            wildcard,
        })
    }

    /// The rule spelled the way `parse` accepts it.
    fn to_raw(&self) -> String {
        let mut raw = self.tokens.join(" ");
        if self.wildcard {
            raw.push_str(" *");
        }
        raw
    }

    fn matches(&self, command: &[String]) -> bool {
        if command.is_empty() {
            return false;
        }
        if !names_match(&self.tokens[0], &command[0]) {
            return false;
        }
        let rest = &command[1..];
        let expected = &self.tokens[1..];
        if self.wildcard {
            rest.len() >= expected.len() && rest.iter().zip(expected).all(|(a, b)| a == b)
        } else {
            rest.len() == expected.len() && rest.iter().zip(expected).all(|(a, b)| a == b)
        }
    }
}

/// Compare two command names by literal form or by base name.
fn names_match(rule: &str, command: &str) -> bool {
    rule == command || base_name(rule) == base_name(command)
}

fn base_name(token: &str) -> &str {
    std::path::Path::new(token)
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or(token)
}

/// A thread-safe set of command rules.
#[derive(Debug, Default)]
pub struct Allowlist {
    rules: RwLock<Vec<Rule>>,
}

impl Allowlist {
    /// Build an allowlist from rules. One invalid rule leaves it empty.
    pub fn parse(rules: impl IntoIterator<Item = String>) -> Result<Self> {
        let parsed = rules
            .into_iter()
            .map(|raw| Rule::parse(&raw))
            .collect::<Result<Vec<_>>>()?;
        Ok(Self {
            rules: RwLock::new(parsed),
        })
    }

    /// Add rules. Every rule is validated before any is applied.
    pub fn add(&self, rules: impl IntoIterator<Item = String>) -> Result<()> {
        let parsed = rules
            .into_iter()
            .map(|raw| Rule::parse(&raw))
            .collect::<Result<Vec<_>>>()?;
        let mut guard = self
            .rules
            .write()
            .map_err(|_| Error::internal("sandbox: allowlist lock poisoned"))?;
        for rule in parsed {
            if !guard.contains(&rule) {
                guard.push(rule);
            }
        }
        Ok(())
    }

    /// A new allowlist with both sets of rules.
    pub fn union(&self, other: &Self) -> Result<Self> {
        let combined = Self::default();
        for list in [self, other] {
            combined.add(list.raw_rules()?)?;
        }
        Ok(combined)
    }

    /// The rules as they were written, for `union` to re-parse.
    fn raw_rules(&self) -> Result<Vec<String>> {
        let guard = self
            .rules
            .read()
            .map_err(|_| Error::internal("sandbox: allowlist lock poisoned"))?;
        Ok(guard.iter().map(Rule::to_raw).collect())
    }

    /// Whether `command` (already normalised) is pre-approved.
    #[must_use]
    pub fn allows(&self, command: &[String]) -> bool {
        let Ok(guard) = self.rules.read() else {
            // A poisoned allowlist approves nothing.
            return false;
        };
        guard.iter().any(|rule| rule.matches(command))
    }

    /// How many rules are loaded.
    #[must_use]
    pub fn len(&self) -> usize {
        self.rules.read().map(|rules| rules.len()).unwrap_or(0)
    }

    /// Whether the list has no rules (and therefore approves nothing).
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// The tokens a rule is matched against.
#[must_use]
pub fn normalise(spec: &SessionSpec) -> Vec<String> {
    normalise_argv(&spec.argv)
}

/// The tokens a rule is matched against, for a bare argv.
#[must_use]
pub fn normalise_argv(argv: &[String]) -> Vec<String> {
    let Some(program) = argv.first() else {
        return Vec::new();
    };
    let base = base_name(program);
    if let Some(script) = unwrap_script(base, &argv[1..])
        && let Some(tokens) = tokenize(script)
    {
        return tokens;
    }
    argv.to_vec()
}

/// The script a wrapper runs, when the wrapper is in its exact supported form.
fn unwrap_script<'a>(base: &str, rest: &'a [String]) -> Option<&'a str> {
    let at = |index: usize| rest.get(index).map(String::as_str);
    if SHELLS.contains(&base) && rest.len() == 2 && at(0) == Some("-c") {
        return at(1);
    }
    if (base == "cmd" || base == "cmd.exe") && rest.len() == 2 && at(0) == Some("/c") {
        return at(1);
    }
    let windows_shell = base == "pwsh" || base == "pwsh.exe" || base == "powershell";
    if windows_shell && rest.len() == 3 && at(0) == Some("-NoProfile") && at(1) == Some("-Command")
    {
        return at(2);
    }
    None
}

/// Split a script into tokens, or refuse when it is more than one simple
/// command.
///
/// Refusal conditions (all conservative): an unquoted metacharacter, or any
/// expansion character anywhere (`$`, backtick) — a body that can compute its own
/// arguments is not a body a rule should be matched against.
fn tokenize(script: &str) -> Option<Vec<String>> {
    let mut tokens = Vec::new();
    let mut current = String::new();
    let mut started = false;
    let mut chars = script.chars().peekable();

    while let Some(ch) = chars.next() {
        match ch {
            '\'' => {
                started = true;
                loop {
                    match chars.next() {
                        Some('\'') => break,
                        Some(inner) => current.push(inner),
                        None => return None,
                    }
                }
            }
            '"' => {
                started = true;
                loop {
                    match chars.next() {
                        Some('"') => break,
                        Some('\\') => match chars.next() {
                            Some(escaped @ ('"' | '\\')) => current.push(escaped),
                            _ => return None,
                        },
                        Some(inner @ ('$' | '`')) => {
                            let _ = inner;
                            return None;
                        }
                        Some(inner) => current.push(inner),
                        None => return None,
                    }
                }
            }
            '\\' => {
                started = true;
                match chars.next() {
                    Some(escaped) => current.push(escaped),
                    None => return None,
                }
            }
            '$' | '`' => return None,
            ';' | '|' | '&' | '<' | '>' | '(' | ')' | '\n' | '\r' => return None,
            ch if ch.is_whitespace() => {
                if started {
                    tokens.push(std::mem::take(&mut current));
                    started = false;
                }
            }
            ch => {
                started = true;
                current.push(ch);
            }
        }
    }
    if started {
        tokens.push(current);
    }
    if tokens.is_empty() {
        None
    } else {
        Some(tokens)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(argv: &[&str]) -> SessionSpec {
        SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect())
    }

    fn allow(rules: &[&str]) -> Allowlist {
        Allowlist::parse(rules.iter().map(|rule| (*rule).to_owned())).expect("rules")
    }

    #[test]
    fn a_trailing_star_allows_more_arguments() {
        let list = allow(&["go run *"]);
        assert!(list.allows(&normalise(&spec(&["go", "run", "main.go"]))));
        assert!(list.allows(&normalise(&spec(&["go", "run"]))));
        assert!(!list.allows(&normalise(&spec(&["go", "test"]))));
    }

    #[test]
    fn without_a_star_the_match_is_exact() {
        let list = allow(&["git status"]);
        assert!(list.allows(&normalise(&spec(&["git", "status"]))));
        assert!(!list.allows(&normalise(&spec(&["git", "status", "-s"]))));
        assert!(!list.allows(&normalise(&spec(&["git", "push"]))));
    }

    #[test]
    fn a_command_matches_by_base_name() {
        let list = allow(&["go *"]);
        assert!(list.allows(&normalise(&spec(&["/usr/local/bin/go", "build"]))));
    }

    #[test]
    fn an_empty_list_approves_nothing() {
        let list = Allowlist::default();
        assert!(list.is_empty());
        assert!(!list.allows(&normalise(&spec(&["rm"]))));
    }

    #[test]
    fn invalid_rules_are_rejected_and_leave_the_list_unchanged() {
        assert!(Allowlist::parse([String::new()]).is_err());
        assert!(Allowlist::parse(["*".to_owned()]).is_err());
        assert!(Allowlist::parse(["a * b".to_owned()]).is_err());
        let list = allow(&["ls"]);
        assert!(list.add(["bad * mid".to_owned()]).is_err());
        assert_eq!(list.len(), 1);
    }

    #[test]
    fn adding_the_same_rule_twice_is_idempotent() {
        let list = allow(&["ls"]);
        list.add(["ls".to_owned()]).expect("add");
        assert_eq!(list.len(), 1);
    }

    #[test]
    fn a_union_contains_both_sides() {
        let left = allow(&["ls"]);
        let right = allow(&["cat"]);
        let union = left.union(&right).expect("union");
        assert!(union.allows(&["ls".to_owned()]));
        assert!(union.allows(&["cat".to_owned()]));
        assert!(!union.allows(&["rm".to_owned()]));
    }

    #[test]
    fn a_simple_shell_script_is_unwrapped() {
        let tokens = normalise(&spec(&["sh", "-c", "rg -n needle ."]));
        assert_eq!(tokens, ["rg", "-n", "needle", "."]);
        assert!(allow(&["rg *"]).allows(&tokens));
    }

    #[test]
    fn a_quoted_argument_survives_the_unwrap() {
        let tokens = normalise(&spec(&["sh", "-c", "echo 'a b'"]));
        assert_eq!(tokens, ["echo", "a b"]);
    }

    #[test]
    fn a_login_shell_is_never_unwrapped() {
        for argv in [
            vec!["bash", "-lc", "ls"],
            vec!["sh", "-lc", "ls"],
            vec!["cmd", "/k", "dir"],
            vec!["pwsh", "-Command", "ls"],
        ] {
            let tokens = normalise(&spec(&argv));
            assert_eq!(tokens[0], argv[0], "{argv:?} must stay raw");
        }
    }

    #[test]
    fn a_composite_script_is_never_unwrapped() {
        for script in [
            "ls; rm -rf /",
            "ls && rm -rf /",
            "cat f | sh",
            "echo x > f",
            "echo $(whoami)",
            "echo `whoami`",
            "echo $HOME",
            "ls\nrm",
        ] {
            let tokens = normalise(&spec(&["sh", "-c", script]));
            assert_eq!(tokens[0], "sh", "{script:?} must stay raw");
        }
    }

    #[test]
    fn the_exact_wrapper_forms_are_unwrapped() {
        assert_eq!(
            normalise(&spec(&["cmd.exe", "/c", "dir"])),
            ["dir".to_owned()]
        );
        assert_eq!(
            normalise(&spec(&["pwsh", "-NoProfile", "-Command", "Get-ChildItem"])),
            ["Get-ChildItem".to_owned()]
        );
    }

    #[test]
    fn an_empty_argv_normalises_to_nothing() {
        assert!(normalise_argv(&[]).is_empty());
    }
}
