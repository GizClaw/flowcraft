//! The admin surface: one unix socket, one JSON request line, one JSON
//! response line.
//!
//! Owner-only by construction (directory 0700, socket 0600) and local by
//! design: there is no TCP admin listener. Commands are deliberately few —
//! `status`, `reload`, `drain`, `crafts`, `revoke`, `diag` — because everything
//! else is deployment-shaped and belongs in the documents.
//!
//! Every command is logged: this socket can stop a deployment, and "who asked
//! for that" has to be answerable from the records.

use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;

use flowcraft_error::{Error, Result};
use flowcraft_telemetry::logerr;
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::Mutex;
use tracing::Instrument as _;

use crate::logging::Applied;
use crate::supervisor::Supervisor;

/// What the admin surface reports and can change.
pub struct AdminState {
    supervisor: Arc<Supervisor>,
    crafts: Vec<String>,
    ready: Arc<AtomicBool>,
    endpoints: usize,
    started_at: Instant,
    reload_gate: Mutex<()>,
    telemetry: Arc<Applied>,
}

impl AdminState {
    /// Build the state for one running daemon.
    #[must_use]
    pub fn new(
        supervisor: Arc<Supervisor>,
        crafts: Vec<String>,
        ready: Arc<AtomicBool>,
        endpoints: usize,
        telemetry: Arc<Applied>,
    ) -> Self {
        Self {
            supervisor,
            crafts,
            ready,
            endpoints,
            started_at: Instant::now(),
            reload_gate: Mutex::new(()),
            telemetry,
        }
    }

    /// The readiness flag shared with the serve loop.
    #[must_use]
    pub fn ready(&self) -> Arc<AtomicBool> {
        self.ready.clone()
    }
}

/// A running admin listener.
pub struct AdminHandle {
    socket: PathBuf,
    task: Option<tokio::task::JoinHandle<()>>,
}

impl AdminHandle {
    /// Stop accepting and remove the socket.
    pub async fn stop(mut self) {
        if let Some(task) = self.task.take() {
            task.abort();
            let _ = task.await;
        }
        let _ = std::fs::remove_file(&self.socket);
    }
}

/// Bind the admin socket and serve requests until stopped.
pub async fn serve(socket: &Path, state: AdminState) -> Result<AdminHandle> {
    if let Some(parent) = socket.parent() {
        std::fs::create_dir_all(parent)?;
        std::fs::set_permissions(parent, std::fs::Permissions::from_mode(0o700))?;
    }
    if socket.exists() {
        std::fs::remove_file(socket)?;
    }
    let listener = tokio::net::UnixListener::bind(socket).map_err(|error| {
        Error::not_available(format!("admin socket {}: {error}", socket.display()))
    })?;
    std::fs::set_permissions(socket, std::fs::Permissions::from_mode(0o600))?;

    let state = Arc::new(state);
    let task = tokio::spawn(async move {
        loop {
            match listener.accept().await {
                Ok((stream, _)) => {
                    let state = state.clone();
                    tokio::spawn(handle_connection(stream, state));
                }
                Err(failure) => {
                    logerr!(
                        warn,
                        flowcraft_error::Error::from(failure),
                        "admin accept failed"
                    );
                    break;
                }
            }
        }
    });

    Ok(AdminHandle {
        socket: socket.to_path_buf(),
        task: Some(task),
    })
}

/// Only the daemon's own uid (or root) may drive the admin socket.
fn peer_allowed(peer_uid: u32, self_uid: u32) -> bool {
    peer_uid == self_uid || peer_uid == 0
}

