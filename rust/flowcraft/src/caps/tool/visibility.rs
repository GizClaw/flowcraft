//! The visibility computation: a pure function of candidates, session state,
//! and policy.

use std::collections::BTreeMap;

use crate::proto::message as msg;

use crate::caps::tool::exposure::{Budget, DiscoveryPolicy, Exposure, Policy};
use crate::caps::tool::state::{DiscoveredEntry, Snapshot};
use crate::message::definition_bytes;

/// One registry tool paired with its exposure for the computation.
#[derive(Debug, Clone)]
pub struct Candidate {
    /// Tool name.
    pub name: String,
    /// The definition the model would see.
    pub definition: msg::ToolDefinition,
    /// The exposure this tool was registered with.
    pub exposure: Exposure,
}

/// Apply the visibility algorithm and return the definitions that reach the
/// model this round, sorted by name for stable output.
///
/// The computation never mutates the session or the policy.
#[must_use]
pub fn visible_candidates(
    candidates: Vec<Candidate>,
    state: &Snapshot,
    policy: &Policy,
) -> Vec<Candidate> {
    let budget = policy.budget;
    let mut visible: Vec<Candidate> = candidates
        .into_iter()
        .filter(|candidate| include(candidate, state))
        .collect();

    // Deterministic pruning order: exposure rank, required-by-name,
    // discovery-pool recency, then name. Same-round pool entries share a
    // `last_use`, so they fall back to discovery rank.
    visible.sort_by(|a, b| less_priority(a, b, state));
    visible.truncate(budget.max_definitions);

    let mut kept = Vec::with_capacity(visible.len());
    let mut total: i64 = 0;
    for (index, candidate) in visible.into_iter().enumerate() {
        let size = definition_bytes(&candidate.definition);
        if total + size > budget.max_bytes && index > 0 {
            // Skip the entry that does not fit instead of stopping: one
            // oversized definition must not starve every smaller candidate
            // behind it. The highest-priority entry is always kept.
            continue;
        }
        total += size;
        kept.push(candidate);
    }

    kept.sort_by(|a, b| a.name.cmp(&b.name));
    kept
}

/// Restrict the snapshot's discovered set to the pool budget, measured against
/// the live definitions of the current round.
///
/// Most-recently-used entries stay; required tools live in their own set and are
/// unaffected. The read path never mutates state, so entries trimmed here are
/// evicted for real on the next mutation or turn advance.
#[must_use]
pub fn fit_discovery_pool(
    state: &Snapshot,
    sizes: &BTreeMap<String, i64>,
    pool: DiscoveryPolicy,
) -> Snapshot {
    if state.discovered.is_empty() {
        return state.clone();
    }
    let mut names: Vec<&String> = state
        .discovered
        .keys()
        .filter(|name| sizes.contains_key(*name))
        .collect();
    names.sort_by(|a, b| {
        let left = state.discovered[*a];
        let right = state.discovered[*b];
        right
            .last_use
            .cmp(&left.last_use)
            .then_with(|| right.seq.cmp(&left.seq))
            .then_with(|| a.cmp(b))
    });

    let mut total: i64 = 0;
    let mut kept: BTreeMap<String, DiscoveredEntry> = BTreeMap::new();
    for name in names {
        if pool.max_tools > 0 && kept.len() >= pool.max_tools {
            break;
        }
        let size = sizes.get(name).copied().unwrap_or(0);
        if total + size > pool.max_bytes && !kept.is_empty() {
            break;
        }
        total += size;
        kept.insert(name.clone(), state.discovered[name]);
    }
    let mut out = state.clone();
    out.discovered = kept;
    out
}

fn include(candidate: &Candidate, state: &Snapshot) -> bool {
    let required = state.is_required(&candidate.name);
    let discovered = state.is_discovered(&candidate.name);
    match candidate.exposure {
        Exposure::Always => true,
        Exposure::Direct | Exposure::Deferred => required || discovered,
        Exposure::Hidden => required,
    }
}

fn less_priority(a: &Candidate, b: &Candidate, state: &Snapshot) -> std::cmp::Ordering {
    use std::cmp::Ordering;

    match a.exposure.rank().cmp(&b.exposure.rank()) {
        Ordering::Equal => {}
        other => return other,
    }
    match state.is_required(&a.name).cmp(&state.is_required(&b.name)) {
        Ordering::Equal => {}
        // Required first.
        other => return other.reverse(),
    }
    let left = state.discovered.get(&a.name);
    let right = state.discovered.get(&b.name);
    match (left, right) {
        (Some(le), Some(re)) => {
            match re.last_use.cmp(&le.last_use) {
                Ordering::Equal => {}
                other => return other,
            }
            match le.rank.cmp(&re.rank) {
                Ordering::Equal => {}
                other => return other,
            }
            match re.seq.cmp(&le.seq) {
                Ordering::Equal => {}
                other => return other,
            }
        }
        (Some(_), None) => return Ordering::Less,
        (None, Some(_)) => return Ordering::Greater,
        (None, None) => {}
    }
    // `tool_search` is the discovery escape hatch: keep it among equals.
    if a.name != b.name
        && (a.name == crate::caps::tool::tool_search::TOOL_NAME
            || b.name == crate::caps::tool::tool_search::TOOL_NAME)
    {
        return if a.name == crate::caps::tool::tool_search::TOOL_NAME {
            Ordering::Less
        } else {
            Ordering::Greater
        };
    }
    a.name.cmp(&b.name)
}

