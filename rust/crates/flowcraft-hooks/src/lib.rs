//! `core/hooks`: external command hooks, loaded from a `hooks.json` file.
//!
//! A `hooks.json` maps lifecycle event names to shell commands, and every
//! matching command receives one JSON event object on stdin. This is the whole
//! of `hooks.go` and `run.go`; the two files that wire the runner into a
//! deployment (`observer.go`, `resource.go`) are deferred -- see the last
//! section.
//!
//! # Transport contract
//!
//! The contract between the platform and a hook command is an ABI, so it is
//! stated here rather than inferred from the code:
//!
//! - The event vocabulary is frozen and append-only: [`EVENT_NAMES`]. Adding an
//!   event is an interface change, renaming one is a breaking change.
//! - Every invocation receives exactly one JSON object on stdin. It always
//!   carries `event`, stamped by the runner rather than trusted from the
//!   producer, plus the event-specific fields the producer supplies.
//! - A command runs under `sh -c`, in the source's directory when it has one,
//!   with the hook's timeout (30s by default) bounding it. On expiry the whole
//!   process group is killed, so a hook that spawned descendants does not leave
//!   them behind.
//! - The first 64 KiB of combined output are kept, and logged.
//! - Failure never propagates. A hook that times out, exits non-zero, or cannot
//!   even start is logged and skipped: user hooks are advisory and an agent loop
//!   must not stop because a script broke. [`Manager::fire`] returns no error for
//!   that reason.
//!
//! # Sources and trust
//!
//! A source is the user's `hooks.json` (loaded first, trusted), an
//! application-injected [`ExtraSource`], or a plugin-contributed file (loaded
//! untrusted: the runner strips the content-bearing fields -- tool inputs, tool
//! results, prompts, commands, errors, messages, targets -- before the command
//! runs, since plugin content is third-party input). A missing primary file
//! yields a runner with no groups; a source that fails to load is skipped with a
//! warning, so one broken plugin cannot disarm every hook in a deployment.
//!
//! # What the port changed
//!
//! - [`Manager::fire`] is `async`, and takes the warning sink as an argument.
//!   Core's version blocks the calling goroutine and logs through
//!   `telemetry.Warn`; here a hook is a subprocess to await, and a *sink* is
//!   where those same six warnings go until the telemetry slice owns one. The
//!   messages are core's, verbatim, so the sink's first real consumer sees what
//!   Go's log showed.
//! - Not ported, with where each belongs: [`Observer`](observer) -- the
//!   subagent-event adapter over the delegation kanban, which is the S8 slice),
//!   and the `resource.Factory` half (`hooks.Runner`, `hooks.SubagentObserver`)
//!   -- the registry is the S4 slice. Nothing in those two files decides what a
//!   hook command sees, so the ABI above is complete without them.
//! - A matcher's *error text* is the regex library's, and the two libraries
//!   word it differently. Everything else in a load failure is core's sentence;
//!   the contract masks the pattern's tail rather than pretending to compare it.

mod exec;
mod run;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use flowcraft_contract::error::{Error, ErrorKind};
use regex::Regex;
use serde::{Deserialize, Serialize};

pub use run::{
    Capture, MAX_OUTPUT, SHELL, STRIPPED, Silent, WAIT_DELAY, WARNING_MESSAGES, Warn, Warning,
};

/// `PreToolUse`: before a tool runs.
pub const EVENT_PRE_TOOL_USE: &str = "PreToolUse";
/// `PostToolUse`: after a tool ran.
pub const EVENT_POST_TOOL_USE: &str = "PostToolUse";
/// `UserPromptSubmit`: the user submitted a prompt.
pub const EVENT_USER_PROMPT_SUBMIT: &str = "UserPromptSubmit";
/// `PermissionRequest`: a permission is being asked for.
pub const EVENT_PERMISSION_REQUEST: &str = "PermissionRequest";
/// `TurnEnd`: a turn ended.
pub const EVENT_TURN_END: &str = "TurnEnd";
/// `SessionStart`: a session started.
pub const EVENT_SESSION_START: &str = "SessionStart";
/// `SessionEnd`: a session ended.
pub const EVENT_SESSION_END: &str = "SessionEnd";
/// `SubagentStart`: a delegated run started.
pub const EVENT_SUBAGENT_START: &str = "SubagentStart";
/// `SubagentStop`: a delegated run finished.
pub const EVENT_SUBAGENT_STOP: &str = "SubagentStop";

