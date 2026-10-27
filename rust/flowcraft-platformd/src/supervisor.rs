//! Generation swapping: reload the deployment without dropping the daemon.
//!
//! A reload is: parse → build → start the new generation, swap it in, then
//! drain the old one under a bounded grace period. Parsing and planning happen
//! before anything is created, so a bad document leaves the running generation
//! untouched. Kernel configuration (`platform.yaml`) is *not* reloadable:
//! endpoints, TLS, and grant policy change only with a restart.
//!
//! The swap happens before the drain, so the new generation serves while the
//! old one tears down. A drain that fails or exceeds the grace is reported and
//! deferred to shutdown rather than failing the reload: the swap already
//! happened, and an error would claim nothing changed.

use std::any::Any;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use flowcraft::error::{Error, Result};
use flowcraft::kernel::Platform;
use flowcraft::logerr;
use flowcraft::resource::Built;
use flowcraft::resource::Deployment;
use flowcraft::resource::layers::{self, Layer};
use tokio::sync::{Mutex, RwLock};

/// What a reload produced.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ReloadOutcome {
    /// The generation that was running before the swap.
    pub previous: u64,
    /// The generation now serving.
    pub current: u64,
    /// How many resources the new generation built.
    pub resources: usize,
    /// Whether the retired generation shut down within the drain grace.
    ///
    /// `false` is reported, not fatal: the new generation is already serving, so
    /// the reload itself succeeded. The retired generation is kept and retried
    /// once more at process shutdown, inside one overall grace.
    pub drained: bool,
}

struct Generation {
    id: u64,
    built: Built,
}

/// Owns the running generation and the reload path.
pub struct Supervisor {
    platform: Arc<Platform>,
    layers: Vec<PathBuf>,
    base_dir: Option<PathBuf>,
    current: RwLock<Arc<Generation>>,
    next_id: AtomicU64,
    reload_gate: Mutex<()>,
    drain_grace: Duration,
    /// Generations a reload retired but could not drain within the grace.
    ///
    /// Retaining them is what gives process shutdown a second attempt: dropping
    /// the handle would stop only the resources whose teardown is drop-driven
    /// and silently forget the rest (a sandbox runner and the children it owns,
    /// for example).
    stranded: Mutex<Vec<Arc<Generation>>>,
}

impl Supervisor {
    /// Boot generation 1 from `layers` (base first, overlays after).
    pub async fn boot(platform: Platform, layers: impl Into<Vec<PathBuf>>) -> Result<Arc<Self>> {
        let layers = layers.into();
        if layers.is_empty() {
            return Err(Error::validation("serve needs at least one --deploy"));
        }
        let base_dir = layers[0].parent().map(Path::to_path_buf);
        let built = build_from(&platform, &layers, base_dir.as_deref()).await?;
        Ok(Arc::new(Self {
            platform: Arc::new(platform),
            layers,
            base_dir,
            current: RwLock::new(Arc::new(Generation { id: 1, built })),
            next_id: AtomicU64::new(2),
            reload_gate: Mutex::new(()),
            drain_grace: Duration::from_secs(10),
            stranded: Mutex::new(Vec::new()),
        }))
    }

    /// Override how long the old generation may take to drain.
    #[must_use]
    pub fn with_drain_grace(mut self, grace: Duration) -> Self {
        self.drain_grace = grace;
        self
    }

    /// The generation currently serving.
    pub async fn generation(&self) -> u64 {
        self.current.read().await.id
    }

    /// Read a resource out of the current generation.
    pub async fn resource<T: Any + Send + Sync>(&self, name: &str) -> Result<Arc<T>> {
        let generation = self.current.read().await.clone();
        generation.built.get::<T>(name)
    }

    /// The current generation's `entries:` — the resources the document asks
    /// the host to hand back.
    pub async fn entries(&self) -> Vec<String> {
        let generation = self.current.read().await.clone();
        generation
            .built
            .entries()
            .into_iter()
            .map(|(name, _)| name)
            .collect()
    }

    /// The kernel behind the generations.
    #[must_use]
    pub fn platform(&self) -> Arc<Platform> {
        self.platform.clone()
    }

