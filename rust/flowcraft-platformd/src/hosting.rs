//! Hosted crafts: launch every configured craft and hand it grants.
//!
//! Crafts are not part of a deployment generation: they survive `SIGHUP`, and
//! a restart is how a craft's argv or grants change. Their sessions are bound to
//! the platform's session, so a shutdown revokes every lease they hold.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use flowcraft::error::{Error, Result};
use flowcraft::kernel::{CraftConfig, EndpointProvider, Platform};
use flowcraft::logerr;
use flowcraft::proto::craft as pb;

use crate::craft::{
    CraftHandle, CraftSpec, CraftSupervisor, ExecSupervisor, ExternalSupervisor, SystemdSupervisor,
};

struct Running {
    supervisor: Arc<dyn CraftSupervisor>,
    handle: CraftHandle,
    renewal: Option<tokio::task::JoinHandle<()>>,
}

/// The crafts a daemon is hosting.
pub struct HostedCrafts {
    running: Vec<Running>,
}

impl std::fmt::Debug for HostedCrafts {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HostedCrafts")
            .field("crafts", &self.names())
            .finish()
    }
}

impl HostedCrafts {
    /// Start every craft, or none: a failure stops what already started.
    ///
    /// Takes an `Arc` because each hosted craft keeps a renewal task that
    /// re-mints its grants before they expire.
    pub async fn start(
        platform: Arc<Platform>,
        crafts: &BTreeMap<String, CraftConfig>,
    ) -> Result<Self> {
        let mut running: Vec<Running> = Vec::with_capacity(crafts.len());
        for (name, config) in crafts {
            let supervisor: Arc<dyn CraftSupervisor> = match config.supervisor.as_str() {
                "external" => Arc::new(ExternalSupervisor::new()),
                "systemd" => {
                    if config.systemctl.trim().is_empty() {
                        Arc::new(SystemdSupervisor::new())
                    } else {
                        Arc::new(SystemdSupervisor::with_systemctl(&config.systemctl))
                    }
                }
                _ => Arc::new(ExecSupervisor::new()),
            };
            match launch(&platform, supervisor.as_ref(), name, config).await {
                Ok(handle) => {
                    tracing::info!(
                        flowcraft.craft = name,
                        flowcraft.session = handle.session(),
                        flowcraft.grants = handle.grants().len(),
                        flowcraft.supervisor = config.supervisor.as_str(),
                        "craft started"
                    );
                    let renewal = renewal_task(&platform, config, &handle);
                    running.push(Running {
                        supervisor,
                        handle,
                        renewal,
                    });
                }
                Err(error) => {
                    for started in running {
                        let _ = started.supervisor.stop(started.handle).await;
                    }
                    return Err(Error::new(error.kind(), format!("crafts.{name}: {error}")));
                }
            }
        }
        Ok(Self { running })
    }

    /// The started crafts.
    #[must_use]
    pub fn handles(&self) -> Vec<&CraftHandle> {
        self.running.iter().map(|running| &running.handle).collect()
    }

    /// Names of the crafts that started.
    #[must_use]
    pub fn names(&self) -> Vec<&str> {
        self.running
            .iter()
            .map(|running| running.handle.info().name.as_str())
            .collect()
    }

    /// Number of hosted crafts.
    #[must_use]
    pub fn len(&self) -> usize {
        self.running.len()
    }

    /// Whether nothing is hosted.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.running.is_empty()
    }

    /// Close every session and stop every process this daemon started.
    pub async fn stop(self) -> Result<()> {
        for running in self.running {
            if let Some(renewal) = running.renewal {
                renewal.abort();
                let _ = renewal.await;
            }
            if let Err(failure) = running.supervisor.stop(running.handle).await {
                logerr!(warn, failure, "craft stop failed");
            }
        }
        Ok(())
    }
}

