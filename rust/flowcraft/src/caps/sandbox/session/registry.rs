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
//!
//! A fourth property makes the table survivable: it is **bounded and
//! self-reclaiming**. A session is listed while it runs, and for a while after it
//! exits so its tail and its exit status stay readable, but never past
//! [`SessionLimits::exit_retention`] and never past the table's own limits. The
//! failure this prevents is the cheap one: a client that starts sessions and never
//! closes them would otherwise grow the table — and the output buffers it pins —
//! without bound.
//!
//! Reclaiming is driven by [`Registry::sweep`], which the supervise tick every
//! listed process session already runs every 50 ms, so no task exists for the
//! table alone. The bounds are additionally enforced inside [`Registry::start`],
//! so what the table holds is bounded by its limits even while no tick runs.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use crate::error::{Error, Result};
use crate::resource::BoxFuture;
use tokio::sync::watch;

use crate::caps::sandbox::codes::{self, Code};
use crate::caps::sandbox::contract::{Session, SessionInfo, SessionSpec};
use crate::caps::sandbox::session::log::{Log, now_nanos};

/// Sessions listed at once when the deployment says nothing.
pub(crate) const DEFAULT_MAX_SESSIONS: usize = 64;
/// Seconds an exited session stays readable when the deployment says nothing.
pub(crate) const DEFAULT_EXIT_RETENTION_SECS: u64 = 5 * 60;
/// Output bytes retained across every session when the deployment says nothing.
pub(crate) const DEFAULT_MAX_TOTAL_LOG_BYTES: u64 = 256 * 1024 * 1024;

/// The bounds the table enforces.
///
/// Each is a count of entries or a byte total, never a rate: a deployment sees
/// the value it configured, and a refusal names the limit that caused it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct SessionLimits {
    /// Sessions listed at once, running and retained. `0` = no bound.
    pub(crate) max_sessions: usize,
    /// How long an exited session stays readable, so a client can still collect
    /// its tail and its exit status. `0` = reclaim it as soon as the exit shows.
    pub(crate) exit_retention: Duration,
    /// Retained output across every session, in bytes. `0` = no bound.
    ///
    /// Separate from a session's own `max_output_bytes`, because those multiply:
    /// 64 sessions each inside a 10 MiB budget still add up to 640 MiB.
    pub(crate) max_total_log_bytes: u64,
}

impl Default for SessionLimits {
    fn default() -> Self {
        Self {
            max_sessions: DEFAULT_MAX_SESSIONS,
            exit_retention: Duration::from_secs(DEFAULT_EXIT_RETENTION_SECS),
            max_total_log_bytes: DEFAULT_MAX_TOTAL_LOG_BYTES,
        }
    }
}

/// Upper bound on one session's teardown while the runner closes.
const CLOSE_PER_SESSION: Duration = Duration::from_secs(10);
/// Upper bound on the whole drain.
const CLOSE_TOTAL: Duration = Duration::from_secs(30);
/// Per-session bound once the overall deadline has passed: best effort, but
/// never unbounded.
const CLOSE_UNWIND: Duration = Duration::from_secs(1);

/// How one backend turns a spec into a launched session.
pub(crate) type Spawn =
    Arc<dyn Fn(SessionSpec) -> BoxFuture<'static, Result<Started>> + Send + Sync>;

/// What a backend hands back once a session exists.
///
/// The table needs more than the contract handle: the output buffer is what it
/// bounds, reclaims, and releases a blocked reader from, and only the backend
/// knows where that buffer is.
pub(crate) struct Started {
    pub(crate) session: Arc<dyn Session>,
    pub(crate) log: Arc<Log>,
}

impl std::fmt::Debug for Started {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.Started")
            .field("id", &self.session.id())
            .finish_non_exhaustive()
    }
}

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
    Ready(Ready),
}

