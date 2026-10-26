//! The session table shared by every local backend.
//!
//! One table per runner, and the only place sessions are remembered: the
//! endpoint asks the runner instead of keeping a second map, so there is one
//! answer to "is this session open".
//!
//! Three properties are load-bearing and tested:
//!
//! - ids are unique: a duplicate while the earlier session is open is a
//!   `Conflict`, never an overwrite;
//! - a start is *pending* until the child is spawned, and a concurrent
//!   `close_session` waits for it to settle instead of racing it;
//! - [`Registry::close`] drains with a per-session and an overall deadline, so
//!   one wedged `terminate` cannot hang shutdown forever.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use crate::error::{Error, Result};
use crate::resource::BoxFuture;
use tokio::sync::watch;

use crate::caps::sandbox::codes::{self, Code};
use crate::caps::sandbox::contract::{Session, SessionInfo, SessionSpec};
use crate::caps::sandbox::session::log::now_nanos;

/// Upper bound on one session's teardown while the runner closes.
const CLOSE_PER_SESSION: Duration = Duration::from_secs(10);
/// Upper bound on the whole drain.
const CLOSE_TOTAL: Duration = Duration::from_secs(30);
/// Per-session bound once the overall deadline has passed: best effort, but
/// never unbounded.
const CLOSE_UNWIND: Duration = Duration::from_secs(1);

/// How one backend turns a spec into a launched session.
pub(crate) type Spawn =
    Arc<dyn Fn(SessionSpec) -> BoxFuture<'static, Result<Arc<dyn Session>>> + Send + Sync>;

/// A reserved id whose owner must clean up if it goes away.
///
/// `start` awaits the spawn while holding a `Pending` entry. If that future is
/// dropped — a client disconnects, an agent task is cancelled — no code after the
/// await runs, so the entry would stay `Pending` forever: `get` answers `Conflict`,
/// `close_session` waits for a settle that never arrives, and a retry is refused as
/// a duplicate. The guard makes the cleanup happen on drop instead.
struct Reservation<'a> {
    registry: &'a Registry,
    id: String,
    generation: u64,
    armed: bool,
}

impl<'a> Reservation<'a> {
    fn new(registry: &'a Registry, id: String, generation: u64) -> Self {
        Self {
            registry,
            id,
            generation,
            armed: true,
        }
    }

    /// The entry is now owned by the normal path (installed or removed).
    fn disarm(&mut self) {
        self.armed = false;
    }
}

impl Drop for Reservation<'_> {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        if let Ok(mut entries) = self.registry.entries.lock()
            && entries
                .get(&self.id)
                .is_some_and(|entry| entry.generation() == self.generation)
        {
            entries.remove(&self.id);
        }
        self.registry.settle();
    }
}

/// What the table holds for one id.
enum Entry {
    /// The id is reserved; the child is still being spawned.
    Pending { generation: u64 },
    /// The session exists.
    Ready {
        generation: u64,
        session: Arc<dyn Session>,
        argv: Vec<String>,
        tty: bool,
        started_at_unix_nano: u64,
    },
}

impl Entry {
    fn generation(&self) -> u64 {
        match self {
            Self::Pending { generation } | Self::Ready { generation, .. } => *generation,
        }
    }
}

/// The session table of one runner.
pub(crate) struct Registry {
    entries: Mutex<BTreeMap<String, Entry>>,
    counter: AtomicU64,
    generation: AtomicU64,
    settles: watch::Sender<u64>,
}

fn poisoned() -> Error {
    Error::internal("sandbox: session registry lock poisoned")
}

impl std::fmt::Debug for Registry {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("sandbox.Registry")
    }
}

impl Default for Registry {
    fn default() -> Self {
        Self::new()
    }
}

impl Registry {
    pub(crate) fn new() -> Self {
        let (settles, _) = watch::channel(0);
        Self {
            entries: Mutex::new(BTreeMap::new()),
            counter: AtomicU64::new(1),
            generation: AtomicU64::new(1),
            settles,
        }
    }