/// The vocabulary in canonical order: core's `EventNames`, and the order
/// `hooks: ... unknown event` lists. Frozen and append-only.
pub const EVENT_NAMES: [&str; 9] = [
    EVENT_PRE_TOOL_USE,
    EVENT_POST_TOOL_USE,
    EVENT_USER_PROMPT_SUBMIT,
    EVENT_PERMISSION_REQUEST,
    EVENT_TURN_END,
    EVENT_SESSION_START,
    EVENT_SESSION_END,
    EVENT_SUBAGENT_START,
    EVENT_SUBAGENT_STOP,
];

/// How long a command hook whose config omits a timeout may take
/// (`defaultTimeout`).
pub const DEFAULT_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);

/// The canonical order as an owned list, the way core's `EventNames` hands it
/// out: a caller that wants to sort or filter gets its own copy.
pub fn event_names() -> Vec<&'static str> {
    EVENT_NAMES.to_vec()
}

/// The canonical spelling of `event`, if it is in the vocabulary.
pub fn known_event(event: &str) -> Option<&'static str> {
    EVENT_NAMES.iter().copied().find(|name| *name == event)
}

/// One command invocation declared in a `hooks.json` group.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Hook {
    /// Selects the hook kind. Empty and `command` are command hooks; any other
    /// value is skipped when the file loads, so a config written for a later
    /// runner version still loads here. Core spells the field `Type` and the
    /// JSON key `type`, which is a keyword here.
    #[serde(rename = "type", default, skip_serializing_if = "String::is_empty")]
    pub kind: String,
    /// The shell command line, run under `sh -c`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command: String,
    /// Timeout in seconds. Zero or negative means [`DEFAULT_TIMEOUT`].
    #[serde(default, skip_serializing_if = "is_zero")]
    pub timeout: i64,
}

impl Hook {
    /// Whether this hook is one the runner executes: core skips a hook whose
    /// `type` names a kind it does not know, rather than refusing the file.
    pub fn runs(&self) -> bool {
        self.kind.is_empty() || self.kind == "command"
    }
}

/// Whether a hook names no timeout, which is what `omitempty` on core's field
/// means: seconds of zero are the default, not a zero-length budget.
fn is_zero(value: &i64) -> bool {
    *value == 0
}

/// Matches one event occurrence and runs its hooks in order.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Group {
    /// A regular expression tested against the occurrence's match value, the
    /// first non-empty of the payload's `tool` / `source` / `reason` /
    /// `subagent` fields. Empty and `*` match every occurrence.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub matcher: String,
    /// The commands this group runs.
    #[serde(default)]
    pub hooks: Vec<Hook>,
}

/// One additional `hooks.json` file, normally contributed by a plugin.
///
/// `dir` anchors the file's commands: it becomes their working directory.
/// `trusted` marks a source whose commands may see the full payload; the zero
/// value strips the content-bearing fields before the command runs.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ExtraSource {
    /// The file to load.
    pub path: PathBuf,
    /// The working directory of this source's commands.
    pub dir: PathBuf,
    /// Whether this source's commands may see the full payload.
    pub trusted: bool,
}

