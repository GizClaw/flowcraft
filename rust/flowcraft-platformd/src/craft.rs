//! Craft supervision: start or reach a craft, wait for it to answer, and speak
//! the `craft.Craft/v1` ABI.
//!
//! A craft has three bearers — built in, a child of this host, or a process that
//! owns its lifecycle somewhere on the network — and only the last two are
//! supervised here. The unix-socket steps (removing a stale file, creating its
//! directory, narrowing its mode) are conditional on the target being a socket;
//! everything else is common, because the ABI is.
//!
//! The platform owns the session and the grants; the craft owns its agent loop
//! and internal graph. Nothing here parses a craft payload.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use flowcraft::client::CraftClient;
use flowcraft::error::{Error, Result};
use flowcraft::kernel::Target;
use flowcraft::proto::craft as pb;
use tokio::process::{Child, Command};
use tonic::transport::ClientTlsConfig;

/// How to launch one craft.
#[derive(Clone)]
pub struct CraftSpec {
    /// Craft name (also the instance id sent to `Start`).
    pub name: String,
    /// `argv[0]` plus arguments.
    pub argv: Vec<String>,
    /// Where the craft serves: a unix socket on this host, or a `dns` target.
    pub target: Target,
    /// Client TLS material, built from bootstrap secrets. Only a `dns` target
    /// has any; a unix socket refuses TLS the same way an endpoint does.
    pub tls: Option<ClientTlsConfig>,
    /// Extra environment.
    pub env: BTreeMap<String, String>,
    /// How long to wait for the socket to appear.
    pub ready_timeout: Duration,
    /// systemd unit name, when a systemd supervisor launches it.
    pub unit: Option<String>,
    /// Tenant the session belongs to.
    pub tenant: String,
    /// Bootstrap token the craft must present on every call after Describe.
    pub token: Option<String>,
    /// What to do with the child's own stdout and stderr.
    pub stdio: flowcraft::kernel::config::CraftStdio,
}

impl std::fmt::Debug for CraftSpec {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CraftSpec")
            .field("name", &self.name)
            .field("target", &self.target)
            .field("tls", &self.tls.is_some())
            .finish_non_exhaustive()
    }
}

impl CraftSpec {
    /// A spec for a craft that serves `target`.
    #[must_use]
    pub fn new(name: impl Into<String>, argv: Vec<String>, target: Target) -> Self {
        Self {
            name: name.into(),
            argv,
            target,
            tls: None,
            env: BTreeMap::new(),
            ready_timeout: Duration::from_secs(10),
            unit: None,
            tenant: String::new(),
            token: None,
            // The conservative default: a spec that says nothing keeps the
            // historical behaviour rather than silently swallowing output.
            stdio: flowcraft::kernel::config::CraftStdio::Inherit,
        }
    }

    /// The socket path, when this craft is reached over one.
    ///
    /// Everything a unix socket needs — removing a stale file, creating its
    /// directory, narrowing its mode — is conditional on this.
    fn unix_path(&self) -> Option<&Path> {
        match &self.target {
            Target::Unix(path) => Some(path),
            Target::Dns(_) => None,
        }
    }
}

/// Make the craft socket owner-only, and its directory too.
#[cfg(unix)]
// Assembly: a craft socket is set up while the deployment is being built.
#[allow(clippy::disallowed_methods)]
pub fn secure_socket(path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;

    if let Some(parent) = path.parent()
        && parent.exists()
    {
        let _ = std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700));
    }
    match std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
        Ok(()) => Ok(()),
        Err(error) => Err(Error::not_available(format!(
            "craft socket {}: cannot set permissions: {error}",
            path.display()
        ))),
    }
}

/// A unix socket is a unix mechanism: elsewhere a craft target is a `dns`
/// address, and there is no file to narrow.
#[cfg(not(unix))]
// Assembly: a craft socket is set up while the deployment is being built.
#[allow(clippy::disallowed_methods)]
pub fn secure_socket(path: &Path) -> Result<()> {
    Err(Error::not_available(format!(
        "craft target {}: a unix socket requires unix",
        path.display()
    )))
}

/// A started craft with an open session.
pub struct CraftHandle {
    info: pb::CraftInfo,
    session: String,
    grants: Vec<pb::Grant>,
    client: Arc<CraftClient>,
    child: Option<Child>,
    unit: Option<String>,
}

impl std::fmt::Debug for CraftHandle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CraftHandle")
            .field("name", &self.info.name)
            .field("session", &self.session)
            .finish_non_exhaustive()
    }
}

impl CraftHandle {
    /// The craft's self-description.
    #[must_use]
    pub fn info(&self) -> &pb::CraftInfo {
        &self.info
    }

