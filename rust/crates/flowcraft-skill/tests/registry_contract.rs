//! The skill registry contract: the trees core's discovery, ranking and body
//! reads are asked about, replayed.
//!
//! Each row is a tree the test builds in a temporary directory of its own -- the
//! one thing that cannot be compared as-is is that directory's absolute path,
//! masked down to the logical form (`a/alpha/SKILL.md`) in every string the dump
//! carries. Two more strings are masked, both of them somebody else's wording: an
//! operating system's sentence after the verb and path core wrote, and a YAML
//! library's error text after the sentence that classifies it. What is compared
//! is everything core chose: the skills, the walk order of the diagnostics, which
//! roots were walked, and what a body read answers.

use std::fs;
use std::path::{Path, PathBuf};

use flowcraft_skill::{
    Diagnostic, Metadata, Options, Outcome, Scored, Service, discover, render_section,
};
use serde::Deserialize;

/// The committed document.
#[derive(Deserialize)]
struct Contract {
    /// The fixtures and their answers.
    trees: Vec<Tree>,
    /// The per-turn section rows.
    renders: Vec<Render>,
}

/// One fixture tree and every question put to it.
#[derive(Deserialize)]
struct Tree {
    /// Identifies the tree in a mismatch.
    name: String,
    /// What the tree is evidence of.
    why: String,
    /// What the harness writes.
    #[serde(default)]
    entries: Vec<Entry>,
    /// The configured roots, relative to the harness's directory.
    #[serde(default)]
    roots: Vec<String>,
    /// `Options.Disabled`, each entry with its rule.
    #[serde(default)]
    disabled: Vec<Disable>,
    /// `Options.TopN`.
    #[serde(default)]
    top_n: usize,
    /// `Options.MinScore`.
    #[serde(default)]
    min_score: f64,
    /// Discovery's answer.
    discover: MaskedOutcome,
    /// The service's answer, when the tree asks for one.
    service: Option<MaskedService>,
}

/// One filesystem entry.
#[derive(Deserialize)]
struct Entry {
    /// Where it is written, relative to the harness's directory.
    path: String,
    /// `dir`, `file` or `link`; empty means `file`.
    #[serde(default)]
    kind: String,
    /// A file's bytes.
    #[serde(default)]
    data: String,
    /// A link's target, relative to the harness's directory.
    #[serde(default)]
    target: String,
}

/// One `Options.Disabled` entry.
#[derive(Deserialize)]
struct Disable {
    /// `name` or `path`.
    kind: String,
    /// The name, or the path relative to the harness's directory.
    value: String,
}

/// A discovery answer.
#[derive(Debug, Deserialize, Default, PartialEq)]
struct MaskedOutcome {
    /// The skills, in `Outcome` order.
    #[serde(default)]
    skills: Vec<Metadata>,
    /// The non-fatal issues, in walk order.
    #[serde(default)]
    diagnostics: Vec<MaskedDiagnostic>,
    /// The roots that contained a skill.
    #[serde(default)]
    roots: Vec<String>,
    /// Every configured root in absolute form.
    #[serde(default)]
    scan_roots: Vec<String>,
}

/// One diagnostic.
#[derive(Debug, Deserialize, PartialEq)]
struct MaskedDiagnostic {
    /// What it is about.
    path: String,
    /// The sentence.
    message: String,
    /// Whether it is a tolerated shape issue.
    #[serde(default)]
    warning: bool,
}

/// One tree's answers through a `Service`.
#[derive(Deserialize)]
struct MaskedService {
    /// `Service::list`, after the disabled list.
    #[serde(default)]
    list: Vec<Metadata>,
    /// `Service::roots`.
    #[serde(default)]
    roots: Vec<String>,
    /// `Service::diagnostics`.
    #[serde(default)]
    diagnostics: Vec<MaskedDiagnostic>,
    /// The mention rows.
    #[serde(default)]
    mentions: Vec<MaskedMention>,
    /// The BM25 rows.
    #[serde(default)]
    rankings: Vec<MaskedRanking>,
    /// The body-read rows.
    #[serde(default)]
    reads: Vec<MaskedRead>,
    /// The reload row.
    reload: Option<MaskedReload>,
}

/// One mention question.
#[derive(Deserialize)]
struct MaskedMention {
    /// The input.
    text: String,
    /// The resolved skills, in mention order.
    #[serde(default)]
    answer: Vec<Metadata>,
}