/// The producer's payload: `core/hooks`' `map[string]any`, as JSON.
///
/// A map rather than a typed enum because the vocabulary is the *event*, not
/// the fields: a producer supplies whichever fields its event documents, and
/// [`event_body`] hands them on unchanged. The type is `serde_json`'s map, which
/// sorts its keys on encode -- the same order Go's `encoding/json` gives a map,
/// which is what makes two bodies comparable byte for byte.
pub type Payload = serde_json::Map<String, serde_json::Value>;

/// One loaded group: a compiled matcher plus the source facts its hooks inherit.
#[derive(Debug)]
struct GroupEntry {
    matcher: Option<Regex>,
    hooks: Vec<Hook>,
    dir: PathBuf,
    trusted: bool,
}

/// A loaded set of hook groups, one runner per deployment.
///
/// Safe for concurrent use: [`fire`](Manager::fire) reads the groups and writes
/// nothing, and the commands it runs are separate processes.
#[derive(Debug)]
pub struct Manager {
    path: PathBuf,
    groups: BTreeMap<&'static str, Vec<GroupEntry>>,
}

impl Manager {
    /// Parse the `hooks.json` file at `path`. A missing file is not an error: it
    /// yields a manager with no groups.
    pub fn load(path: impl AsRef<Path>) -> Result<Manager, Error> {
        Manager::load_with_sources(path, &[], &run::Silent)
    }

    /// Parse the primary `hooks.json` plus every extra source.
    ///
    /// A missing primary file is fine; a primary file that does not parse is an
    /// error, because a user's own misconfiguration should be loud. An extra
    /// source that cannot be read or parsed is skipped with a warning instead: a
    /// broken plugin must not take the runner down with it.
    pub fn load_with_sources(
        path: impl AsRef<Path>,
        extra: &[ExtraSource],
        warn: &dyn Warn,
    ) -> Result<Manager, Error> {
        let mut manager = Manager {
            path: path.as_ref().to_path_buf(),
            groups: BTreeMap::new(),
        };
        manager.load_file(path.as_ref(), Path::new(""), true, true)?;
        for source in extra {
            if source.path.as_os_str().is_empty() {
                continue;
            }
            if let Err(err) = manager.load_file(&source.path, &source.dir, source.trusted, false) {
                warn.warn(&Warning::new(
                    run::SKIPPING_SOURCE,
                    [
                        ("path", source.path.display().to_string()),
                        ("error", err.to_string()),
                    ],
                ));
            }
        }
        Ok(manager)
    }

    /// Read one `hooks.json` into the manager. An `optional` source may be
    /// absent; a required one reports it.
    fn load_file(
        &mut self,
        path: &Path,
        dir: &Path,
        trusted: bool,
        optional: bool,
    ) -> Result<(), Error> {
        let data = match std::fs::read(path) {
            Ok(data) => data,
            Err(err) if optional && err.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(err) => return Err(read_failure(path, &err)),
        };
        let config: ConfigFile = serde_json::from_slice(&data).map_err(|err| {
            Error::new(
                ErrorKind::Validation,
                format!("hooks: parse {}: {err}", path.display()),
            )
        })?;
        // Sorted keys, so loading reports the first error in file order instead
        // of map order: a file with two faults names the same one twice.
        for (name, groups) in &config.hooks {
            let Some(event) = known_event(name) else {
                return Err(unknown_event(path, name));
            };
            for (index, group) in groups.iter().enumerate() {
                let Some(entry) = new_group_entry(group, dir, trusted)
                    .map_err(|reason| group_failure(path, event, index, &reason))?
                else {
                    continue;
                };
                self.groups.entry(event).or_default().push(entry);
            }
        }
        Ok(())
    }

    /// The primary `hooks.json` path.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Whether the manager holds no hook groups at all.
    pub fn empty(&self) -> bool {
        self.groups.is_empty()
    }

