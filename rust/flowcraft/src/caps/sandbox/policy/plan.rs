//! The plan: what one start actually runs.
//!
//! Every other policy module judges a *string* — `argv[0]`, a base name, a token
//! prefix — while the executor resolves an *environment*: `PATH`, a shell
//! wrapper, a pager's `LESSOPEN`, a programme that re-execs itself. Nothing in
//! between turned "the string the caller wrote" into "the inode that will be
//! exec'd", so each rule was written against something that may never run.
//!
//! [`plan`] closes that gap once per start. `argv[0]` is resolved against the
//! **host's** [`HostPolicy::path_dirs`] and never against the request's `PATH`:
//! the caller owns the child's environment through `EnvPolicy::inject`, so a
//! resolution that consulted it would let the caller pick which file a rule is
//! judged against. The resolved file is canonicalized, so a judgement is about
//! the real inode rather than its spelling, and the environment the child will
//! be handed is recorded alongside it.
//!
//! A plan never decides anything on its own. It is the input the judging layers
//! share ([`super::approval`], [`super::readonly`], the grant scope in
//! [`crate::caps::sandbox::scope`]) and the audit log's account of a start.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use crate::caps::sandbox::contract::{EnvPolicy, SessionSpec};

use super::allowlist::names_match;

/// Where the host looks for programmes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HostPolicy {
    /// The directories a bare `argv[0]` is resolved against, in order.
    ///
    /// A host-side list, not the request's `PATH`, and the order is the
    /// precedence: the first executable match wins.
    pub path_dirs: Vec<PathBuf>,
}

impl Default for HostPolicy {
    fn default() -> Self {
        Self {
            path_dirs: ["/usr/local/bin", "/usr/bin", "/bin"]
                .iter()
                .map(PathBuf::from)
                .collect(),
        }
    }
}

/// What `argv[0]` resolved to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Resolution {
    /// A regular, executable file.
    Path {
        /// The file that will be exec'd, canonicalized.
        program: PathBuf,
        /// The host directory this programme belongs to, or `None` when it is
        /// outside [`HostPolicy::path_dirs`] — a path the caller spelled, which
        /// only a pinned rule may approve.
        ///
        /// `Some` either because the host's own PATH found the programme there,
        /// or because the caller spelled a path that lands in one of those
        /// directories (`/usr/bin/git`).
        path_dir: Option<PathBuf>,
    },
    /// No file of that name exists in [`HostPolicy::path_dirs`], or the spelled
    /// path does not exist.
    NotFound,
    /// A candidate exists but is not an executable regular file.
    NotExecutable(PathBuf),
}

/// What one start means to the executor.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlannedCommand {
    /// `argv[0]` exactly as the caller wrote it: display and audit only.
    pub argv0: String,
    /// The full argument list as written, `argv0` included.
    pub argv: Vec<String>,
    /// What will be exec'd.
    pub resolution: Resolution,
    /// The environment the child will be given, sorted by name: what
    /// [`EnvPolicy::allow`] passes through, overlaid with [`EnvPolicy::inject`].
    ///
    /// `EnvPolicy::allow == None` inherits the host environment, so the list is
    /// then the daemon's own environment — the values the child starts from.
    pub env: Vec<(String, String)>,
    /// The names in [`Self::env`] that `EnvPolicy::inject` sets.
    ///
    /// Only these can be *removed* by a later layer; an inherited variable can
    /// only be shadowed, so the distinction decides what hardening can promise.
    pub injected: Vec<String>,
}

impl PlannedCommand {
    /// The file that will be exec'd, when the plan resolved one.
    #[must_use]
    pub fn program(&self) -> Option<&Path> {
        match &self.resolution {
            Resolution::Path { program, .. } => Some(program),
            Resolution::NotFound | Resolution::NotExecutable(_) => None,
        }
    }

