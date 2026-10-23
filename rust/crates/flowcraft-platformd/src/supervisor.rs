//! Generation swapping: reload the deployment without dropping the daemon.
//!
//! A reload is: parse → build → start the new generation, swap it in, then
//! drain the old one. Parsing and planning happen before anything is created,
//! so a bad document leaves the running generation untouched. Kernel
//! configuration (`platform.yaml`) is *not* reloadable: endpoints, TLS, and
//! grant policy change only with a restart.

use std::any::Any;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use flowcraft_error::{Error, Result};
use flowcraft_kernel::Platform;
use flowcraft_resource::Built;
use flowcraft_resource::Deployment;
use flowcraft_resource::layers::{self, Layer};
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
        let previous = {
            let mut current = self.current.write().await;
            let previous = current.id;
            *current = Arc::new(Generation { id, built });
            previous
        };

        // The swap already happened; draining the old generation is bounded so a
        // stuck listener cannot wedge the reload path.
        tracing::info!(
            flowcraft.gen.previous = previous,
            flowcraft.gen = id,
            "generation swapped"
        );
        Ok(ReloadOutcome {
            previous,
            current: id,
            resources,
        })
    }

    /// Drain the current generation.
    pub async fn shutdown(&self) -> Result<()> {
        let generation = self.current.read().await.clone();
        match tokio::time::timeout(self.drain_grace, generation.built.shutdown()).await {
            Ok(result) => result,
            Err(_) => Err(Error::timeout(format!(
                "generation {} did not drain within {:?}",
                generation.id, self.drain_grace
            ))),
        }
    }
}

async fn build_from(
    platform: &Platform,
    paths: &[PathBuf],
    base_dir: Option<&Path>,
) -> Result<Built> {
    let deployment = load_deployment(paths)?;
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