/// One listed session.
struct Ready {
    generation: u64,
    session: Arc<dyn Session>,
    log: Arc<Log>,
    argv: Vec<String>,
    tty: bool,
    started_at_unix_nano: u64,
    /// When a sweep first saw this session's process exit.
    ///
    /// Retention is counted from here rather than from the instant the process
    /// died: the tick is what observes the exit, so the window a deployment
    /// configured is the window it gets. It is also the eviction order —
    /// "oldest exited" means finished longest.
    exited_at: Option<Instant>,
}

impl Entry {
    fn generation(&self) -> u64 {
        match self {
            Self::Pending { generation } => *generation,
            Self::Ready(ready) => ready.generation,
        }
    }
}

/// Stamp every session whose process has exited.
///
/// Two things are counted from this stamp: how long the entry is retained, and
/// which exited session is evicted first. A sweep is the only writer, so an entry
/// cannot claim to be older than the sweep that saw it exit.
fn observe_exits(entries: &mut BTreeMap<String, Entry>, now: Instant) {
    for entry in entries.values_mut() {
        let Entry::Ready(ready) = entry else {
            continue;
        };
        if ready.exited_at.is_none() && ready.session.exit().is_some() {
            ready.exited_at = Some(now);
        }
    }
}

/// Take one exited session out of the table.
///
/// The order is deliberate: an exited session whose output a client has already
/// read to the end goes first (nothing is owed to anyone), then the one that has
/// been finished longest. A **running** session is never a candidate — killing a
/// half-finished build to admit a new one is the worst answer available, because
/// it destroys work that was going to succeed and leaves the caller holding an
/// exit status that explains nothing.
fn evict_one(entries: &mut BTreeMap<String, Entry>) -> bool {
    let mut consumed: Option<(&str, Instant)> = None;
    let mut oldest: Option<(&str, Instant)> = None;
    for (id, entry) in entries.iter() {
        let Entry::Ready(ready) = entry else {
            continue;
        };
        let Some(exited_at) = ready.exited_at else {
            continue;
        };
        if oldest.is_none_or(|(_, at)| exited_at < at) {
            oldest = Some((id.as_str(), exited_at));
        }
        if ready.log.consumed() && consumed.is_none_or(|(_, at)| exited_at < at) {
            consumed = Some((id.as_str(), exited_at));
        }
    }
    let Some((id, _)) = consumed.or(oldest) else {
        return false;
    };
    let id = id.to_owned();
    let Some(Entry::Ready(ready)) = entries.remove(&id) else {
        return false;
    };
    // Release whoever is blocked reading it, then let the handle drop: the
    // process has already exited, so there is nothing left to stop.
    ready.log.reclaim();
    true
}

/// Retained output across the table, in bytes.
fn log_bytes(entries: &BTreeMap<String, Entry>) -> u64 {
    entries
        .values()
        .filter_map(|entry| match entry {
            Entry::Ready(ready) => Some(ready.log.bytes()),
            Entry::Pending { .. } => None,
        })
        .sum()
}

/// Reclaim every entry the retention window no longer owes anyone.
///
/// Two ways out of the table, both of them in the same direction: nothing is
/// owed. A session whose output a client read to the end may go at once, and one
/// whose window ran out goes even if nobody read it (the alternative is a client
/// that can pin an entry by not reading).
fn reclaim_expired(
    entries: &mut BTreeMap<String, Entry>,
    now: Instant,
    limits: &SessionLimits,
) -> usize {
    let mut reclaimed = 0;
    let expired: Vec<String> = entries
        .iter()
        .filter_map(|(id, entry)| {
            let Entry::Ready(ready) = entry else {
                return None;
            };
            let exited_at = ready.exited_at?;
            let past_window = limits.exit_retention.is_zero()
                || now.saturating_duration_since(exited_at) >= limits.exit_retention;
            (past_window || ready.log.consumed()).then(|| id.clone())
        })
        .collect();
    for id in expired {
        if let Some(Entry::Ready(ready)) = entries.remove(&id) {
            ready.log.reclaim();
            reclaimed += 1;
        }
    }
    reclaimed
}