    /// The host directory this programme belongs to (see [`Resolution::Path`]).
    #[must_use]
    pub fn path_dir(&self) -> Option<&Path> {
        match &self.resolution {
            Resolution::Path { path_dir, .. } => path_dir.as_deref(),
            Resolution::NotFound | Resolution::NotExecutable(_) => None,
        }
    }

    /// The value the child will see for `name`.
    #[must_use]
    pub fn env_var(&self, name: &str) -> Option<&str> {
        self.env
            .iter()
            .find(|(key, _)| key == name)
            .map(|(_, value)| value.as_str())
    }

    /// Whether a rule names this command.
    ///
    /// A rule containing `/` is a path: it is canonicalized and compared with
    /// the canonical programme, so `..` and symlink spellings of the same file
    /// agree and a pinned rule never approves another file of the same name.
    ///
    /// A bare rule names a *host* programme, so it only matches a programme that
    /// belongs to a host programme directory ([`Self::path_dir`]). A path the
    /// caller spelled outside those directories has to be pinned; that is what
    /// keeps `["git"]` from approving `/tmp/x/git`.
    #[must_use]
    pub fn names(&self, rule: &str) -> bool {
        if rule.is_empty() {
            return false;
        }
        let Some(program) = self.program().and_then(Path::to_str) else {
            return false;
        };
        if rule.contains('/') {
            let pinned = std::fs::canonicalize(rule).unwrap_or_else(|_| PathBuf::from(rule));
            return names_match(&pinned.to_string_lossy(), program);
        }
        self.path_dir().is_some() && names_match(rule, program)
    }
}

/// Resolve what `spec` will execute and with which environment.
///
/// `host` is the only source of programme directories: the request's `PATH` is
/// part of the child's environment, not of the resolution.
#[must_use]
pub fn plan(spec: &SessionSpec, host: &HostPolicy) -> PlannedCommand {
    let argv = spec.argv.clone();
    let argv0 = argv.first().cloned().unwrap_or_default();
    PlannedCommand {
        resolution: resolve(&argv0, spec.opts.workdir.as_deref(), host),
        injected: spec.opts.env.inject.keys().cloned().collect(),
        env: effective_env(&spec.opts.env),
        argv0,
        argv,
    }
}

/// Resolve `argv[0]`: a spelled path as given, a bare name from `host`.
fn resolve(argv0: &str, workdir: Option<&Path>, host: &HostPolicy) -> Resolution {
    let spelled = Path::new(argv0);
    if spelled.is_absolute() {
        return inspect(spelled, host);
    }
    if spelled.components().count() > 1 {
        // A relative path is relative to the child's working directory. The plan
        // only knows that directory when the caller spelled it absolutely: a
        // relative `workdir` is resolved against the runner's root, which the
        // plan does not have, so there is nothing to canonicalize and nothing to
        // judge.
        let Some(dir) = workdir.filter(|dir| dir.is_absolute()) else {
            return Resolution::NotFound;
        };
        return inspect(&dir.join(spelled), host);
    }
    let mut not_executable = None;
    for dir in &host.path_dirs {
        let Ok(program) = std::fs::canonicalize(dir.join(argv0)) else {
            continue;
        };
        if is_executable(&program) {
            return Resolution::Path {
                program,
                path_dir: Some(dir.clone()),
            };
        }
        not_executable.get_or_insert(program);
    }
    not_executable.map_or(Resolution::NotFound, Resolution::NotExecutable)
}

/// Resolve a path the caller spelled.
fn inspect(path: &Path, host: &HostPolicy) -> Resolution {
    match std::fs::canonicalize(path) {
        Ok(program) if is_executable(&program) => {
            let path_dir = program.parent().and_then(|parent| {
                host.path_dirs
                    .iter()
                    .find(|dir| dir.as_path() == parent)
                    .cloned()
            });
            Resolution::Path { program, path_dir }
        }
        Ok(program) => Resolution::NotExecutable(program),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Resolution::NotFound,
        Err(_) => Resolution::NotExecutable(path.to_path_buf()),
    }
}

#[cfg(unix)]
fn is_executable(path: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(path)
        .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
}