/// One BM25 question.
#[derive(Deserialize)]
struct MaskedRanking {
    /// The query.
    query: String,
    /// The call's limit; zero means the service's.
    #[serde(default)]
    top_n: usize,
    /// The call's threshold.
    #[serde(default)]
    min_score: f64,
    /// The ranked skills.
    #[serde(default)]
    answer: Vec<MaskedScored>,
}

/// One ranked skill.
#[derive(Debug, Deserialize)]
struct MaskedScored {
    /// Its path, masked.
    path: String,
    /// Its score, printed to twelve significant digits.
    score: String,
}

/// One body read.
#[derive(Deserialize)]
struct MaskedRead {
    /// `name` or `path`.
    by: String,
    /// The name, or the masked path.
    key: String,
    /// What the read answered.
    answer: MaskedReadAnswer,
}

/// One read's answer.
#[derive(Debug, Deserialize, Default, PartialEq)]
struct MaskedReadAnswer {
    /// The skill's masked path, when it resolved one.
    #[serde(default)]
    path: String,
    /// The body.
    #[serde(default)]
    body: String,
    /// The refusal.
    error: Option<String>,
}

/// The reload row.
#[derive(Deserialize)]
struct MaskedReload {
    /// The file written between the passes.
    add: Entry,
    /// `Service::list` after reload.
    #[serde(default)]
    list: Vec<Metadata>,
    /// `Service::diagnostics` after reload.
    #[serde(default)]
    diagnostics: Vec<MaskedDiagnostic>,
}

/// One per-turn section row.
#[derive(Deserialize)]
struct Render {
    /// Identifies the row.
    name: String,
    /// What the row is evidence of.
    why: String,
    /// The input list.
    #[serde(default)]
    skills: Vec<Metadata>,
    /// The rendered section.
    answer: String,
}

#[test]
fn the_skill_registry_contract_replays() {
    let contract: Contract = serde_json::from_str(
        &fs::read_to_string(contract_path()).unwrap_or_else(|err| panic!("{err}")),
    )
    .unwrap_or_else(|err| panic!("skill-registry-contract.json: {err}"));
    assert!(!contract.trees.is_empty(), "the contract has trees");

    let mut mismatches = Vec::new();
    for tree in &contract.trees {
        let directory = Directory::new("skill-registry-contract");
        let canonical = fs::canonicalize(directory.path())
            .unwrap_or_else(|err| panic!("{}: {err}", directory.path().display()));
        for entry in &tree.entries {
            materialize(directory.path(), entry);
        }
        let text = directory.path().to_string_lossy().into_owned();
        let canonical_text = canonical.to_string_lossy().into_owned();
        let mask = |value: &str| mask(value, &text, &canonical_text);
        let roots: Vec<String> = tree
            .roots
            .iter()
            .map(|root| directory.path().join(root).to_string_lossy().into_owned())
            .collect();

        let outcome = discover(&roots);
        let actual = masked_outcome(&outcome, &mask);
        if let Some(reason) = difference_outcome(&tree.discover, &actual) {
            mismatches.push(format!("tree {}: {reason}\n  why: {}", tree.name, tree.why));
            continue;
        }

        let Some(expected) = &tree.service else {
            continue;
        };
        let disabled: Vec<String> = tree
            .disabled
            .iter()
            .map(|entry| match entry.kind.as_str() {
                "path" => canonical.join(&entry.value).to_string_lossy().into_owned(),
                _ => entry.value.clone(),
            })
            .collect();
        let service = Service::new(Options {
            roots: roots.clone(),
            top_n: tree.top_n,
            min_score: tree.min_score,
            disabled,
        });
        for reason in differences_service(expected, &service, &canonical, &mask) {
            mismatches.push(format!("tree {}: {reason}\n  why: {}", tree.name, tree.why));
        }
        if let Some(reload) = &expected.reload {
            materialize(directory.path(), &reload.add);
            service.reload();
            let list = masked_skills(service.list(), &mask);
            if list != reload.list {
                mismatches.push(format!(
                    "tree {}: reload list\n  core: {:?}\n  port: {:?}",
                    tree.name, reload.list, list
                ));
            }
            let diagnostics = masked_diagnostics(service.diagnostics(), &mask);
            if diagnostics != reload.diagnostics {
                mismatches.push(format!(
                    "tree {}: reload diagnostics\n  core: {:?}\n  port: {:?}",
                    tree.name, reload.diagnostics, diagnostics
                ));
            }
        }
    }

    for row in &contract.renders {
        let actual = render_section(&row.skills);
        if actual != row.answer {
            mismatches.push(format!(
                "render {}: section\n  core: {:?}\n  port: {:?}\n  why: {}",
                row.name, row.answer, actual, row.why
            ));
        }
    }

    assert!(
        mismatches.is_empty(),
        "{} answers disagree:\n\n{}",
        mismatches.len(),
        mismatches.join("\n\n")
    );
}