async fn handle_connection(stream: tokio::net::UnixStream, state: Arc<AdminState>) {
    let allowed = match stream.peer_cred() {
        Ok(credentials) => peer_allowed(credentials.uid(), nix::unistd::geteuid().as_raw()),
        Err(_) => false,
    };
    let (read, mut write) = stream.into_split();
    if !allowed {
        let response = json!({"ok": false, "error": "admin socket is owner-only"});
        let _ = write.write_all(format!("{response}\n").as_bytes()).await;
        return;
    }
    let mut reader = BufReader::new(read);
    let mut line = String::new();
    if reader.read_line(&mut line).await.is_err() {
        return;
    }
    let request: Value = match serde_json::from_str(&line) {
        Ok(request) => request,
        Err(error) => json!({"command": "", "parse_error": error.to_string()}),
    };
    if let Some(error) = request.get("parse_error").and_then(|value| value.as_str()) {
        let response = json!({"ok": false, "error": format!("invalid JSON: {error}")});
        let _ = write.write_all(format!("{response}\n").as_bytes()).await;
        return;
    }
    // One span per command: the audit record and everything the command causes
    // share a trace, so "who asked for the reload" and "what the reload did"
    // stop being two unrelated stories.
    let command = request
        .get("command")
        .and_then(|value| value.as_str())
        .unwrap_or_default();
    let span = tracing::info_span!("admin", flowcraft.command = command);
    let response = dispatch(&request, &state).instrument(span).await;
    let _ = write.write_all(format!("{response}\n").as_bytes()).await;
    let _ = write.shutdown().await;
}

async fn dispatch(request: &Value, state: &AdminState) -> Value {
    let command = request.get("command").and_then(|value| value.as_str());
    tracing::info!(
        flowcraft.command = command.unwrap_or_default(),
        "admin command"
    );
    match command {
        Some("status") => {
            let generation = state.supervisor.generation().await;
            let entries = state.supervisor.entries().await;
            json!({
                "ok": true,
                "ready": state.ready.load(Ordering::SeqCst),
                "generation": generation,
                "entries": entries,
                "crafts": state.crafts,
                "endpoints": state.endpoints,
                "uptime_secs": state.started_at.elapsed().as_secs(),
            })
        }
        Some("reload") => {
            let _gate = state.reload_gate.lock().await;
            match state.supervisor.reload().await {
                Ok(outcome) => json!({
                    "ok": true,
                    "previous": outcome.previous,
                    "current": outcome.current,
                    "resources": outcome.resources,
                }),
                Err(error) => json!({"ok": false, "error": error.to_string()}),
            }
        }
        Some("drain") => {
            // Readiness off, and no new leases: existing work finishes, new
            // sessions cannot start.
            state.ready.store(false, Ordering::SeqCst);
            state.supervisor.platform().drain();
            json!({"ok": true, "ready": false})
        }
        Some("crafts") => json!({"ok": true, "crafts": state.crafts}),
        Some("diag") => {
            // Everything a support bundle needs about this process, except
            // anything that is itself a credential: the identity salt and any
            // token stay here.
            let resource = &state.telemetry.install.resource;
            json!({
                "ok": true,
                "service": resource.service,
                "version": resource.version,
                "install": resource.install,
                "install_scope": resource.install_scope,
                "install_epoch": resource.install_epoch,
                "principal": resource.principal,
                "session": resource.session,
                "format": match state.telemetry.install.format {
                    flowcraft_telemetry::Format::Text => "text",
                    flowcraft_telemetry::Format::Json => "json",
                },
                "profile": format!("{:?}", state.telemetry.install.profile),
                "directives": state.telemetry.guard.directives(),
                "file": state.telemetry.guard.file_path().map(|path| path.display().to_string()),
                "file_disabled": state.telemetry.guard.file_disabled(),
                "ring_bytes": state.telemetry.guard.ring_len(),
                "ring": state.telemetry.guard.ring_dump(),
            })
        }
        Some("revoke") => {
            let session = request
                .get("session")
                .and_then(|value| value.as_str())
                .unwrap_or_default();
            if session.is_empty() {
                return json!({"ok": false, "error": "revoke needs a session"});
            }
            match state.supervisor.platform().revoke_session(session) {
                Ok(()) => json!({"ok": true, "session": session}),
                Err(error) => json!({"ok": false, "error": error.to_string()}),
            }
        }
        Some(other) => json!({"ok": false, "error": format!("unknown command {other:?}")}),
        None => json!({"ok": false, "error": "command is required"}),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_owner_or_root_may_use_the_socket() {
        assert!(peer_allowed(1000, 1000));
        assert!(peer_allowed(0, 1000));
        assert!(!peer_allowed(1001, 1000));
    }
}
