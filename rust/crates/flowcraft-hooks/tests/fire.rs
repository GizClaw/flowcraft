//! The execution half, against real processes: what a hook command sees on
//! stdin, in what order, and what happens when it overruns.
//!
//! These are port-level tests rather than contract rows. A contract row is a
//! comparison of two implementations, so it can pin a load failure's text or the
//! order commands ran in; it cannot pin *that the group died*, because the
//! evidence is a process table neither side can dump the same way. What is here
//! is the part of the transport contract that needs a clock and a signal.
//!
//! Every test drives a source whose `dir` is a temporary directory, and every
//! command writes a file relative to it. That is what makes the commands text the
//! same in both implementations: no absolute path and no environment variable
//! crosses the boundary.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use flowcraft_hooks::{DEFAULT_TIMEOUT, ExtraSource, Manager, Payload, Warn, Warning, event_body};

/// A directory that removes itself, since the crate has no `tempfile` and the
/// tests only need "somewhere private".
struct TempDir {
    path: PathBuf,
}

impl TempDir {
    fn new(name: &str) -> TempDir {
        let unique = std::process::id();
        let path = std::env::temp_dir().join(format!("flowcraft-hooks-{name}-{unique}"));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{path:?}: {err}"));
        TempDir { path }
    }

    fn path(&self) -> &Path {
        &self.path
    }

    /// Write a file into the directory, as a `hooks.json` would be written.
    fn write(&self, name: &str, body: &str) -> PathBuf {
        let path = self.path.join(name);
        std::fs::write(&path, body).unwrap_or_else(|err| panic!("{path:?}: {err}"));
        path
    }

    /// Read a file the commands wrote, if they wrote one.
    fn read(&self, name: &str) -> Option<String> {
        std::fs::read_to_string(self.path.join(name)).ok()
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// A sink that keeps what it was told.
#[derive(Default)]
struct Collected(Mutex<Vec<Warning>>);

impl Collected {
    fn warnings(&self) -> Vec<Warning> {
        self.0
            .lock()
            .map(|warnings| warnings.clone())
            .unwrap_or_default()
    }

    fn messages(&self) -> Vec<&'static str> {
        self.warnings()
            .iter()
            .map(|warning| warning.message)
            .collect()
    }
}

impl Warn for Collected {
    fn warn(&self, warning: &Warning) {
        let mut warnings = self
            .0
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        warnings.push(warning.clone());
    }
}

/// A command that records its own label, then its stdin, into `trace.part`: one
/// `label:{json}` line per invocation.
///
/// `printf` and `cat` are separate processes: the shell runs them in order, so
/// the label lands before the body even though both write the same file
/// descriptor.
fn recorder(label: &str) -> String {
    format!("printf '{label}:' >> trace.part; cat >> trace.part; printf '\\n' >> trace.part")
}

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

/// The fired hooks' lines, in order: `[("A", "{...}")]`.
fn trace(directory: &TempDir) -> Vec<(String, String)> {
    let Some(text) = directory.read("trace.part") else {
        return Vec::new();
    };
    text.lines()
        .filter(|line| !line.is_empty())
        .map(|line| {
            let (label, body) = line.split_at(line.find(':').map(|at| at + 1).unwrap_or(0));
            (label.trim_end_matches(':').to_owned(), body.to_owned())
        })
        .collect()
}

#[tokio::test]
async fn hooks_run_in_load_order_and_only_when_they_match() {
    let directory = TempDir::new("order");
    let first = directory.write(
        "first.json",
        &format!(
            r#"{{"hooks": {{"PreToolUse": [
                {{"matcher": "Bash", "hooks": [{{"command": "{}"}}]}},
                {{"matcher": "*", "hooks": [{{"command": "{}"}}]}}
            ]}}}}"#,
            recorder("matched"),
            recorder("wildcard")
        ),
    );
    let second = directory.write(
        "second.json",
        &format!(
            r#"{{"hooks": {{"PreToolUse": [{{"hooks": [{{"command": "{}"}}]}}]}}}}"#,
            recorder("later")
        ),
    );
    let sources = [
        ExtraSource {
            path: first,
            dir: directory.path().to_path_buf(),
            trusted: true,
        },
        ExtraSource {
            path: second,
            dir: directory.path().to_path_buf(),
            trusted: true,
        },
    ];
    let manager = Manager::load_with_sources(
        directory.path().join("absent.json"),
        &sources,
        &Collected::default(),
    )
    .unwrap_or_else(|err| panic!("{err}"));
    assert!(!manager.empty());

    manager
        .fire(
            "PreToolUse",
            &payload(&[("tool", "Bash")]),
            &Collected::default(),
        )
        .await;
    let matched: Vec<String> = trace(&directory)
        .into_iter()
        .map(|(label, _)| label)
        .collect();
    assert_eq!(
        matched,
        ["matched", "wildcard", "later"],
        "one group's hooks run in declaration order, and the sources in load order"
    );

    let _ = std::fs::remove_file(directory.path().join("trace.part"));
    manager
        .fire(
            "PreToolUse",
            &payload(&[("tool", "Read")]),
            &Collected::default(),
        )
        .await;
    let read: Vec<String> = trace(&directory)
        .into_iter()
        .map(|(label, _)| label)
        .collect();
    assert_eq!(
        read,
        ["wildcard", "later"],
        "an exact matcher declines the others"
    );

    // An event with no groups is not a warning: nothing happened, which is the
    // whole of what a caller needs to know.
    let collected = Collected::default();
    manager.fire("SessionEnd", &payload(&[]), &collected).await;
    assert!(collected.warnings().is_empty());
}