/// Make room for one more session, or refuse it.
///
/// Two gates, because they fail for different reasons: the table may hold too
/// many sessions, and it may hold too many bytes. Both evict only exited sessions
/// and both end in a refusal rather than in truncating a live session — a
/// truncated log leaves a sequence gap that nothing in the output explains.
fn make_room(
    entries: &mut BTreeMap<String, Entry>,
    limits: &SessionLimits,
    now: Instant,
) -> Result<()> {
    observe_exits(entries, now);
    loop {
        let over_count = limits.max_sessions != 0 && entries.len() >= limits.max_sessions;
        let over_bytes =
            limits.max_total_log_bytes != 0 && log_bytes(entries) > limits.max_total_log_bytes;
        if !over_count && !over_bytes {
            return Ok(());
        }
        if !evict_one(entries) {
            let limit = if over_count {
                format!("sessions.max_sessions={}", limits.max_sessions)
            } else {
                format!(
                    "sessions.max_total_log_bytes={}",
                    limits.max_total_log_bytes
                )
            };
            return Err(codes::exhausted(
                Code::SessionLimit,
                format!(
                    "sandbox: cannot start a session while {} sessions are listed and none of \
                     them has exited; raise {limit} or close a session",
                    entries.len()
                ),
            ));
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

    /// Reserve an id and a generation for a start, making room first.
    fn reserve(
        &self,
        requested: &str,
        limits: &SessionLimits,
        now: Instant,
    ) -> Result<(String, u64)> {
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
        // A duplicate is refused before a single entry is evicted: a typo must not
        // cost a session that was going to keep working.
        if entries.contains_key(&id) {
            return Err(codes::conflict(
                Code::SessionExists,
                format!("sandbox: session {id:?} already exists"),
            ));
        }
        make_room(&mut entries, limits, now)?;
        entries.insert(id.clone(), Entry::Pending { generation });
        Ok((id, generation))
    }

    /// Start one session through `spawn`.
    ///
    /// The table holds a pending entry for the whole spawn, so `list` never
    /// shows a session that does not exist yet and `close_session` can wait for
    /// one that is being created.
    pub(crate) async fn start(
        &self,
        spec: SessionSpec,
        spawn: &Spawn,
        limits: &SessionLimits,
    ) -> Result<Arc<dyn Session>> {
        // Reclaim first: a start is the one moment the table is measured, and an
        // entry the retention window has released must not consume a slot.
        let now = Instant::now();
        self.sweep(now, limits);
        let (id, generation) = self.reserve(&spec.id, limits, now)?;
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
            Ok(started) => {
                let session = started.session;
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
                        Entry::Ready(Ready {
                            generation,
                            session: session.clone(),
                            log: started.log,
                            argv,
                            tty,
                            started_at_unix_nano: now_nanos(),
                            exited_at: None,
                        }),
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
                Some(Entry::Ready(_)) => return Ok(()),
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
            Some(Entry::Ready(ready)) => Ok(ready.session.clone()),
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

    /// Whether `id` is still listed, running or retained.
    ///
    /// This is how a session's own tick learns that its entry is gone, and so
    /// that the number of live ticks never exceeds the number of listed sessions.
    pub(crate) fn holds(&self, id: &str) -> bool {
        self.entries
            .lock()
            .is_ok_and(|entries| entries.contains_key(id))
    }

    /// Reclaim what nothing is owed anymore, and stamp freshly exited sessions.
    ///
    /// Driven by the 50 ms supervise tick of each listed process session, and by
    /// [`Registry::start`] before a limit is measured, so it needs no task of its
    /// own. A pty session has no supervise tick: its entry is reclaimed by the
    /// next sweep from any other session or start, which bounds the table but can
    /// leave an exited pty entry listed for longer than its window.
    /// `(ready, exited, retained bytes)` for the status surface.
    ///
    /// One lock, one pass: a caller that asked three times could see three
    /// different tables, which is exactly the reading an operator would
    /// mis-trust.
    pub(crate) fn stats(&self) -> (usize, usize, u64) {
        let Ok(entries) = self.entries.lock() else {
            return (0, 0, 0);
        };
        let mut ready = 0;
        let mut exited = 0;
        for entry in entries.values() {
            match entry {
                Entry::Pending { .. } => ready += 1,
                Entry::Ready(ready_entry) => {
                    if ready_entry.exited_at.is_some() {
                        exited += 1;
                    } else {
                        ready += 1;
                    }
                }
            }
        }
        (ready, exited, log_bytes(&entries))
    }

    pub(crate) fn sweep(&self, now: Instant, limits: &SessionLimits) {
        let Ok(mut entries) = self.entries.lock() else {
            return;
        };
        observe_exits(&mut entries, now);
        reclaim_expired(&mut entries, now, limits);
    }

    /// Every known session.
    pub(crate) fn list(&self) -> Result<Vec<SessionInfo>> {
        let entries = self.lock()?;
        let mut infos = Vec::with_capacity(entries.len());
        for entry in entries.values() {
            let Entry::Ready(ready) = entry else {
                continue;
            };
            let exit = ready.session.exit();
            infos.push(SessionInfo {
                id: ready.session.id().to_owned(),
                argv: ready.argv.clone(),
                tty: ready.tty,
                pid: ready.session.pid(),
                started_at_unix_nano: ready.started_at_unix_nano,
                features: ready.session.features(),
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
                    Entry::Ready(ready) => Some((id.clone(), ready.session.clone())),
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
    use crate::caps::sandbox::contract::{ExitStatus, ReadResult, SessionFeatures};
    use crate::error::ErrorKind;
    use crate::error::Result as SandboxResult;
    use async_trait::async_trait;
    use std::sync::atomic::AtomicBool;

    /// A session whose exit and output the test decides.
    ///
    /// `read` goes through a real [`Log`], so "consumed" is produced the way a
    /// client produces it instead of by poking a flag.
    #[derive(Debug)]
    struct Fake {
        id: String,
        exited: AtomicBool,
        log: Arc<Log>,
    }

    impl Fake {
        fn new(id: &str) -> Arc<Self> {
            Arc::new(Self {
                id: id.to_owned(),
                exited: AtomicBool::new(false),
                log: Arc::new(Log::new(0)),
            })
        }

        /// End the process without touching its log: the exit is what the table
        /// observes, and the output stays exactly as it was.
        fn finish(&self) {
            self.exited.store(true, Ordering::Release);
        }

        /// End the process *and* its output, the way a real session does.
        ///
        /// A test about "consumed" needs this one: consumption is defined by
        /// `Log::read` delivering `eof`, and `eof` is only reachable once the log
        /// has an exit. A fake that ends the process alone leaves a reader
        /// waiting on output that will never come.
        fn finish_with_output(&self) {
            self.log
                .finish(crate::caps::sandbox::contract::ExitStatus::exited(0));
            self.finish();
        }

        fn write(&self, text: &str) {
            self.log.push(
                crate::caps::sandbox::contract::Stream::Stdout,
                text.as_bytes(),
            );
        }

        async fn read_to_end(&self) {
            let mut after = 0;
            loop {
                let read = self.log.read(after, 4096).await.expect("read");
                after = read.next_seq;
                if read.eof {
                    return;
                }
            }
        }
    }

    #[async_trait]
    impl Session for Fake {
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
            self.exited
                .load(Ordering::Acquire)
                .then(|| ExitStatus::exited(0))
        }
        async fn read(&self, after_seq: u64, max: usize) -> SandboxResult<ReadResult> {
            self.log.read(after_seq, max).await
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

    /// A spawner that hands out fakes, keeping one handle per session so a test
    /// can end a process or read its output afterwards.
    fn spawner(sessions: &Arc<Mutex<Vec<Arc<Fake>>>>) -> Spawn {
        let sessions = sessions.clone();
        Arc::new(move |spec: SessionSpec| {
            let sessions = sessions.clone();
            Box::pin(async move {
                let fake = Fake::new(&spec.id);
                if let Ok(mut all) = sessions.lock() {
                    all.push(fake.clone());
                }
                Ok(Started {
                    log: fake.log.clone(),
                    session: fake as Arc<dyn Session>,
                })
            })
        })
    }

    /// The default bounds, with the output budget off so a test that is about
    /// the count is not accidentally about bytes.
    fn limits() -> SessionLimits {
        SessionLimits {
            max_total_log_bytes: 0,
            ..SessionLimits::default()
        }
    }

    async fn start(
        registry: &Registry,
        spawn: &Spawn,
        limits: &SessionLimits,
        id: &str,
    ) -> Result<Arc<dyn Session>> {
        let mut spec = SessionSpec::new(vec!["true".to_owned()]);
        spec.id = id.to_owned();
        registry.start(spec, spawn, limits).await
    }

    /// The ids the table currently lists, in order.
    fn listed(registry: &Registry) -> Vec<String> {
        registry
            .list()
            .expect("list")
            .into_iter()
            .map(|info| info.id)
            .collect()
    }

    #[tokio::test]
    async fn an_empty_id_is_generated_and_unique() {
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let limits = limits();
        let first = registry
            .start(SessionSpec::new(vec!["true".to_owned()]), &spawn, &limits)
            .await
            .expect("start");
        let second = registry
            .start(SessionSpec::new(vec!["true".to_owned()]), &spawn, &limits)
            .await
            .expect("start");
        assert_ne!(first.id(), second.id());
        assert_eq!(registry.list().expect("list").len(), 2);
    }

    #[tokio::test]
    async fn a_duplicate_id_is_a_conflict() {
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let limits = limits();
        start(&registry, &spawn, &limits, "fixed")
            .await
            .expect("start");
        let error = start(&registry, &spawn, &limits, "fixed")
            .await
            .expect_err("conflict");
        assert_eq!(error.kind(), ErrorKind::Conflict);
    }

    #[tokio::test]
    async fn a_failed_spawn_does_not_leave_a_reservation() {
        let registry = Registry::new();
        let failing: Spawn =
            Arc::new(|_spec: SessionSpec| Box::pin(async move { Err(Error::not_available("no")) }));
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let limits = limits();
        assert!(
            start(&registry, &failing, &limits, "reusable")
                .await
                .is_err()
        );
        start(&registry, &spawn, &limits, "reusable")
            .await
            .expect("the id is free again");
    }

    #[tokio::test]
    async fn close_session_waits_for_a_pending_start() {
        let registry = Arc::new(Registry::new());
        let slow: Spawn = Arc::new(|spec: SessionSpec| {
            Box::pin(async move {
                tokio::time::sleep(Duration::from_millis(50)).await;
                let fake = Fake::new(&spec.id);
                Ok(Started {
                    log: fake.log.clone(),
                    session: fake as Arc<dyn Session>,
                })
            })
        });
        let starting = {
            let registry = registry.clone();
            let slow = slow.clone();
            tokio::spawn(async move {
                let mut spec = SessionSpec::new(vec!["true".to_owned()]);
                spec.id = "slow".to_owned();
                registry.start(spec, &slow, &limits()).await
            })
        };
        tokio::time::sleep(Duration::from_millis(10)).await;
        // The close arrives while the start is pending: it must wait, then
        // release, rather than fail with "not found".
        registry.close_session("slow").await.expect("close");
        assert!(starting.await.expect("join").is_ok());
        assert!(registry.list().expect("list").is_empty());
    }

    #[tokio::test]
    async fn close_session_forgets_the_session() {
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let limits = limits();
        let session = start(&registry, &spawn, &limits, "closing")
            .await
            .expect("start");
        assert!(registry.get(session.id()).is_ok());
        registry.close_session(session.id()).await.expect("close");
        assert_eq!(
            registry.get(session.id()).expect_err("gone").kind(),
            ErrorKind::NotFound
        );
        assert!(registry.list().expect("list").is_empty());
    }

    /// The failure this bound prevents: a client that starts sessions and never
    /// closes them grows the table until the process dies.
    #[tokio::test]
    async fn exited_sessions_make_room_up_to_the_session_count() {
        let limits = SessionLimits {
            max_sessions: 4,
            max_total_log_bytes: 0,
            ..SessionLimits::default()
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        for index in 0..40 {
            start(&registry, &spawn, &limits, &format!("s{index}"))
                .await
                .expect("an exited session must not block a start");
            sessions
                .lock()
                .expect("lock")
                .last()
                .expect("spawned")
                .finish();
            assert!(registry.list().expect("list").len() <= limits.max_sessions);
        }
    }

    /// The eviction order: a session whose output was read to the end goes before
    /// one that has been finished longer, because nothing is owed to its reader.
    #[tokio::test]
    async fn a_consumed_session_is_evicted_before_an_unread_one() {
        let limits = SessionLimits {
            max_sessions: 2,
            max_total_log_bytes: 0,
            ..SessionLimits::default()
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let now = Instant::now();

        let older = start(&registry, &spawn, &limits, "older")
            .await
            .expect("start");
        let newer = start(&registry, &spawn, &limits, "newer")
            .await
            .expect("start");
        {
            let sessions = sessions.lock().expect("lock");
            // `older` exits first and is never read; `newer` exits later but its
            // output is consumed, so it is the one nothing is owed.
            sessions[0].write("tail");
            sessions[0].finish();
            // `newer` is the one whose output is read to the end below, so its
            // log has to end too — that is what makes `read` deliver `eof`.
            sessions[1].finish_with_output();
        }
        registry.sweep(now, &limits);
        // The handle is cloned out of the lock: holding a `std::sync` guard across
        // an `await` is how a test deadlocks the whole binary.
        let consumed = sessions.lock().expect("lock")[1].clone();
        consumed.read_to_end().await;
        assert!(registry.holds(older.id()) && registry.holds(newer.id()));

        start(&registry, &spawn, &limits, "third")
            .await
            .expect("room after eviction");
        assert!(
            !registry.holds(newer.id()),
            "the consumed session goes first"
        );
        assert!(
            registry.holds(older.id()),
            "the unread one keeps its window"
        );
    }

    /// The hard rule: work in progress is never the price of admitting new work.
    #[tokio::test]
    async fn a_running_session_is_never_evicted() {
        let limits = SessionLimits {
            max_sessions: 1,
            max_total_log_bytes: 0,
            ..SessionLimits::default()
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let running = start(&registry, &spawn, &limits, "build")
            .await
            .expect("start");
        let error = start(&registry, &spawn, &limits, "next")
            .await
            .expect_err("the table is full of live work");
        assert_eq!(error.kind(), ErrorKind::BudgetExceeded);
        assert!(error.to_string().contains("max_sessions"), "{error}");
        assert_eq!(listed(&registry), vec![running.id().to_owned()]);
        assert!(running.exit().is_none(), "the running session is untouched");
    }

    /// The second gate. 64 sessions each inside their own 10 MiB budget still add
    /// up, so the table has a byte budget of its own — and it refuses rather than
    /// truncating, because a truncated log leaves a sequence gap nothing explains.
    #[tokio::test]
    async fn the_output_budget_refuses_a_start_rather_than_truncating_a_live_session() {
        let limits = SessionLimits {
            max_sessions: 0,
            max_total_log_bytes: 8,
            ..SessionLimits::default()
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let noisy = start(&registry, &spawn, &limits, "noisy")
            .await
            .expect("start");
        let fake = sessions.lock().expect("lock")[0].clone();
        fake.write("0123456789");
        let fake = sessions.lock().expect("lock")[0].clone();
        fake.write("0123456789");

        let error = start(&registry, &spawn, &limits, "second")
            .await
            .expect_err("the budget is spent");
        assert_eq!(error.kind(), ErrorKind::BudgetExceeded);
        assert!(error.to_string().contains("max_total_log_bytes"), "{error}");
        // The live session's output is intact: it was not the one made to fit.
        let read = registry
            .get(noisy.id())
            .expect("listed")
            .read(0, 4096)
            .await;
        assert!(read.is_ok(), "the live session's output is still readable");
    }

    /// The whole lifecycle: listed while it runs, retained for its window, then
    /// gone — and the exit status stays readable for exactly that long.
    #[tokio::test]
    async fn an_exited_session_is_retained_for_its_window_and_then_reclaimed() {
        let limits = SessionLimits {
            max_sessions: 4,
            exit_retention: Duration::from_secs(300),
            max_total_log_bytes: 0,
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let session = start(&registry, &spawn, &limits, "done")
            .await
            .expect("start");
        sessions.lock().expect("lock")[0].finish();

        let exited_at = Instant::now();
        registry.sweep(exited_at, &limits);
        let infos = registry.list().expect("list");
        assert_eq!(infos.len(), 1);
        assert!(!infos[0].running);
        assert_eq!(infos[0].exit.as_ref().map(|exit| exit.code), Some(Some(0)));

        registry.sweep(exited_at + Duration::from_secs(299), &limits);
        assert!(registry.holds(session.id()), "still inside the window");

        registry.sweep(exited_at + Duration::from_secs(300), &limits);
        assert!(!registry.holds(session.id()), "the window has ended");
    }

    /// `0` seconds is a legitimate deployment choice: keep nothing after the exit.
    #[tokio::test]
    async fn a_zero_window_reclaims_the_moment_the_exit_is_seen() {
        let limits = SessionLimits {
            exit_retention: Duration::ZERO,
            ..SessionLimits::default()
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let session = start(&registry, &spawn, &limits, "gone")
            .await
            .expect("start");
        sessions.lock().expect("lock")[0].finish();
        registry.sweep(Instant::now(), &limits);
        assert!(!registry.holds(session.id()));
    }

    /// A client that read to the end is owed nothing, so its session may go before
    /// the window ends — that is what keeps a busy runner from accumulating full
    /// retention windows.
    #[tokio::test]
    async fn a_consumed_session_is_reclaimed_before_its_window_ends() {
        let limits = SessionLimits {
            exit_retention: Duration::from_secs(300),
            ..SessionLimits::default()
        };
        let registry = Registry::new();
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let session = start(&registry, &spawn, &limits, "read")
            .await
            .expect("start");
        let fake = sessions.lock().expect("lock")[0].clone();
        fake.write("all of it");
        fake.finish_with_output();
        fake.read_to_end().await;
        // The exit still has to be observed once, then the entry may go.
        registry.sweep(Instant::now(), &limits);
        assert!(!registry.holds(session.id()));
    }

    /// A reader blocked on a reclaimed session learns why it ended, instead of
    /// waiting forever on a session that is not there anymore.
    #[tokio::test]
    async fn reclaiming_a_session_releases_a_blocked_reader() {
        let limits = SessionLimits {
            exit_retention: Duration::ZERO,
            ..SessionLimits::default()
        };
        let registry = Arc::new(Registry::new());
        let sessions = Arc::new(Mutex::new(Vec::new()));
        let spawn = spawner(&sessions);
        let session = start(&registry, &spawn, &limits, "quiet")
            .await
            .expect("start");
        let reader = {
            let session = session.clone();
            tokio::spawn(async move {
                session
                    .read(0, 64)
                    .await
                    .err()
                    .map(|error| error.to_string())
            })
        };
        tokio::time::sleep(Duration::from_millis(20)).await;
        sessions.lock().expect("lock")[0].finish();
        registry.sweep(Instant::now(), &limits);
        let message = reader
            .await
            .expect("join")
            .expect("a reclaimed session is closed");
        assert!(message.contains("reclaimed"), "{message}");
        assert_eq!(
            registry.get(session.id()).expect_err("gone").kind(),
            ErrorKind::NotFound
        );
    }
}