    fn lock(&self) -> Result<MutexGuard<'_, BTreeMap<String, Entry>>> {
        self.entries.lock().map_err(|_| poisoned())
    }

    /// Announce that a pending entry resolved.
    fn settle(&self) {
        let next = self.settles.borrow().wrapping_add(1);
        self.settles.send_replace(next);
    }

    /// Reserve an id and a generation for a start.
    fn reserve(&self, requested: &str) -> Result<(String, u64)> {
        let id = if requested.trim().is_empty() {
            format!(
                "s-{}-{}",
                std::process::id(),
                self.counter.fetch_add(1, Ordering::Relaxed)
            )
        } else {
            requested.to_owned()
        };
        let generation = self.generation.fetch_add(1, Ordering::Relaxed);
        let mut entries = self.lock()?;
        if entries.contains_key(&id) {
            return Err(codes::conflict(
                Code::SessionExists,
                format!("sandbox: session {id:?} already exists"),
            ));
        }
        entries.insert(id.clone(), Entry::Pending { generation });
        Ok((id, generation))
    }

    /// Start one session through `spawn`.
    ///
    /// The table holds a pending entry for the whole spawn, so `list` never
    /// shows a session that does not exist yet and `close_session` can wait for
    /// one that is being created.
    pub(crate) async fn start(&self, spec: SessionSpec, spawn: &Spawn) -> Result<Arc<dyn Session>> {
        let (id, generation) = self.reserve(&spec.id)?;
        let mut reservation = Reservation::new(self, id.clone(), generation);
        let mut spec = spec;
        spec.id = id.clone();
        let argv = spec.argv.clone();
        let tty = spec.tty;

        let outcome = spawn(spec).await;
        // A close may have claimed the pending entry while the spawn ran; the
        // generation is what tells the two apart. The guard is scoped so no lock
        // is held across the `close` await below.
        let still_ours = self
            .lock()?
            .get(&id)
            .is_some_and(|entry| entry.generation() == generation);

        match outcome {
            Ok(session) => {
                if !still_ours {
                    // `close` claimed the reservation while the spawn ran: the
                    // runner has declared everything closed, so the child must not
                    // be handed out — nobody could ever reach or kill it again.
                    reservation.disarm();
                    let _ = session.close().await;
                    self.settle();
                    return Err(Error::closed(
                        "sandbox: the runner closed while this session was starting",
                    ));
                }
                // Synchronous block: a `MutexGuard` must never be alive across
                // the await below (the future has to stay `Send`).
                let installed = (|| -> Result<()> {
                    let mut entries = self.lock()?;
                    entries.insert(
                        id,
                        Entry::Ready {
                            generation,
                            session: session.clone(),
                            argv,
                            tty,
                            started_at_unix_nano: now_nanos(),
                        },
                    );
                    Ok(())
                })();
                if let Err(error) = installed {
                    reservation.disarm();
                    let _ = session.close().await;
                    self.settle();
                    return Err(error);
                }
                reservation.disarm();
                self.settle();
                Ok(session)
            }
            Err(error) => {
                if still_ours && let Ok(mut entries) = self.lock() {
                    entries.remove(&id);
                }
                reservation.disarm();
                self.settle();
                Err(error)
            }
        }
    }

    /// Wait until `id` is no longer pending (or is gone).
    async fn settled(&self, id: &str) -> Result<()> {
        loop {
            let mut settles = self.settles.subscribe();
            let seen = *settles.borrow_and_update();
            let pending = match self.lock()?.get(id) {
                None => return Ok(()),
                Some(Entry::Ready { .. }) => return Ok(()),
                Some(Entry::Pending { .. }) => true,
            };
            debug_assert!(pending);
            if *settles.borrow_and_update() != seen {
                continue;
            }
            settles.changed().await.map_err(|_| poisoned())?;
        }
    }

    /// Look up a live session.
    pub(crate) fn get(&self, id: &str) -> Result<Arc<dyn Session>> {
        match self.lock()?.get(id) {
            Some(Entry::Ready { session, .. }) => Ok(session.clone()),
            Some(Entry::Pending { .. }) => Err(codes::conflict(
                Code::SessionExists,
                format!("sandbox: session {id:?} is still starting"),
            )),
            None => Err(codes::not_found(
                Code::SessionNotFound,
                format!("sandbox: session {id:?}"),
            )),
        }
    }

    /// Every known session.
    pub(crate) fn list(&self) -> Result<Vec<SessionInfo>> {
        let entries = self.lock()?;
        let mut infos = Vec::with_capacity(entries.len());
        for entry in entries.values() {
            let Entry::Ready {
                session,
                argv,
                tty,
                started_at_unix_nano,
                ..
            } = entry
            else {
                continue;
            };
            let exit = session.exit();
            infos.push(SessionInfo {
                id: session.id().to_owned(),
                argv: argv.clone(),
                tty: *tty,
                pid: session.pid(),
                started_at_unix_nano: *started_at_unix_nano,
                features: session.features(),
                running: exit.is_none(),
                exit,
            });
        }
        Ok(infos)
    }

    /// Stop one session's process group, keeping it listable.
    pub(crate) async fn terminate(&self, id: &str) -> Result<()> {
        self.get(id)?.terminate().await
    }

    /// Release one session and forget it.
    pub(crate) async fn close_session(&self, id: &str) -> Result<()> {
        self.settled(id).await?;
        let session = self.get(id)?;
        session.close().await?;
        self.lock()?.remove(id);
        self.settle();
        Ok(())
    }

    /// Release every session, bounded by the drain deadlines.
    pub(crate) async fn close(&self) -> Result<()> {
        let deadline = tokio::time::Instant::now() + CLOSE_TOTAL;
        let sessions: Vec<(String, Arc<dyn Session>)> = {
            let entries = self.lock()?;
            entries
                .iter()
                .filter_map(|(id, entry)| match entry {
                    Entry::Ready { session, .. } => Some((id.clone(), session.clone())),
                    Entry::Pending { .. } => None,
                })
                .collect()
        };
        for (id, session) in sessions {
            let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
            let budget = remaining.min(CLOSE_PER_SESSION);
            if budget.is_zero() {
                // Out of time. Every remaining session still gets one bounded
                // best-effort attempt — skipping the rest would leave them
                // running *and* forget them in the same breath.
                let _ = tokio::time::timeout(CLOSE_UNWIND, session.terminate()).await;
                self.lock()?.remove(&id);
                continue;
            }
            let _ = tokio::time::timeout(budget, session.close()).await;
            self.lock()?.remove(&id);
        }
        self.lock()?.clear();
        self.settle();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::caps::sandbox::contract::{ExitStatus, SessionFeatures};
    use crate::error::Result as SandboxResult;
    use async_trait::async_trait;

    /// A session that ends immediately.
    #[derive(Debug)]
    struct Done {
        id: String,
    }

    #[async_trait]
    impl Session for Done {
        fn id(&self) -> &str {
            &self.id
        }
        fn pid(&self) -> Option<u32> {
            None
        }
        fn features(&self) -> SessionFeatures {
            SessionFeatures::default()
        }
        fn exit(&self) -> Option<ExitStatus> {
            Some(ExitStatus::exited(0))
        }
        async fn read(
            &self,
            after_seq: u64,
            _max: usize,
        ) -> SandboxResult<crate::caps::sandbox::contract::ReadResult> {
            Ok(crate::caps::sandbox::contract::ReadResult {
                next_seq: after_seq,
                chunks: Vec::new(),
                eof: true,
            })
        }
        async fn write(&self, _data: &[u8]) -> SandboxResult<()> {
            Ok(())
        }
        async fn close_input(&self) -> SandboxResult<()> {
            Ok(())
        }
        async fn resize(&self, _rows: u16, _cols: u16) -> SandboxResult<()> {
            Ok(())
        }
        async fn signal(
            &self,
            _signal: crate::caps::sandbox::contract::Signal,
        ) -> SandboxResult<()> {
            Ok(())
        }
        async fn terminate(&self) -> SandboxResult<()> {
            Ok(())
        }
        async fn wait(&self) -> SandboxResult<ExitStatus> {
            Ok(ExitStatus::exited(0))
        }
        fn watch(&self) -> SandboxResult<crate::caps::sandbox::contract::EventStream> {
            Err(Error::not_available("no events"))
        }
        async fn close(&self) -> SandboxResult<()> {
            Ok(())
        }
    }

    fn spawn_done() -> Spawn {
        Arc::new(|spec: SessionSpec| {
            Box::pin(async move { Ok(Arc::new(Done { id: spec.id }) as Arc<dyn Session>) })
        })
    }

    #[tokio::test]
    async fn an_empty_id_is_generated_and_unique() {
        let registry = Registry::new();
        let spawn = spawn_done();
        let first = registry
            .start(SessionSpec::new(vec!["true".to_owned()]), &spawn)
            .await
            .unwrap();
        let second = registry
            .start(SessionSpec::new(vec!["true".to_owned()]), &spawn)
            .await
            .unwrap();
        assert_ne!(first.id(), second.id());
        assert_eq!(registry.list().unwrap().len(), 2);
    }

    #[tokio::test]
    async fn a_duplicate_id_is_a_conflict() {
        let registry = Registry::new();
        let spawn = spawn_done();
        let mut spec = SessionSpec::new(vec!["true".to_owned()]);
        spec.id = "fixed".to_owned();
        registry.start(spec.clone(), &spawn).await.unwrap();
        let error = registry.start(spec, &spawn).await.expect_err("conflict");
        assert_eq!(error.kind(), crate::error::ErrorKind::Conflict);
    }

    #[tokio::test]
    async fn a_failed_spawn_does_not_leave_a_reservation() {
        let registry = Registry::new();
        let failing: Spawn =
            Arc::new(|_spec: SessionSpec| Box::pin(async move { Err(Error::not_available("no")) }));
        let mut spec = SessionSpec::new(vec!["true".to_owned()]);
        spec.id = "reusable".to_owned();
        assert!(registry.start(spec.clone(), &failing).await.is_err());
        registry.start(spec, &spawn_done()).await.unwrap();
    }

    #[tokio::test]
    async fn close_session_waits_for_a_pending_start() {
        let registry = Arc::new(Registry::new());
        let slow: Spawn = Arc::new(|spec: SessionSpec| {
            Box::pin(async move {
                tokio::time::sleep(Duration::from_millis(50)).await;
                Ok(Arc::new(Done { id: spec.id }) as Arc<dyn Session>)
            })
        });
        let starting = {
            let registry = registry.clone();
            let slow = slow.clone();
            tokio::spawn(async move {
                let mut spec = SessionSpec::new(vec!["true".to_owned()]);
                spec.id = "slow".to_owned();
                registry.start(spec, &slow).await
            })
        };
        tokio::time::sleep(Duration::from_millis(10)).await;
        // The close arrives while the start is pending: it must wait, then
        // release, rather than fail with "not found".
        registry.close_session("slow").await.unwrap();
        assert!(starting.await.unwrap().is_ok());
        assert!(registry.list().unwrap().is_empty());
    }

    #[tokio::test]
    async fn close_session_forgets_the_session() {
        let registry = Registry::new();
        let session = registry
            .start(SessionSpec::new(vec!["true".to_owned()]), &spawn_done())
            .await
            .unwrap();
        let id = session.id().to_owned();
        assert!(registry.get(&id).is_ok());
        registry.close_session(&id).await.unwrap();
        assert_eq!(
            registry.get(&id).err().unwrap().kind(),
            crate::error::ErrorKind::NotFound
        );
        assert!(registry.list().unwrap().is_empty());
    }
}