    /// Build, start, swap, drain. One reload at a time.
    ///
    /// The span is the unit an operator reasons about: every record the rebuild
    /// emits — plan, build, start, bind — is inside it, and carries the
    /// generation once it is known.
    #[tracing::instrument(
        name = "generation.reload",
        skip_all,
        fields(flowcraft.gen = tracing::field::Empty)
    )]
    pub async fn reload(&self) -> Result<ReloadOutcome> {
        let _gate = self.reload_gate.lock().await;

        // Parse before touching the running generation: a syntax error must not
        // cost a healthy deployment.
        let built = build_from(&self.platform, &self.layers, self.base_dir.as_deref()).await?;
        let resources = built.names().len();

        let id = self.next_id.fetch_add(1, Ordering::SeqCst);
        tracing::Span::current().record("flowcraft.gen", id);
        // Keep the retired generation alive across the swap: dropping it here
        // would only stop the listeners whose teardown happens to be
        // drop-driven, and skip every explicit `shutdown` (a sandbox runner
        // closing its sessions, for example).
        let retired = {
            let mut current = self.current.write().await;
            let retired = current.clone();
            *current = Arc::new(Generation { id, built });
            retired
        };

        tracing::info!(
            flowcraft.gen.previous = retired.id,
            flowcraft.gen = id,
            "generation swapped"
        );

        // The swap already happened; draining the retired generation is bounded
        // so a stuck listener cannot wedge the reload path.
        let drained = self.retire(&retired).await;
        Ok(ReloadOutcome {
            previous: retired.id,
            current: id,
            resources,
            drained,
        })
    }

    /// Drain the current generation, then retry whatever a reload could not.
    pub async fn shutdown(&self) -> Result<()> {
        let current = self.current.read().await.clone();
        let mut first = self.drain(&current).await.err();

        // One more bounded attempt for stranded generations: the first wait
        // already happened at reload, and the retries share one overall grace so
        // a pathological series of stuck drains cannot hold exit open for a full
        // grace each.
        let stranded: Vec<Arc<Generation>> = std::mem::take(&mut *self.stranded.lock().await);
        let deadline = tokio::time::Instant::now() + self.drain_grace;
        for generation in stranded {
            let grace = deadline.saturating_duration_since(tokio::time::Instant::now());
            if let Err(error) = self.drain_with(&generation, grace).await {
                logerr!(
                    warn,
                    error,
                    "stranded generation did not drain",
                    flowcraft.gen = generation.id
                );
                if first.is_none() {
                    first = Some(error);
                }
            }
        }
        match first {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }

    /// Shut one generation down, bounded by the drain grace.
    async fn drain(&self, generation: &Generation) -> Result<()> {
        self.drain_with(generation, self.drain_grace).await
    }

    /// Shut one generation down, bounded by `grace`.
    async fn drain_with(&self, generation: &Generation, grace: Duration) -> Result<()> {
        match tokio::time::timeout(grace, generation.built.shutdown()).await {
            Ok(result) => result,
            Err(_) => Err(Error::timeout(format!(
                "generation {} did not drain within {grace:?}",
                generation.id
            ))),
        }
    }

    /// Retire a generation that a swap replaced.
    ///
    /// The new generation is already serving, so a failure here never becomes a
    /// reload error: the generation is kept for a retry at shutdown and the
    /// outcome reports `drained: false`.
    async fn retire(&self, generation: &Arc<Generation>) -> bool {
        match self.drain(generation).await {
            Ok(()) => {
                tracing::info!(flowcraft.gen = generation.id, "generation drained");
                true
            }
            Err(error) => {
                logerr!(
                    warn,
                    error,
                    "retired generation did not drain; retrying at shutdown",
                    flowcraft.gen = generation.id
                );
                self.stranded.lock().await.push(generation.clone());
                false
            }
        }
    }
}

async fn build_from(
    platform: &Platform,
    paths: &[PathBuf],
    base_dir: Option<&Path>,
) -> Result<Built> {
    // Reading and merging the layers is file I/O plus a sync `saphyr` parse, on
    // the reload path that holds `reload_gate`: exactly what must not run on a
    // worker.
    let layers = paths.to_vec();
    let deployment =
        flowcraft::blocking::run("platformd.layers", move || load_deployment(&layers)).await?;
    if deployment.runtime.is_some() {
        // The Go side reads `runtime:`; the Rust platform keeps host settings in
        // platform.yaml. Accept the field (documents are shared) but say so.
        tracing::warn!(
            "deploy.runtime is ignored by the Rust platform; configure the daemon in platform.yaml"
        );
    }
    let built = crate::assembly::build(platform, deployment, base_dir).await?;
    Ok(built)
}

/// Read each layer from disk, in order.
///
/// Synchronous on purpose: its callers are [`load_deployment`], which the daemon
/// only ever reaches through [`flowcraft::blocking::run`], and the one-shot CLI,
/// which has no executor to block.
#[allow(clippy::disallowed_methods)]
pub fn read_layers(paths: &[PathBuf]) -> Result<Vec<Layer>> {
    let mut layers = Vec::with_capacity(paths.len());
    for path in paths {
        let data = std::fs::read(path).map_err(|error| {
            Error::not_found(format!("cannot read {}: {error}", path.display()))
        })?;
        layers.push(Layer::new(path.display().to_string(), data));
    }
    Ok(layers)
}

/// Read, merge, and validate the layers.
pub fn load_deployment(paths: &[PathBuf]) -> Result<Deployment> {
    let (deployment, _provenance) = layers::load(&read_layers(paths)?)?;
    Ok(deployment)
}