/// The effective per-round budget, for callers that need the numbers.
#[must_use]
pub fn effective_budget(policy: &Policy) -> Budget {
    policy.budget
}

#[cfg(test)]
mod tests {
    use super::*;

    fn definition(name: &str) -> msg::ToolDefinition {
        msg::ToolDefinition {
            name: name.to_owned(),
            description: String::new(),
            input_schema: Vec::new(),
        }
    }

    fn candidate(name: &str, exposure: Exposure) -> Candidate {
        Candidate {
            name: name.to_owned(),
            definition: definition(name),
            exposure,
        }
    }

    fn policy(budget: Budget) -> Policy {
        Policy {
            default: Exposure::Deferred,
            budget,
            ..Policy::default()
        }
        .normalized()
    }

    #[test]
    fn always_is_visible_and_deferred_is_not() {
        let state = Snapshot::default();
        let visible = visible_candidates(
            vec![
                candidate("always", Exposure::Always),
                candidate("deferred", Exposure::Deferred),
            ],
            &state,
            &policy(Budget::default()),
        );
        assert_eq!(
            visible.iter().map(|c| c.name.as_str()).collect::<Vec<_>>(),
            vec!["always"]
        );
    }

    #[test]
    fn required_and_discovered_surface_direct_and_hidden() {
        let mut state = Snapshot::default();
        state.required.insert("hidden".to_owned());
        let mut entry = DiscoveredEntry {
            last_use: 0,
            seq: 1,
            rank: 0,
            bytes: 10,
        };
        state.discovered.insert("direct".to_owned(), entry);
        entry.seq = 2;
        state.discovered.insert("deferred".to_owned(), entry);

        let visible = visible_candidates(
            vec![
                candidate("hidden", Exposure::Hidden),
                candidate("direct", Exposure::Direct),
                candidate("deferred", Exposure::Deferred),
            ],
            &state,
            &policy(Budget::default()),
        );
        let names: Vec<&str> = visible.iter().map(|c| c.name.as_str()).collect();
        assert_eq!(names, vec!["deferred", "direct", "hidden"]);
    }

    #[test]
    fn the_budget_caps_definitions_and_bytes() {
        let first = policy(Budget {
            max_definitions: 2,
            max_bytes: 16 * 1024,
        });
        let visible = visible_candidates(
            vec![
                candidate("a", Exposure::Always),
                candidate("b", Exposure::Always),
                candidate("c", Exposure::Always),
            ],
            &Snapshot::default(),
            &first,
        );
        assert_eq!(visible.len(), 2);

        let second = policy(Budget {
            max_definitions: 10,
            max_bytes: 100,
        });
        let visible = visible_candidates(
            vec![
                candidate("a", Exposure::Always),
                candidate("b", Exposure::Always),
            ],
            &Snapshot::default(),
            &second,
        );
        // The first (highest-priority) is always kept; the second may not fit.
        assert!(!visible.is_empty());
    }

    #[test]
    fn tool_search_wins_ties() {
        let visible = visible_candidates(
            vec![
                candidate("a", Exposure::Always),
                candidate(crate::caps::tool::tool_search::TOOL_NAME, Exposure::Always),
            ],
            &Snapshot::default(),
            &policy(Budget {
                max_definitions: 1,
                max_bytes: 16 * 1024,
            }),
        );
        assert_eq!(visible[0].name, crate::caps::tool::tool_search::TOOL_NAME);
    }

    #[test]
    fn the_pool_fit_keeps_the_most_recent() {
        let mut state = Snapshot::default();
        for (seq, name) in ["old", "mid", "new"].into_iter().enumerate() {
            state.discovered.insert(
                name.to_owned(),
                DiscoveredEntry {
                    last_use: 0,
                    seq: seq as u64,
                    rank: 0,
                    bytes: 60,
                },
            );
        }
        let sizes = ["old", "mid", "new"]
            .iter()
            .map(|name| ((*name).to_owned(), 60))
            .collect();
        let fitted = fit_discovery_pool(
            &state,
            &sizes,
            DiscoveryPolicy {
                max_tools: 10,
                max_bytes: 130,
                idle_rounds: 10,
            },
        );
        assert_eq!(fitted.discovered.len(), 2);
        assert!(fitted.is_discovered("new"));
        assert!(fitted.is_discovered("mid"));
        assert!(!fitted.is_discovered("old"));
    }
}