/// Compares one tree's service answers, returning every disagreement.
fn differences_service(
    expected: &MaskedService,
    service: &Service,
    canonical: &Path,
    mask: &dyn Fn(&str) -> String,
) -> Vec<String> {
    let mut out = Vec::new();
    let list = masked_skills(service.list(), mask);
    if list != expected.list {
        out.push(format!(
            "list\n  core: {:?}\n  port: {:?}",
            expected.list, list
        ));
    }
    let roots: Vec<String> = service.roots().iter().map(|root| mask(root)).collect();
    if roots != expected.roots {
        out.push(format!(
            "roots\n  core: {:?}\n  port: {:?}",
            expected.roots, roots
        ));
    }
    let diagnostics = masked_diagnostics(service.diagnostics(), mask);
    if diagnostics != expected.diagnostics {
        out.push(format!(
            "diagnostics\n  core: {:?}\n  port: {:?}",
            expected.diagnostics, diagnostics
        ));
    }
    for (index, row) in expected.mentions.iter().enumerate() {
        let actual = masked_skills(service.mentioned(&row.text), mask);
        if actual != row.answer {
            out.push(format!(
                "mention {:?}\n  core: {:?}\n  port: {:?}",
                row.text, row.answer, actual
            ));
        }
        let _ = index;
    }
    for row in &expected.rankings {
        let actual: Vec<Scored> = service.rank_scored(&row.query, row.top_n, row.min_score);
        let mut same = actual.len() == row.answer.len();
        if same {
            for (got, want) in actual.iter().zip(row.answer.iter()) {
                // The same tolerance `bm25_contract.rs` states, for the same
                // reason: ranking goes through idf and `math.Log`, whose last
                // bit is the platform's, and the dump itself gives up the
                // thirteenth digit for that. A drift large enough to reorder
                // two skills would still fail.
                let ok = match want.score.parse::<f64>() {
                    Ok(want_score) => {
                        let tolerance = want_score.abs() * 1e-11;
                        (got.score - want_score).abs() <= tolerance
                    }
                    Err(_) => false,
                };
                if mask(&got.skill.path) != want.path || !ok {
                    same = false;
                    break;
                }
            }
        }
        if !same {
            let port: Vec<(String, f64)> = actual
                .iter()
                .map(|scored| (mask(&scored.skill.path), scored.score))
                .collect();
            let core: Vec<(String, &str)> = row
                .answer
                .iter()
                .map(|scored| (scored.path.clone(), scored.score.as_str()))
                .collect();
            out.push(format!(
                "ranking {:?} limit {} threshold {}\n  core: {core:?}\n  port: {port:?}",
                row.query, row.top_n, row.min_score
            ));
        }
    }
    for row in &expected.reads {
        let actual = if row.by == "path" {
            // The row's key is the masked path; discovery recorded canonical
            // ones, so the harness's canonical directory is what it resolves
            // against.
            let key = canonical.join(&row.key);
            service.read_by_path(&key.to_string_lossy())
        } else {
            service.read_full(&row.key)
        };
        let answer = match actual {
            Ok((skill, body)) => MaskedReadAnswer {
                path: mask(&skill.path),
                body: mask(&body),
                error: None,
            },
            Err(err) => MaskedReadAnswer {
                path: String::new(),
                body: String::new(),
                error: Some(mask(&err.to_string())),
            },
        };
        if answer != row.answer {
            out.push(format!(
                "read {} {:?}\n  core: {:?}\n  port: {:?}",
                row.by, row.key, row.answer, answer
            ));
        }
    }
    out
}

/// Masks a discovery outcome.
fn masked_outcome(outcome: &Outcome, mask: &dyn Fn(&str) -> String) -> MaskedOutcome {
    MaskedOutcome {
        skills: masked_skills(outcome.skills.clone(), mask),
        diagnostics: masked_diagnostics(outcome.diagnostics.clone(), mask),
        roots: outcome.roots.iter().map(|root| mask(root)).collect(),
        scan_roots: outcome.scan_roots.iter().map(|root| mask(root)).collect(),
    }
}