#[tokio::test]
async fn a_hook_receives_one_json_object_on_stdin() {
    let directory = TempDir::new("stdin");
    let source = directory.write(
        "hooks.json",
        &format!(
            r#"{{"hooks": {{"UserPromptSubmit": [{{"hooks": [{{"command": "{}"}}]}}]}}}}"#,
            recorder("prompt")
        ),
    );
    let sources = [ExtraSource {
        path: source,
        dir: directory.path().to_path_buf(),
        trusted: true,
    }];
    let manager = Manager::load_with_sources(
        directory.path().join("absent.json"),
        &sources,
        &Collected::default(),
    )
    .unwrap_or_else(|err| panic!("{err}"));
    manager
        .fire(
            "UserPromptSubmit",
            &payload(&[("prompt", "hello"), ("source", "cli")]),
            &Collected::default(),
        )
        .await;

    let trace = trace(&directory);
    assert_eq!(trace.len(), 1);
    let body: Payload = serde_json::from_str(&trace[0].1).unwrap_or_else(|err| panic!("{err}"));
    assert_eq!(
        body.get("event").and_then(|v| v.as_str()),
        Some("UserPromptSubmit")
    );
    assert_eq!(body.get("prompt").and_then(|v| v.as_str()), Some("hello"));
    assert_eq!(
        body.keys().map(String::as_str).collect::<Vec<_>>(),
        ["event", "prompt", "source"],
        "keys are sorted, which is what makes two bodies comparable"
    );
}

#[tokio::test]
async fn an_untrusted_source_never_sees_content() {
    let directory = TempDir::new("untrusted");
    let source = directory.write(
        "plugin.json",
        &format!(
            r#"{{"hooks": {{"PostToolUse": [{{"hooks": [{{"command": "{}"}}]}}]}}}}"#,
            recorder("plugin")
        ),
    );
    let sources = [ExtraSource {
        path: source,
        dir: directory.path().to_path_buf(),
        trusted: false,
    }];
    let manager = Manager::load_with_sources(
        directory.path().join("absent.json"),
        &sources,
        &Collected::default(),
    )
    .unwrap_or_else(|err| panic!("{err}"));
    manager
        .fire(
            "PostToolUse",
            &payload(&[
                ("tool", "Bash"),
                ("tool_input", "secret"),
                ("tool_result", "secret"),
            ]),
            &Collected::default(),
        )
        .await;

    let body: Payload =
        serde_json::from_str(&trace(&directory)[0].1).unwrap_or_else(|err| panic!("{err}"));
    assert_eq!(
        body.keys().map(String::as_str).collect::<Vec<_>>(),
        ["event", "tool"]
    );
}

#[tokio::test]
async fn a_failing_hook_does_not_stop_the_next_one() {
    let directory = TempDir::new("failing");
    let source = directory.write(
        "hooks.json",
        &format!(
            r#"{{"hooks": {{"TurnEnd": [{{"hooks": [
                {{"command": "{}; echo boom >&2; exit 3"}},
                {{"command": "{}"}}
            ]}}]}}}}"#,
            recorder("first"),
            recorder("second")
        ),
    );
    let sources = [ExtraSource {
        path: source,
        dir: directory.path().to_path_buf(),
        trusted: true,
    }];
    let collected = Collected::default();
    let manager =
        Manager::load_with_sources(directory.path().join("absent.json"), &sources, &collected)
            .unwrap_or_else(|err| panic!("{err}"));
    manager.fire("TurnEnd", &payload(&[]), &collected).await;

    assert_eq!(
        trace(&directory)
            .into_iter()
            .map(|(label, _)| label)
            .collect::<Vec<_>>(),
        ["first", "second"],
        "the failing hook ran and the next hook still ran"
    );
    assert_eq!(collected.messages(), ["hooks: command hook failed"]);
    let warning = &collected.warnings()[0];
    assert_eq!(warning.field("event"), Some("TurnEnd"));
    assert_eq!(
        warning.field("output"),
        Some("boom\n"),
        "the output is logged"
    );
}