    /// Run every group matching `event` and `payload`, in load order, running
    /// each group's hooks in declaration order.
    ///
    /// Never returns an error: hook failures are logged to `warn` and skipped, so
    /// user hooks can never block the agent loop. `payload` is read, never
    /// mutated; the runner stamps `event` itself.
    pub async fn fire(&self, event: &str, payload: &Payload, warn: &dyn Warn) {
        let Some(entries) = known_event(event).and_then(|name| self.groups.get(name)) else {
            return;
        };
        let value = match_value(payload);
        for entry in entries {
            if let Some(matcher) = &entry.matcher
                && !matcher.is_match(value)
            {
                continue;
            }
            for hook in &entry.hooks {
                run::run(event, entry, hook, payload, warn).await;
            }
        }
    }
}

/// The on-disk `hooks.json` shape.
#[derive(Debug, Deserialize)]
struct ConfigFile {
    #[serde(default)]
    hooks: BTreeMap<String, Vec<Group>>,
}

/// Compile one group, dropping the hook kinds this runner does not run. A group
/// left with no hooks is `None`.
fn new_group_entry(group: &Group, dir: &Path, trusted: bool) -> Result<Option<GroupEntry>, String> {
    let matcher = group.matcher.trim();
    let matcher = match matcher {
        "" | "*" => None,
        pattern => Some(Regex::new(pattern).map_err(|err| format!("matcher: {err}"))?),
    };
    let mut hooks = Vec::with_capacity(group.hooks.len());
    for hook in &group.hooks {
        if !hook.runs() {
            continue;
        }
        if hook.command.trim().is_empty() {
            return Err("hook command is required".to_owned());
        }
        hooks.push(hook.clone());
    }
    if hooks.is_empty() {
        return Ok(None);
    }
    Ok(Some(GroupEntry {
        matcher,
        hooks,
        dir: dir.to_path_buf(),
        trusted,
    }))
}

/// `hooks: read <path>: <error>`. Core returns this unclassified; the port calls
/// that [`ErrorKind::Internal`], like every other unclassified failure.
fn read_failure(path: &Path, err: &std::io::Error) -> Error {
    Error::new(
        ErrorKind::Internal,
        format!("hooks: read {}: {err}", path.display()),
    )
}

/// `hooks: <path>: unknown event "<name>"; known events are <list>`.
///
/// The caller passes the first key that is not in the vocabulary, which the
/// sorted walk of the file's keys makes the one core names.
fn unknown_event(path: &Path, unknown: &str) -> Error {
    Error::new(
        ErrorKind::Validation,
        format!(
            "hooks: {}: unknown event {unknown:?}; known events are {}",
            path.display(),
            EVENT_NAMES.join(", ")
        ),
    )
}

/// `hooks: <path>: <event>[<index>]: <reason>`.
fn group_failure(path: &Path, event: &str, index: usize, reason: &str) -> Error {
    Error::new(
        ErrorKind::Validation,
        format!("hooks: {}: {event}[{index}]: {reason}", path.display()),
    )
}

/// The field a matcher regex is tested against: tool events match `tool`,
/// session events `source` or `reason`, subagent events `subagent`.
pub fn match_value(payload: &Payload) -> &str {
    ["tool", "source", "reason", "subagent"]
        .iter()
        // An empty field is skipped, not matched: the first *non-empty* one is
        // the occurrence's value, which is what core's loop says.
        .find_map(|key| {
            payload
                .get(*key)
                .and_then(serde_json::Value::as_str)
                .filter(|value| !value.is_empty())
        })
        .unwrap_or_default()
}

/// The JSON object one invocation receives: the producer's payload plus the
/// event name, which the runner stamps so a producer cannot mislabel its event.
///
/// The producer's map is never mutated, and an untrusted source loses the
/// content-bearing fields before the body is encoded.
pub fn event_body(event: &str, payload: &Payload, trusted: bool) -> Payload {
    let mut body = payload.clone();
    body.insert(
        "event".to_owned(),
        serde_json::Value::String(event.to_owned()),
    );
    if !trusted {
        for key in STRIPPED {
            body.remove(key);
        }
    }
    body
}

