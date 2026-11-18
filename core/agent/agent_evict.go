package agent

import (
	"sort"
	"time"
)

// This file is the session table's retention policy: what gets dropped, when, and
// at what cost. It is separate from agent.go because none of it is about a turn —
// a run never calls into it, and the message path only pays one atomic load for
// the cap it enforces.
//
// Two limits, one pass: IdleTimeout drops a session that has not been used, and
// MaxSessions bounds the table. Both are cache decisions, not data loss — the
// Task lives in the store, so an evicted session can be reopened.

// sweep evicts idle sessions and then enforces the cap. It is the background
// loop's whole pass, and it costs nothing when no limit is configured.
func (a *Agent) sweep(now time.Time) []*session {
	if a.opts.IdleTimeout <= 0 && a.opts.MaxSessions <= 0 {
		return nil
	}
	evicted := a.evictIdle(now)
	return append(evicted, a.trim()...)
}

// evictIdle drops the sessions whose idle timeout has passed.
func (a *Agent) evictIdle(now time.Time) []*session {
	if a.opts.IdleTimeout <= 0 {
		return nil
	}
	var evicted []*session
	var evictedIDs []string
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for id, entry := range shard.entries {
			if entry.running || entry.opening != nil || now.Sub(entry.lastUsed) < a.opts.IdleTimeout {
				continue
			}
			if shard.remove(id) != nil {
				evicted = append(evicted, entry)
				evictedIDs = append(evictedIDs, id)
			}
		}
		shard.mu.Unlock()
	}
	a.fireEvict(evictedIDs)
	return evicted
}

// trim evicts least-recently-used idle sessions until the table is within
// MaxSessions.
//
// It is the one limit the message path enforces, because a cap is a memory
// bound and a bound checked once a minute is not a bound. Within the cap it
// costs one atomic load. Over the cap it costs one pass over the table plus a
// selection of the count-max least recently used candidates — the previous shape
// called oldestIdle in a loop, so dropping k sessions off a table of N scanned N
// entries k times, and the first version of this one collected and sorted all N
// even when a single victim was needed.
func (a *Agent) trim() []*session {
	max := int64(a.opts.MaxSessions)
	if max <= 0 || a.table.count() <= max {
		return nil
	}
	need := int(a.table.count() - max)

	/* One pass collects the count-max least recently used idle sessions.

	Each shard is read under its own lock, and the timestamp is copied under that
	lock: `lastUsed` is written by acquire and release under the same lock, so
	sorting on the live field afterwards would be a race, and a torn 24-byte
	Time would order evictions arbitrarily. */
	victims := selectIdle(a, need)
	if len(victims) == 0 {
		return nil
	}
	sort.Slice(victims, func(i, j int) bool {
		if !victims[i].lastUsed.Equal(victims[j].lastUsed) {
			return victims[i].lastUsed.Before(victims[j].lastUsed)
		}
		return victims[i].entry.id < victims[j].entry.id
	})

	evicted := make([]*session, 0, len(victims))
	evictedIDs := make([]string, 0, len(victims))
	for _, victim := range victims {
		if a.table.count() <= max {
			break
		}
		entry := victim.entry
		where := a.table.shard(entry.id)
		where.mu.Lock()
		// Re-check everything the collection pass promised: still indexed under
		// this pointer, and still idle. A run that started in between keeps its
		// session.
		if where.entries[entry.id] == entry && !entry.running && entry.opening == nil {
			where.remove(entry.id)
			evicted = append(evicted, entry)
			evictedIDs = append(evictedIDs, entry.id)
		}
		where.mu.Unlock()
	}
	a.fireEvict(evictedIDs)
	return evicted
}

// trimCandidate is one idle session with the clock reading taken under its
// shard lock.
type trimCandidate struct {
	entry    *session
	lastUsed time.Time
}

// trimCandidates is a max-heap on recency: the root is the most recently used
// candidate held, so a newly seen candidate can replace it in O(log need) when it
// is older. Keeping only the `need` least recently used sessions is what makes
// the one-session-over-the-cap case as cheap as the single scan it replaced.
//
// The heap is maintained by hand rather than through container/heap: that
// interface boxes every element, which costs one allocation per candidate
// pushed — hundreds per pass in exactly the state this function exists to bound.
type trimCandidates []trimCandidate

// greater reports whether i sorts after j in the eviction order (oldest first),
// i.e. whether i is the more recent of the two. Equal timestamps fall back to the
// id, so the order never depends on map iteration.
func (h trimCandidates) greater(i, j int) bool {
	if !h[i].lastUsed.Equal(h[j].lastUsed) {
		return h[i].lastUsed.After(h[j].lastUsed)
	}
	return h[i].entry.id > h[j].entry.id
}

func (h trimCandidates) swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *trimCandidates) push(candidate trimCandidate) {
	*h = append(*h, candidate)
	for i := len(*h) - 1; i > 0; {
		parent := (i - 1) / 2
		if !h.greater(i, parent) {
			return
		}
		h.swap(i, parent)
		i = parent
	}
}

// popMostRecent drops the root, which is the most recently used candidate held.
func (h *trimCandidates) popMostRecent() {
	s := *h
	last := len(s) - 1
	s[0] = s[last]
	*h = s[:last]
	for i := 0; ; {
		left := 2*i + 1
		if left >= len(*h) {
			return
		}
		worst := left
		if right := left + 1; right < len(*h) && h.greater(right, left) {
			worst = right
		}
		if !h.greater(worst, i) {
			return
		}
		h.swap(i, worst)
		i = worst
	}
}

// selectIdle returns at most need idle sessions, choosing the least recently
// used ones. It takes each shard lock for its own scan and no lock across shards.
func selectIdle(a *Agent, need int) []trimCandidate {
	if need <= 0 {
		return nil
	}
	kept := make(trimCandidates, 0, min(need, 64))
	for _, shard := range a.table.shards {
		shard.mu.Lock()
		for _, entry := range shard.entries {
			if entry.running || entry.opening != nil {
				continue
			}
			candidate := trimCandidate{entry: entry, lastUsed: entry.lastUsed}
			if len(kept) < need {
				kept.push(candidate)
				continue
			}
			// Full: the root is the most recently used one held. Replace it only
			// if this candidate is older, so the heap keeps the need oldest.
			if olderCandidate(candidate, kept[0]) {
				kept.popMostRecent()
				kept.push(candidate)
			}
		}
		shard.mu.Unlock()
	}
	return kept
}

// olderCandidate reports whether left is the older of the two, with the id
// tie-break that keeps the choice independent of map order.
func olderCandidate(left, right trimCandidate) bool {
	if !left.lastUsed.Equal(right.lastUsed) {
		return left.lastUsed.Before(right.lastUsed)
	}
	return left.entry.id < right.entry.id
}

// fireEvict reports every session a pass dropped. It runs outside the table
// locks: a hook is host code and may call back into the agent.
func (a *Agent) fireEvict(ids []string) {
	if a.opts.OnEvict == nil {
		return
	}
	for _, id := range ids {
		a.opts.OnEvict(id)
	}
}