#[tokio::test]
async fn a_hook_that_overruns_is_killed_with_its_group() {
    let directory = TempDir::new("timeout");
    // The shell spawns a descendant that would write `late.part` after two
    // seconds, and then sleeps: killing only the shell would leave the
    // descendant to write it.
    let source = directory.write(
        "hooks.json",
        r#"{"hooks": {"SessionStart": [{"hooks": [
            {"command": "(sleep 2; printf late > late.part) & sleep 5", "timeout": 1}
        ]}]}}"#,
    );
    let sources = [ExtraSource {
        path: source,
        dir: directory.path().to_path_buf(),
        trusted: true,
    }];
    let collected = Collected::default();
    let manager =
        Manager::load_with_sources(directory.path().join("absent.json"), &sources, &collected)
            .unwrap_or_else(|err| panic!("{err}"));

    let started = Instant::now();
    manager
        .fire("SessionStart", &payload(&[]), &collected)
        .await;
    let elapsed = started.elapsed();
    assert!(
        elapsed < Duration::from_millis(2500),
        "the fire must return on its timeout, not when the hook finishes: {elapsed:?}"
    );
    assert_eq!(
        collected.messages(),
        ["hooks: command hook did not finish in time"]
    );

    tokio::time::sleep(Duration::from_millis(2200)).await;
    assert_eq!(
        directory.read("late.part"),
        None,
        "the descendant was in the killed process group"
    );
}

#[tokio::test]
async fn a_missing_primary_file_is_an_empty_runner() {
    let directory = TempDir::new("missing");
    let manager = Manager::load(directory.path().join("absent.json"))
        .unwrap_or_else(|err| panic!("a missing file is not an error: {err}"));
    assert!(manager.empty());
    assert_eq!(manager.path(), directory.path().join("absent.json"));
}

#[tokio::test]
async fn a_broken_extra_source_is_skipped_and_the_rest_load() {
    let directory = TempDir::new("broken");
    let broken = directory.write("broken.json", "{not json");
    let good = directory.write(
        "good.json",
        &format!(
            r#"{{"hooks": {{"TurnEnd": [{{"hooks": [{{"command": "{}"}}]}}]}}}}"#,
            recorder("good")
        ),
    );
    let sources = [
        ExtraSource {
            path: broken.clone(),
            dir: directory.path().to_path_buf(),
            trusted: true,
        },
        ExtraSource {
            path: good,
            dir: directory.path().to_path_buf(),
            trusted: true,
        },
    ];
    let collected = Collected::default();
    let manager =
        Manager::load_with_sources(directory.path().join("absent.json"), &sources, &collected)
            .unwrap_or_else(|err| panic!("an extra source must not fail the load: {err}"));
    assert_eq!(collected.messages(), ["hooks: skipping hook source"]);
    assert_eq!(
        collected.warnings()[0].field("path").map(Path::new),
        Some(broken.as_path())
    );

    manager.fire("TurnEnd", &payload(&[]), &collected).await;
    assert_eq!(trace(&directory).len(), 1);
}

#[tokio::test]
async fn a_primary_file_that_does_not_parse_is_loud() {
    let directory = TempDir::new("primary-bad");
    let path = directory.write("hooks.json", "{not json");
    let err = Manager::load(&path)
        .err()
        .unwrap_or_else(|| panic!("must be refused"));
    assert_eq!(err.kind(), flowcraft_contract::error::ErrorKind::Validation);
    assert!(
        err.to_string()
            .starts_with(&format!("hooks: parse {}: ", path.display()))
    );
}

#[tokio::test]
async fn the_default_timeout_is_thirty_seconds() {
    assert_eq!(DEFAULT_TIMEOUT, Duration::from_secs(30));
    // The body a caller builds for an event with no payload still carries the
    // stamped name and nothing else.
    let empty = event_body("TurnEnd", &Payload::new(), false);
    assert_eq!(
        empty.keys().map(String::as_str).collect::<Vec<_>>(),
        ["event"]
    );
    // `Arc<Collected>` is a sink too, which is how a caller shares one.
    let shared: Arc<Collected> = Arc::new(Collected::default());
    shared.warn(&Warning::new("hooks: command hook failed", []));
    assert_eq!(shared.messages(), ["hooks: command hook failed"]);
}