/// Masks every skill's path, which is the one field that carries the harness's
/// temporary directory.
fn masked_skills(skills: Vec<Metadata>, mask: &dyn Fn(&str) -> String) -> Vec<Metadata> {
    skills
        .into_iter()
        .map(|mut skill| {
            skill.path = mask(&skill.path);
            skill
        })
        .collect()
}

/// Masks a diagnostic list.
fn masked_diagnostics(
    diagnostics: Vec<Diagnostic>,
    mask: &dyn Fn(&str) -> String,
) -> Vec<MaskedDiagnostic> {
    diagnostics
        .into_iter()
        .map(|diagnostic| MaskedDiagnostic {
            path: mask(&diagnostic.path),
            message: mask(&diagnostic.message),
            warning: diagnostic.warning,
        })
        .collect()
}

/// The first field two discovery answers disagree about, as a sentence.
fn difference_outcome(expected: &MaskedOutcome, actual: &MaskedOutcome) -> Option<String> {
    if expected.skills != actual.skills {
        return Some(format!(
            "skills\n  core: {:?}\n  port: {:?}",
            expected.skills, actual.skills
        ));
    }
    if expected.diagnostics != actual.diagnostics {
        return Some(format!(
            "diagnostics\n  core: {:?}\n  port: {:?}",
            expected.diagnostics, actual.diagnostics
        ));
    }
    if expected.roots != actual.roots {
        return Some(format!(
            "roots\n  core: {:?}\n  port: {:?}",
            expected.roots, actual.roots
        ));
    }
    if expected.scan_roots != actual.scan_roots {
        return Some(format!(
            "scan roots\n  core: {:?}\n  port: {:?}",
            expected.scan_roots, actual.scan_roots
        ));
    }
    None
}

/// Writes one entry.
fn materialize(directory: &Path, entry: &Entry) {
    let path = directory.join(&entry.path);
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).unwrap_or_else(|err| panic!("{}: {err}", parent.display()));
    }
    match entry.kind.as_str() {
        "dir" => {
            fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{}: {err}", path.display()))
        }
        "link" => {
            let target = directory.join(&entry.target);
            symlink(&target, &path);
        }
        _ => fs::write(&path, entry.data.as_bytes())
            .unwrap_or_else(|err| panic!("{}: {err}", path.display())),
    }
}

/// The masks the document lists, in the order the dump applied them: the
/// harness's directory in both its forms, then an operating system's sentence,
/// then a library's error text.
fn mask(text: &str, directory: &str, canonical: &str) -> String {
    let mut text = text.to_owned();
    for prefix in [canonical, directory] {
        if prefix.is_empty() {
            continue;
        }
        text = text.replace(&format!("{prefix}/"), "");
        if text == prefix {
            text = ".".to_owned();
        }
    }
    if text.starts_with("resolve symlink: ") {
        // The operating system's sentence and the path it names are the
        // platform's: what core decided is that a dangling link is an error
        // diagnostic and not a skill.
        return "resolve symlink: <os error>".to_owned();
    }
    for verb in [
        "open ",
        "stat ",
        "lstat ",
        "read ",
        "readdirent ",
        "readlink ",
    ] {
        if let Some(rest) = text.strip_prefix(verb)
            && let Some(at) = rest.find(": ")
        {
            return format!("{verb}{}<os error>", &rest[..at + 2]);
        }
    }
    const PREFIX: &str = "skill: parse frontmatter: ";
    if let Some(rest) = text.strip_prefix(PREFIX) {
        if rest.starts_with("YAML key") {
            return text;
        }
        return format!("{PREFIX}<decode error>");
    }
    text
}

/// A symlink, or a failure that names the platform rather than the row.
#[cfg(unix)]
fn symlink(target: &Path, path: &Path) {
    std::os::unix::fs::symlink(target, path)
        .unwrap_or_else(|err| panic!("link {}: {err}", path.display()));
}

/// A symlink is not available here, so a row that needs one cannot replay.
#[cfg(not(unix))]
fn symlink(target: &Path, path: &Path) {
    panic!(
        "link {} -> {}: the registry contract replays on unix",
        path.display(),
        target.display()
    );
}

/// The committed dump, next to this crate in the workspace.
fn contract_path() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../conformance/skill-registry-contract.json")
}

/// A directory that removes itself, since the crate has no `tempfile`.
struct Directory {
    path: PathBuf,
}

impl Directory {
    fn new(name: &str) -> Directory {
        let path = std::env::temp_dir().join(format!("flowcraft-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&path);
        fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{}: {err}", path.display()));
        Directory { path }
    }

    fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for Directory {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.path);
    }
}