    /// The session id.
    #[must_use]
    pub fn session(&self) -> &str {
        &self.session
    }

    /// The grants the craft is holding.
    #[must_use]
    pub fn grants(&self) -> &[pb::Grant] {
        &self.grants
    }

    /// The systemd unit, when this handle is managed by one.
    #[must_use]
    pub fn unit(&self) -> Option<&str> {
        self.unit.as_deref()
    }

    /// Feed the running turn. The payload uses the same business envelope as
    /// `RunRequest.input`.
    pub async fn steer(&self, payload: pb::Envelope) -> Result<()> {
        crate::abi::validate_input(&self.info, &payload)?;
        self.client
            .steer(pb::SteerRequest {
                session: self.session.clone(),
                payload: Some(payload),
            })
            .await
    }

    /// The typed client, for `Cancel`.
    #[must_use]
    pub fn client(&self) -> Arc<CraftClient> {
        self.client.clone()
    }

    /// Run one turn to completion (or the first terminal event).
    pub async fn run(&self, input: pb::Envelope, credit: i64) -> Result<Vec<pb::RunEvent>> {
        crate::abi::validate_input(&self.info, &input)?;
        let mut stream = self
            .client
            .run(pb::RunRequest {
                session: self.session.clone(),
                input: Some(input),
                credit,
            })
            .await?;
        let mut events = Vec::new();
        while let Some(event) = stream
            .message()
            .await
            .map_err(flowcraft::client::grpc::from_status)?
        {
            crate::abi::validate_event(&self.info, &event)?;
            let terminal = event.r#type == "done" || event.r#type == "error";
            events.push(event);
            if terminal {
                break;
            }
        }
        Ok(events)
    }
}

/// Starts and stops craft processes.
#[async_trait]
pub trait CraftSupervisor: Send + Sync {
    /// Spawn the craft, open a session, and hand it `grants`.
    async fn start(&self, spec: &CraftSpec, grants: Vec<pb::Grant>) -> Result<CraftHandle>;

    /// Close the session and stop the process.
    async fn stop(&self, handle: CraftHandle) -> Result<()>;
}

/// Launches crafts as child processes on this host.
#[derive(Debug, Default)]
pub struct ExecSupervisor;

impl ExecSupervisor {
    /// A supervisor with no children yet.
    #[must_use]
    pub fn new() -> Self {
        Self
    }
}

#[async_trait]
impl CraftSupervisor for ExecSupervisor {
    #[tracing::instrument(
        name = "craft.start",
        skip_all,
        fields(flowcraft.craft = %spec.name)
    )]
    async fn start(&self, spec: &CraftSpec, grants: Vec<pb::Grant>) -> Result<CraftHandle> {
        if spec.argv.is_empty() {
            return Err(Error::validation("craft: argv is empty"));
        }
        // A socket left behind by a crashed child is ours to replace; a `dns`
        // target has no file, and its address is the host's argv to choose.
        if let Some(path) = spec.unix_path() {
            let socket = path.to_path_buf();
            // Inside the closure: this *is* the boundary.
            #[allow(clippy::disallowed_methods)]
            flowcraft::blocking::run("platformd.craft_socket", move || {
                if socket.exists() {
                    std::fs::remove_file(&socket)?;
                }
                if let Some(parent) = socket.parent() {
                    std::fs::create_dir_all(parent)?;
                }
                Ok(())
            })
            .await?;
        }

        let mut command = Command::new(&spec.argv[0]);
        command.args(&spec.argv[1..]);
        // A craft is a separate program, not a separate trust domain: it runs
        // with this daemon's uid, so anything in the daemon's environment is
        // something the craft can read. That includes the bootstrap secrets when
        // the host configured them through `FLOWCRAFT_SECRET_*`, and the identity
        // path of this process. So the child starts from nothing and gets this
        // list plus `crafts.<name>.env` — the sanctioned place to hand it a
        // value.
        command.env_clear();
        command.envs(inherited_environment());
        command.envs(&spec.env);
        if let Some(token) = &spec.token {
            command.env("FLOWCRAFT_CRAFT_TOKEN", token);
        }
        command.kill_on_drop(true);
        match spec.stdio {
            flowcraft::kernel::config::CraftStdio::Capture => {
                command.stdout(std::process::Stdio::piped());
                command.stderr(std::process::Stdio::piped());
            }
            flowcraft::kernel::config::CraftStdio::Inherit => {}
            flowcraft::kernel::config::CraftStdio::Ignore => {
                command.stdout(std::process::Stdio::null());
                command.stderr(std::process::Stdio::null());
            }
        }
        let mut child = command.spawn().map_err(|error| {
            Error::not_available(format!("craft {:?}: cannot start: {error}", spec.name))
        })?;
        crate::craft_stdio::attach(&spec.name, &mut child, spec.stdio);

        if let Err(error) = wait_for_target(&spec.target, spec.ready_timeout).await {
            let _ = child.kill().await;
            return Err(error);
        }
        if let Some(path) = spec.unix_path()
            && let Err(error) = secure_socket(path)
        {
            let _ = child.kill().await;
            return Err(error);
        }
        let client = match CraftClient::connect_with_token(
            &spec.target,
            spec.tls.clone(),
            spec.token.as_deref(),
        )
        .await
        {
            Ok(client) => client,
            Err(error) => {
                let _ = child.kill().await;
                return Err(error);
            }
        };
        let info = match client.describe().await {
            Ok(info) => info,
            Err(error) => {
                let _ = child.kill().await;
                return Err(error);
            }
        };
        if let Err(error) = crate::abi::validate_info(&info) {
            let _ = child.kill().await;
            return Err(error);
        }
        let started = match client
            .start(pb::StartRequest {
                instance: spec.name.clone(),
                tenant: spec.tenant.clone(),
                grants,
            })
            .await
        {
            Ok(started) => started,
            Err(error) => {
                let _ = child.kill().await;
                return Err(error);
            }
        };

        Ok(CraftHandle {
            info,
            session: started.session,
            grants: started.grants,
            client: Arc::new(client),
            child: Some(child),
            unit: None,
        })
    }

    async fn stop(&self, mut handle: CraftHandle) -> Result<()> {
        let _ = handle
            .client
            .close(pb::CloseRequest {
                session: handle.session.clone(),
            })
            .await;
        if let Some(mut child) = handle.child.take() {
            let _ = child.kill().await;
            let _ = child.wait().await;
        }
        Ok(())
    }
}

