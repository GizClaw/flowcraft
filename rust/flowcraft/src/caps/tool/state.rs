//! Per-session injection state: required names and the discovery pool.

use std::collections::{BTreeMap, BTreeSet};

/// One discovery-pool entry.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DiscoveredEntry {
    /// Turn of the latest use or discovery refresh.
    pub last_use: u64,
    /// Monotonic sequence, breaking same-turn recency ties.
    pub seq: u64,
    /// Position inside the discovering batch (0 = best hit).
    pub rank: usize,
    /// Definition size at discovery time.
    pub bytes: i64,
}

/// A read-only view for the pure visibility computation.
#[derive(Debug, Clone, Default)]
pub struct Snapshot {
    /// Names required by the host for this round.
    pub required: BTreeSet<String>,
    /// Discovery pool entries.
    pub discovered: BTreeMap<String, DiscoveredEntry>,
    /// Current turn.
    pub turn: u64,
}

impl Snapshot {
    /// Whether the host required this tool by name.
    #[must_use]
    pub fn is_required(&self, name: &str) -> bool {
        self.required.contains(name)
    }

    /// Whether the tool sits in the discovery pool.
    #[must_use]
    pub fn is_discovered(&self, name: &str) -> bool {
        self.discovered.contains_key(name)
    }
}

/// The mutable session state behind a [`Snapshot`].
#[derive(Debug, Clone, Default)]
pub struct SessionState {
    required: BTreeSet<String>,
    discovered: BTreeMap<String, DiscoveredEntry>,
    turn: u64,
    seq: u64,
}

impl SessionState {
    /// A snapshot for one visibility computation.
    #[must_use]
    pub fn snapshot(&self) -> Snapshot {
        Snapshot {
            required: self.required.clone(),
            discovered: self.discovered.clone(),
            turn: self.turn,
        }
    }

    /// Mark tools required by name. Idempotent.
    pub fn require(&mut self, names: &[&str]) {
        for name in names {
            self.required.insert((*name).to_owned());
        }
    }

    /// Insert or refresh one discovery entry as most recently used.
    pub fn touch(&mut self, name: &str, bytes: i64, rank: usize) {
        self.seq += 1;
        self.discovered.insert(
            name.to_owned(),
            DiscoveredEntry {
                last_use: self.turn,
                seq: self.seq,
                rank,
                bytes,
            },
        );
    }

    /// Drop one discovery entry.
    pub fn remove_discovered(&mut self, name: &str) {
        self.discovered.remove(name);
    }

    /// Number of pool entries and their total recorded size.
    #[must_use]
    pub fn discovered_totals(&self) -> (usize, i64) {
        (
            self.discovered.len(),
            self.discovered.values().map(|entry| entry.bytes).sum(),
        )
    }

    /// The current turn.
    #[must_use]
    pub fn turn(&self) -> u64 {
        self.turn
    }

    /// Whether a tool is in the pool.
    #[must_use]
    pub fn is_discovered(&self, name: &str) -> bool {
        self.discovered.contains_key(name)
    }

    /// Advance one round and drop entries that went idle or left the catalog.
    /// Returns the removed names.
    pub fn advance_turn(
        &mut self,
        idle_rounds: usize,
        alive: impl Fn(&str) -> bool,
    ) -> Vec<String> {
        self.turn += 1;
        let turn = self.turn;
        let stale: Vec<String> = self
            .discovered
            .iter()
            .filter(|(name, entry)| {
                let idle =
                    idle_rounds > 0 && turn.saturating_sub(entry.last_use) > idle_rounds as u64;
                idle || !alive(name)
            })
            .map(|(name, _)| name.clone())
            .collect();
        for name in &stale {
            self.discovered.remove(name);
        }
        stale
    }

    /// Replace the discovery pool (used by the read-side trimming path when it
    /// has to make a visible set fit).
    pub fn retain_discovered(&mut self, keep: &BTreeMap<String, DiscoveredEntry>) {
        self.discovered = keep.clone();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn touching_refreshes_recency() {
        let mut state = SessionState::default();
        state.touch("a", 10, 0);
        state.touch("b", 10, 0);
        let first_a = state.snapshot().discovered["a"];
        state.touch("a", 12, 0);
        let second_a = state.snapshot().discovered["a"];
        assert!(second_a.seq > first_a.seq);
        assert_eq!(second_a.bytes, 12);
    }

    #[test]
    fn advancing_a_turn_drops_idle_and_dead_entries() {
        let mut state = SessionState::default();
        state.touch("idle", 1, 0);
        state.touch("gone", 1, 0);
        state.touch("live", 1, 0);
        for _ in 0..3 {
            state.advance_turn(2, |name| name != "gone");
            // Keep "live" fresh, the way a real session re-touches a tool it
            // used this round.
            state.touch("live", 1, 0);
        }
        let snapshot = state.snapshot();
        assert!(!snapshot.is_discovered("idle"));
        assert!(!snapshot.is_discovered("gone"));
        assert!(snapshot.is_discovered("live"));
    }

    #[test]
    fn required_names_are_kept_separately() {
        let mut state = SessionState::default();
        state.require(&["a", "a", "b"]);
        let snapshot = state.snapshot();
        assert!(snapshot.is_required("a"));
        assert!(snapshot.is_required("b"));
        assert!(!snapshot.is_discovered("a"));
    }
}
