//! Craft supervision: start a craft process, wait for its socket, and speak
//! the `craft.Craft/v1` ABI.
//!
//! The platform owns the session and the grants; the craft owns its agent loop
//! and internal graph. Nothing here parses a craft payload.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use flowcraft_client::CraftClient;
use flowcraft_error::{Error, Result};
use flowcraft_kernel::Target;
use flowcraft_proto::craft as pb;
use tokio::process::{Child, Command};

/// How to launch one craft.
#[derive(Debug, Clone)]
pub struct CraftSpec {
    /// Craft name (also the instance id sent to `Start`).
    pub name: String,
    /// `argv[0]` plus arguments.
    pub argv: Vec<String>,
    /// The socket the craft will serve on.
    pub socket: PathBuf,
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
    pub stdio: flowcraft_kernel::config::CraftStdio,
}

impl CraftSpec {
    /// A spec for an executable that serves `socket`.
    #[must_use]
    pub fn new(name: impl Into<String>, argv: Vec<String>, socket: impl Into<PathBuf>) -> Self {
        Self {
            name: name.into(),
            argv,
            socket: socket.into(),
            env: BTreeMap::new(),
            ready_timeout: Duration::from_secs(10),
            unit: None,
            tenant: String::new(),
            token: None,
            // The conservative default: a spec that says nothing keeps the
            // historical behaviour rather than silently swallowing output.
            stdio: flowcraft_kernel::config::CraftStdio::Inherit,
        }
    }
}

/// Make the craft socket owner-only, and its directory too.
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
            .map_err(flowcraft_client::grpc::from_status)?
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
        if spec.socket.exists() {
            std::fs::remove_file(&spec.socket)?;
        }
        if let Some(parent) = spec.socket.parent() {
            std::fs::create_dir_all(parent)?;
        }

        let mut command = Command::new(&spec.argv[0]);
        command.args(&spec.argv[1..]);
        command.envs(&spec.env);
        if let Some(token) = &spec.token {
            command.env("FLOWCRAFT_CRAFT_TOKEN", token);
        }
        command.kill_on_drop(true);
        match spec.stdio {
            flowcraft_kernel::config::CraftStdio::Capture => {
                command.stdout(std::process::Stdio::piped());
                command.stderr(std::process::Stdio::piped());
            }
            flowcraft_kernel::config::CraftStdio::Inherit => {}
            flowcraft_kernel::config::CraftStdio::Ignore => {
                command.stdout(std::process::Stdio::null());
                command.stderr(std::process::Stdio::null());
            }
        }
        let mut child = command.spawn().map_err(|error| {
            Error::not_available(format!("craft {:?}: cannot start: {error}", spec.name))
        })?;
        crate::craft_stdio::attach(&spec.name, &mut child, spec.stdio);

        if let Err(error) = wait_for_socket(&spec.socket, spec.ready_timeout).await {
            let _ = child.kill().await;
            return Err(error);
        }
        if let Err(error) = secure_socket(&spec.socket) {
            let _ = child.kill().await;
            return Err(error);
        }
        let client = match CraftClient::connect_with_token(
            &Target::Unix(spec.socket.clone()),
            None,
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

async fn wait_for_socket(path: &Path, timeout: Duration) -> Result<()> {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if tokio::net::UnixStream::connect(path).await.is_ok() {
            return Ok(());
        }
        if tokio::time::Instant::now() >= deadline {
            return Err(Error::not_available(format!(
                "craft did not listen on {} within {timeout:?}",
                path.display()
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
        wait_for_socket(&spec.socket, spec.ready_timeout).await?;
        secure_socket(&spec.socket)?;
        let client = CraftClient::connect_with_token(
            &Target::Unix(spec.socket.clone()),
            None,
            spec.token.as_deref(),
        )
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
        let ready = wait_for_socket(&spec.socket, spec.ready_timeout).await;
        if let Err(error) = ready {
            let _ = self.systemctl(&["stop", &unit]).await;
            return Err(error);
        }
        if let Err(error) = secure_socket(&spec.socket) {
            let _ = self.systemctl(&["stop", &unit]).await;
            return Err(error);
        }
        let client = match CraftClient::connect_with_token(
            &Target::Unix(spec.socket.clone()),
            None,
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