#[cfg(not(unix))]
fn is_executable(path: &Path) -> bool {
    std::fs::metadata(path).is_ok_and(|meta| meta.is_file())
}

/// The environment the child will see, mirroring how the spawn applies
/// [`EnvPolicy`].
fn effective_env(policy: &EnvPolicy) -> Vec<(String, String)> {
    let mut env: BTreeMap<String, String> = BTreeMap::new();
    match &policy.allow {
        // `vars_os`, not `vars`: a non-unicode variable in the daemon's own
        // environment must not panic a start.
        None => {
            for (name, value) in std::env::vars_os() {
                if let (Ok(name), Ok(value)) = (name.into_string(), value.into_string()) {
                    env.insert(name, value);
                }
            }
        }
        Some(names) => {
            for name in names {
                if let Ok(value) = std::env::var(name) {
                    env.insert(name.clone(), value);
                }
            }
        }
    }
    for (name, value) in &policy.inject {
        env.insert(name.clone(), value.clone());
    }
    env.into_iter().collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(unix)]
    fn program(dir: &Path, name: &str) -> PathBuf {
        use std::os::unix::fs::PermissionsExt;
        let path = dir.join(name);
        std::fs::write(&path, "#!/bin/sh\nexit 0\n").expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        std::fs::canonicalize(path).expect("canonicalize")
    }

    #[cfg(unix)]
    fn host(dir: &Path) -> HostPolicy {
        HostPolicy {
            path_dirs: vec![std::fs::canonicalize(dir).expect("canonicalize")],
        }
    }

    fn spec(argv: &[&str], path: &str) -> SessionSpec {
        let mut spec = SessionSpec::new(argv.iter().map(|arg| (*arg).to_owned()).collect());
        spec.opts
            .env
            .inject
            .insert("PATH".to_owned(), path.to_owned());
        spec
    }

    fn planned(program: PathBuf, path_dir: Option<PathBuf>) -> PlannedCommand {
        PlannedCommand {
            argv0: "git".to_owned(),
            argv: vec!["git".to_owned()],
            resolution: Resolution::Path { program, path_dir },
            env: Vec::new(),
            injected: Vec::new(),
        }
    }

    #[cfg(unix)]
    #[test]
    fn a_bare_name_resolves_only_from_the_host_path() {
        let host_dir = tempfile::tempdir().expect("tempdir");
        let evil = tempfile::tempdir().expect("tempdir");
        let real = program(host_dir.path(), "git");
        program(evil.path(), "git");

        // The request's PATH points at the smuggled binary; the plan must not
        // look at it.
        let planned = plan(
            &spec(&["git", "status"], evil.path().to_str().unwrap()),
            &host(host_dir.path()),
        );
        assert_eq!(
            planned.resolution,
            Resolution::Path {
                program: real.clone(),
                path_dir: Some(std::fs::canonicalize(host_dir.path()).unwrap()),
            }
        );
        assert!(
            !planned.env_var("PATH").unwrap().is_empty(),
            "the request's PATH stays part of the child's environment"
        );
    }

    #[cfg(unix)]
    #[test]
    fn an_absolute_program_is_used_as_given() {
        let dir = tempfile::tempdir().expect("tempdir");
        let real = program(dir.path(), "git");
        let host = host(dir.path());
        let planned = plan(
            &spec(&[real.to_str().unwrap(), "status"], "/nonexistent"),
            &host,
        );
        assert_eq!(
            planned.resolution,
            Resolution::Path {
                program: real,
                // The temp dir is the host directory here, so a spelled path
                // that lands in it is a host programme by definition.
                path_dir: Some(std::fs::canonicalize(dir.path()).unwrap()),
            }
        );
    }

    #[test]
    fn a_missing_program_is_not_found() {
        let planned = plan(
            &spec(&["definitely-not-a-sandbox-programme"], "/tmp"),
            &HostPolicy::default(),
        );
        assert_eq!(planned.resolution, Resolution::NotFound);
        assert!(planned.program().is_none());
    }

    #[cfg(unix)]
    #[test]
    fn a_file_that_is_not_executable_is_not_executable() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("git");
        std::fs::write(&path, "not a programme").expect("write");
        let planned = plan(&spec(&[path.to_str().unwrap()], "/tmp"), &host(dir.path()));
        assert!(matches!(planned.resolution, Resolution::NotExecutable(_)));
        assert!(planned.program().is_none());
    }

    #[cfg(unix)]
    #[test]
    fn a_relative_path_outside_every_host_directory_is_not_a_host_programme() {
        let dir = tempfile::tempdir().expect("tempdir");
        let elsewhere = tempfile::tempdir().expect("tempdir");
        let real = program(elsewhere.path(), "git");
        let mut spec = spec(&["./git"], "/tmp");
        spec.opts.workdir = Some(elsewhere.path().to_path_buf());
        let planned = plan(&spec, &host(dir.path()));
        assert_eq!(
            planned.resolution,
            Resolution::Path {
                program: real,
                // Spelled by the caller, outside the host's PATH: only a pinned
                // rule may approve it.
                path_dir: None,
            }
        );
        assert!(!planned.names("git"), "a bare rule must not follow a path");
        assert!(planned.names(elsewhere.path().join("git").to_str().unwrap()));
    }

    #[cfg(unix)]
    #[test]
    fn a_bare_rule_refuses_a_spelled_path_outside_the_host_path() {
        let dir = tempfile::tempdir().expect("tempdir");
        let real = program(dir.path(), "git");
        // The same file spelled through `..`: only a pinned rule may agree, and
        // it agrees because the rule is canonicalized.
        let spelled = dir
            .path()
            .join("..")
            .join(dir.path().file_name().expect("name"))
            .join("git");
        let planned = planned(real.clone(), None);
        assert!(!planned.names("git"));
        assert!(
            planned.names(real.to_str().unwrap()),
            "a pinned rule matches the path"
        );
        assert!(planned.names(spelled.to_str().unwrap()));
        assert!(!planned.names("/tmp/definitely-not-a-sandbox-dir/git"));
    }

    #[test]
    fn a_bare_rule_names_a_host_programme() {
        let host_program = planned(
            PathBuf::from("/usr/bin/git"),
            Some(PathBuf::from("/usr/bin")),
        );
        assert!(host_program.names("git"));
        assert!(host_program.names("/usr/bin/git"));
        assert!(!host_program.names("/tmp/evil/git"));
    }

    #[test]
    fn an_unresolved_plan_matches_no_rule() {
        let planned = PlannedCommand {
            argv0: "git".to_owned(),
            argv: vec!["git".to_owned()],
            resolution: Resolution::NotFound,
            env: Vec::new(),
            injected: Vec::new(),
        };
        assert!(!planned.names("git"));
        assert!(!planned.names("/usr/bin/git"));
    }

    #[test]
    fn the_plan_records_the_environment_the_child_will_get() {
        let mut spec = SessionSpec::new(vec!["true".to_owned()]);
        spec.opts.env.allow = Some(Vec::new());
        spec.opts
            .env
            .inject
            .insert("GIT_PAGER".to_owned(), "sh".to_owned());
        let planned = plan(&spec, &HostPolicy::default());
        assert_eq!(planned.env_var("GIT_PAGER"), Some("sh"));
        assert_eq!(planned.injected, ["GIT_PAGER".to_owned()]);
    }

    #[test]
    fn an_inherited_variable_is_not_injected() {
        // `allow: Some([…])` passes the host value through, which no later layer
        // can remove by editing `inject`.
        let name = "PATH";
        let mut spec = SessionSpec::new(vec!["true".to_owned()]);
        spec.opts.env.allow = Some(vec![name.to_owned()]);
        let planned = plan(&spec, &HostPolicy::default());
        assert!(planned.injected.is_empty());
        assert_eq!(planned.env_var(name), std::env::var(name).ok().as_deref());
    }
}