/// The environment a craft child inherits.
///
/// Only what a program needs to start at all: where to find its own tools, where
/// "home" is, how to spell paths and messages, and where to put temporaries. An
/// allow-list rather than a deny-list because the failure mode of the wrong
/// choice is a leak, and the failure mode of a missing entry is a loud one — a
/// craft that cannot find a library says so on its first line of output.
///
/// Deliberately absent: anything named `FLOWCRAFT_*` (bootstrap secrets, the
/// identity path, another craft's token), and the dynamic-loader variables
/// (`LD_PRELOAD`, `DYLD_*`), which are an injection vector rather than a need.
fn inherited_environment() -> Vec<(String, String)> {
    #[cfg(windows)]
    const NAMES: &[&str] = &[
        "PATH",
        "PATHEXT",
        "SystemRoot",
        "SystemDrive",
        "windir",
        "TEMP",
        "TMP",
        "USERPROFILE",
        "APPDATA",
        "LOCALAPPDATA",
        "COMSPEC",
    ];
    #[cfg(not(windows))]
    const NAMES: &[&str] = &[
        "PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TERM",
    ];

    NAMES
        .iter()
        .filter_map(|name| {
            std::env::var(name)
                .ok()
                .map(|value| ((*name).to_owned(), value))
        })
        .collect()
}