/// Mint one grant per configured capability.
fn mint_grants(platform: &Platform, config: &CraftConfig) -> Result<Vec<pb::Grant>> {
    let mut grants = Vec::with_capacity(config.grants.len());
    for request in &config.grants {
        // Resolve through the provider trait: it yields the parsed target and
        // the TLS material the craft will need.
        let resolved = EndpointProvider::resolve(platform, &request.endpoint)?;
        let lease = platform.grant(
            &request.endpoint,
            &request.capability,
            platform.session(),
            &config.tenant,
            request.scope.clone(),
        )?;
        grants.push(pb::Grant {
            capability: lease.capability.clone(),
            endpoint: resolved.target.as_uri(),
            token: lease.token.clone(),
            expires_at_unix: i64::try_from(lease.expires_at).unwrap_or(i64::MAX),
            scope: serde_json::to_vec(&lease.scope)
                .map_err(|error| Error::internal(format!("grant scope: {error}")))?,
        });
    }
    Ok(grants)
}

/// Renew at half the platform TTL, at least once a second.
fn renewal_interval(ttl_secs: u64) -> Duration {
    Duration::from_secs((ttl_secs / 2).max(1))
}

/// Keep a hosted craft's grants alive for the life of its session.
fn renewal_task(
    platform: &Arc<Platform>,
    config: &CraftConfig,
    handle: &CraftHandle,
) -> Option<tokio::task::JoinHandle<()>> {
    if config.grants.is_empty() {
        return None;
    }
    let platform = platform.clone();
    let config = config.clone();
    let client = handle.client();
    let session = handle.session().to_owned();
    let name = handle.info().name.clone();
    let interval = renewal_interval(platform.config().grants.ttl_secs);
    Some(tokio::spawn(async move {
        loop {
            tokio::time::sleep(interval).await;
            let grants = match mint_grants(&platform, &config) {
                Ok(grants) => grants,
                Err(failure) => {
                    logerr!(
                        warn,
                        failure,
                        "craft grant renewal: mint failed",
                        flowcraft.craft = %name
                    );
                    continue;
                }
            };
            match client
                .renew(pb::RenewRequest {
                    session: session.clone(),
                    grants,
                })
                .await
            {
                Ok(_) => tracing::debug!(flowcraft.craft = %name, "craft grants renewed"),
                Err(failure) => {
                    logerr!(
                        warn,
                        failure,
                        "craft grant renewal failed",
                        flowcraft.craft = %name
                    );
                    break;
                }
            }
        }
    }))
}

#[tracing::instrument(name = "craft.launch", skip_all, fields(flowcraft.craft = %name))]
async fn launch(
    platform: &Platform,
    supervisor: &dyn CraftSupervisor,
    name: &str,
    config: &CraftConfig,
) -> Result<CraftHandle> {
    let grants = mint_grants(platform, config)?;

    let mut spec = CraftSpec::new(name, config.argv.clone(), config.socket.clone());
    spec.env = config.env.clone();
    spec.ready_timeout = Duration::from_secs(config.ready_timeout_secs);
    spec.unit = Some(config.unit.clone()).filter(|unit| !unit.trim().is_empty());
    spec.tenant = config.tenant.clone();
    // One bootstrap token per craft: handed to `exec` via the environment, sent
    // as metadata by the platform on every call after Describe.
    spec.token = Some(format!("{:032x}", rand::random::<u128>()));
    spec.stdio = stdio_policy(&platform.config().log, &config.supervisor);
    supervisor.start(&spec, grants).await
}

/// What to do with a craft's own output.
///
/// `exec` captures by default: this daemon spawned the process, so it owns its
/// streams and is the only thing that can attribute them. A `systemd` unit is
/// already collected by the journal, and capturing it would duplicate every
/// line. An explicit `log.craft_stdio` wins over both.
fn stdio_policy(
    log: &flowcraft::kernel::config::LogConfig,
    supervisor: &str,
) -> flowcraft::kernel::config::CraftStdio {
    let default = if supervisor.trim() == "systemd" {
        flowcraft::kernel::config::CraftStdio::Inherit
    } else {
        flowcraft::kernel::config::CraftStdio::Capture
    };
    flowcraft::kernel::config::CraftStdio::parse(&log.craft_stdio, default).unwrap_or(default)
}