#[cfg(test)]
mod tests {
    use super::*;

    fn payload(pairs: &[(&str, &str)]) -> Payload {
        let mut payload = Payload::new();
        for (key, value) in pairs {
            payload.insert(
                (*key).to_owned(),
                serde_json::Value::String((*value).to_owned()),
            );
        }
        payload
    }

    #[test]
    fn the_vocabulary_is_frozen_and_ordered() {
        assert_eq!(event_names(), EVENT_NAMES.to_vec());
        assert_eq!(known_event("PreToolUse"), Some(EVENT_PRE_TOOL_USE));
        assert_eq!(known_event("preToolUse"), None);
        // Appending is an interface change; the count is the assertion that a
        // removal or a rename is too.
        assert_eq!(EVENT_NAMES.len(), 9);
    }

    #[test]
    fn a_matcher_tests_the_first_matching_field() {
        assert_eq!(match_value(&payload(&[("tool", "Bash")])), "Bash");
        assert_eq!(
            match_value(&payload(&[("tool", ""), ("source", "cli")])),
            "cli",
            "an empty field is skipped, not matched"
        );
        assert_eq!(match_value(&payload(&[("subagent", "review")])), "review");
        assert_eq!(match_value(&payload(&[("status", "done")])), "");
    }

    #[test]
    fn an_event_body_is_stamped_and_stripped() {
        let given = payload(&[
            ("tool", "Bash"),
            ("tool_input", "rm -rf /"),
            ("prompt", "hello"),
            ("status", "claimed"),
        ]);
        let trusted = event_body(EVENT_PRE_TOOL_USE, &given, true);
        assert_eq!(
            trusted.get("tool_input").and_then(|v| v.as_str()),
            Some("rm -rf /")
        );
        assert_eq!(
            trusted.get("event").and_then(|v| v.as_str()),
            Some("PreToolUse")
        );
        let untrusted = event_body(EVENT_PRE_TOOL_USE, &given, false);
        assert_eq!(untrusted.get("tool_input"), None);
        assert_eq!(untrusted.get("prompt"), None);
        assert_eq!(
            untrusted.get("status").and_then(|v| v.as_str()),
            Some("claimed")
        );
        assert_eq!(
            given.get("event"),
            None,
            "the producer's map is not mutated"
        );
    }

    #[test]
    fn the_runner_stamps_its_own_event_name() {
        let lying = payload(&[("event", "Bogus")]);
        let body = event_body(EVENT_POST_TOOL_USE, &lying, true);
        assert_eq!(
            body.get("event").and_then(|v| v.as_str()),
            Some("PostToolUse")
        );
    }

    #[test]
    fn a_group_with_only_other_kinds_is_dropped() {
        let group = Group {
            matcher: String::new(),
            hooks: vec![Hook {
                kind: "webhook".to_owned(),
                command: "curl example.test".to_owned(),
                timeout: 0,
            }],
        };
        assert!(new_group_entry(&group, Path::new(""), true).is_ok_and(|entry| entry.is_none()));
    }

    #[test]
    fn a_command_hook_without_a_command_is_refused() {
        let group = Group {
            matcher: String::new(),
            hooks: vec![Hook::default()],
        };
        let err = new_group_entry(&group, Path::new(""), true)
            .err()
            .unwrap_or_else(|| panic!("an empty command must be refused"));
        assert_eq!(err, "hook command is required");
    }

    #[test]
    fn a_bad_matcher_is_refused_by_name() {
        let group = Group {
            matcher: "[".to_owned(),
            hooks: vec![Hook {
                command: "true".to_owned(),
                ..Hook::default()
            }],
        };
        let err = new_group_entry(&group, Path::new(""), true)
            .err()
            .unwrap_or_else(|| panic!("a bad matcher must be refused"));
        assert!(err.starts_with("matcher: "), "{err}");
    }
}