async fn wait_for_target(target: &Target, timeout: Duration) -> Result<()> {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        let ready = match target {
            #[cfg(unix)]
            Target::Unix(path) => tokio::net::UnixStream::connect(path).await.is_ok(),
            #[cfg(not(unix))]
            Target::Unix(path) => {
                return Err(Error::not_available(format!(
                    "craft target {}: a unix socket requires unix; use a dns target",
                    path.display()
                )));
            }
            // A `dns` craft owns its lifecycle, so "ready" is "the port answers":
            // the handshake happens once, in `connect`.
            Target::Dns(address) => tokio::net::TcpStream::connect(address).await.is_ok(),
        };
        if ready {
            return Ok(());
        }
        if tokio::time::Instant::now() >= deadline {
            return Err(Error::not_available(format!(
                "craft did not answer at {} within {timeout:?}",
                target.as_uri()
            )));
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// Connects to a craft somebody else launched.
///
/// The platform does not own the process: it waits for the socket, opens a
/// session, and on stop only closes that session.
#[derive(Debug, Default)]
pub struct ExternalSupervisor;

impl ExternalSupervisor {
    /// A supervisor for externally launched crafts.
    #[must_use]
    pub fn new() -> Self {
        Self
    }
}

#[async_trait]
impl CraftSupervisor for ExternalSupervisor {
    async fn start(&self, spec: &CraftSpec, grants: Vec<pb::Grant>) -> Result<CraftHandle> {
        wait_for_target(&spec.target, spec.ready_timeout).await?;
        if let Some(path) = spec.unix_path() {
            secure_socket(path)?;
        }
        let client =
            CraftClient::connect_with_token(&spec.target, spec.tls.clone(), spec.token.as_deref())
                .await?;
        let info = client.describe().await?;
        crate::abi::validate_info(&info)?;
        let started = client
            .start(pb::StartRequest {
                instance: spec.name.clone(),
                tenant: spec.tenant.clone(),
                grants,
            })
            .await?;
        Ok(CraftHandle {
            info,
            session: started.session,
            grants: started.grants,
            client: Arc::new(client),
            child: None,
            unit: None,
        })
    }

    async fn stop(&self, handle: CraftHandle) -> Result<()> {
        let _ = handle
            .client
            .close(pb::CloseRequest {
                session: handle.session.clone(),
            })
            .await;
        Ok(())
    }
}

/// Starts and stops crafts through systemd.
///
/// The unit owns the process, so this supervisor never spawns or kills
/// anything: it runs `systemctl start`/`stop` and speaks the ABI in between.
#[derive(Debug)]
pub struct SystemdSupervisor {
    systemctl: PathBuf,
}

impl Default for SystemdSupervisor {
    fn default() -> Self {
        Self::new()
    }
}

impl SystemdSupervisor {
    /// Use `systemctl` from `PATH`.
    #[must_use]
    pub fn new() -> Self {
        Self {
            systemctl: PathBuf::from("systemctl"),
        }
    }

    /// Use a specific binary (tests, non-standard installs).
    #[must_use]
    pub fn with_systemctl(systemctl: impl Into<PathBuf>) -> Self {
        Self {
            systemctl: systemctl.into(),
        }
    }

    async fn systemctl(&self, args: &[&str]) -> Result<()> {
        let output = Command::new(&self.systemctl)
            .args(args)
            .output()
            .await
            .map_err(|error| {
                Error::not_available(format!(
                    "systemctl {}: cannot run {}: {error}",
                    args.join(" "),
                    self.systemctl.display()
                ))
            })?;
        if !output.status.success() {
            return Err(Error::not_available(format!(
                "systemctl {}: {}",
                args.join(" "),
                String::from_utf8_lossy(&output.stderr).trim()
            )));
        }
        Ok(())
    }
}

#[async_trait]
impl CraftSupervisor for SystemdSupervisor {
    async fn start(&self, spec: &CraftSpec, grants: Vec<pb::Grant>) -> Result<CraftHandle> {
        let unit = spec
            .unit
            .clone()
            .unwrap_or_else(|| format!("flowcraft-{}.service", spec.name));
        self.systemctl(&["start", &unit]).await?;
        let ready = wait_for_target(&spec.target, spec.ready_timeout).await;
        if let Err(error) = ready {
            let _ = self.systemctl(&["stop", &unit]).await;
            return Err(error);
        }
        if let Some(path) = spec.unix_path()
            && let Err(error) = secure_socket(path)
        {
            let _ = self.systemctl(&["stop", &unit]).await;
            return Err(error);
        }
        let client = match CraftClient::connect_with_token(
            &spec.target,
            spec.tls.clone(),
            spec.token.as_deref(),
        )
        .await
        {
            Ok(client) => client,
            Err(error) => {
                let _ = self.systemctl(&["stop", &unit]).await;
                return Err(error);
            }
        };
        let info = match client.describe().await {
            Ok(info) => info,
            Err(error) => {
                let _ = self.systemctl(&["stop", &unit]).await;
                return Err(error);
            }
        };
        if let Err(error) = crate::abi::validate_info(&info) {
            let _ = self.systemctl(&["stop", &unit]).await;
            return Err(error);
        }
        let started = match client
            .start(pb::StartRequest {
                instance: spec.name.clone(),
                tenant: spec.tenant.clone(),
                grants,
            })
            .await
        {
            Ok(started) => started,
            Err(error) => {
                let _ = self.systemctl(&["stop", &unit]).await;
                return Err(error);
            }
        };
        Ok(CraftHandle {
            info,
            session: started.session,
            grants: started.grants,
            client: Arc::new(client),
            child: None,
            unit: Some(unit),
        })
    }

    async fn stop(&self, handle: CraftHandle) -> Result<()> {
        let _ = handle
            .client
            .close(pb::CloseRequest {
                session: handle.session.clone(),
            })
            .await;
        if let Some(unit) = handle.unit.as_deref() {
            let _ = self.systemctl(&["stop", unit]).await;
        }
        Ok(())
    }
}
