package fleet

import (
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
)

// runRegistry caches the terminal state of every Submit so the
// HTTP /runs/{id} endpoint can answer "what happened to that
// fire-and-forget submission?". Without this the fleet's
// handle-discarding goroutine consumed the only Wait slot and the
// HTTP caller had nothing to query — submit was effectively a
// black hole.
//
// The registry stores entries indefinitely until either:
//   - the configured retention window elapses (sweepLoop GC), or
//   - the daemon is restarted (in-memory only — explicitly NOT
//     persisted; a vessel that needs durable run history should
//     consume bus envelopes instead).
type runRegistry struct {
	mu        sync.RWMutex
	runs      map[string]*runEntry
	retention time.Duration
}

// runEntry is one tracked run. status / result / err are set when
// the underlying Handle terminates; CompletedAt is non-zero once
// completion has been observed.
type runEntry struct {
	RunID       string
	VesselName  string
	AgentName   string
	StartedAt   time.Time
	CompletedAt time.Time
	Status      agent.Status
	Messages    []model.Message
	Err         error
}

// newRunRegistry creates an empty registry. retention=0 disables
// the sweep goroutine — every entry then lives until the process
// exits. Production deployments should pass a finite retention
// (e.g. 1h) so the map cannot grow unbounded.
func newRunRegistry(retention time.Duration) *runRegistry {
	return &runRegistry{
		runs:      map[string]*runEntry{},
		retention: retention,
	}
}

// track records a new run and registers a Handle.OnTerminate
// hook to capture the terminal state. The hook fires
// synchronously inside the captain's dispatch goroutine BEFORE
// h.Done is closed and BEFORE any Wait caller observes the
// result — which is the contract /metrics and /v1/runs scrape
// endpoints rely on (a /call HTTP response cannot return before
// this entry is recorded).
//
// Previously this method spawned a private goroutine watching
// <-h.Done() and racing the HTTP handler's own Wait, leaving
// scrape consumers occasionally observing "completed run, no
// terminal counter". OnTerminate eliminates that race by design.
func (r *runRegistry) track(vesselName string, h *vessel.Handle) {
	if r == nil || h == nil {
		return
	}
	entry := &runEntry{
		RunID:      h.RunID,
		VesselName: vesselName,
		AgentName:  h.AgentName,
		StartedAt:  time.Now(),
	}
	r.mu.Lock()
	r.runs[h.RunID] = entry
	r.mu.Unlock()

	h.OnTerminate(func(res *agent.Result, err error) {
		r.mu.Lock()
		entry.CompletedAt = time.Now()
		if res != nil {
			entry.Status = res.Status
			entry.Messages = res.Messages
		}
		entry.Err = err
		r.mu.Unlock()
	})
}

// lookup returns a snapshot of the entry. Always returns a copy of
// the slice so callers can't accidentally mutate the registry.
func (r *runRegistry) lookup(runID string) (*runEntry, error) {
	if r == nil {
		return nil, errdefs.NotFoundf("vesseld fleet: run registry not initialised")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.runs[runID]
	if !ok {
		return nil, errdefs.NotFoundf("vesseld fleet: no such run %q", runID)
	}
	out := *e
	if e.Messages != nil {
		out.Messages = append([]model.Message(nil), e.Messages...)
	}
	return &out, nil
}

// list returns a snapshot of every entry, newest StartedAt first.
// Filters on vessel name and terminal state are applied before
// pagination so callers cannot read entries outside their filter.
// Pagination is keyset on (StartedAt, RunID) — startedBefore is
// the exclusive upper bound, runIDAfter is the tiebreaker for the
// equal-StartedAt case (so two runs with the same nanosecond don't
// duplicate or skip).
func (r *runRegistry) list(opts runListOptions) []runEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	all := make([]runEntry, 0, len(r.runs))
	for _, e := range r.runs {
		if opts.vessel != "" && e.VesselName != opts.vessel {
			continue
		}
		if opts.state != "" {
			if !matchesStateFilter(e, opts.state) {
				continue
			}
		}
		all = append(all, *e)
	}
	r.mu.RUnlock()

	// Newest first; ties break on RunID ascending so the keyset
	// cursor is unambiguous.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].StartedAt.Equal(all[j].StartedAt) {
			return all[i].StartedAt.After(all[j].StartedAt)
		}
		return all[i].RunID < all[j].RunID
	})

	if !opts.startedBefore.IsZero() {
		out := all[:0]
		for _, e := range all {
			if e.StartedAt.After(opts.startedBefore) {
				continue
			}
			if e.StartedAt.Equal(opts.startedBefore) && e.RunID <= opts.runIDAfter {
				continue
			}
			out = append(out, e)
		}
		all = out
	}

	if opts.pageSize > 0 && len(all) > opts.pageSize {
		all = all[:opts.pageSize]
	}
	return all
}

// runListOptions controls runRegistry.list. State filter compares
// against the same string LookupRun reports, so callers pass
// "completed" / "error" / "interrupted" / "cancelled" / "running".
type runListOptions struct {
	vessel        string
	state         string
	pageSize      int
	startedBefore time.Time
	runIDAfter    string
}

// matchesStateFilter applies the same state-derivation logic as
// LookupRun so the API layer can accept human-readable state names.
func matchesStateFilter(e *runEntry, want string) bool {
	if e.CompletedAt.IsZero() {
		return want == "running"
	}
	if e.Err != nil {
		return want == "error"
	}
	if e.Status != "" {
		return want == string(e.Status)
	}
	return want == "completed"
}

// sweep drops entries older than retention. Safe to call when
// retention<=0 (it just no-ops). Intended to run on a ticker.
func (r *runRegistry) sweep(now time.Time) {
	if r == nil || r.retention <= 0 {
		return
	}
	cutoff := now.Add(-r.retention)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.runs {
		if e.CompletedAt.IsZero() {
			continue // never drop in-flight runs
		}
		if e.CompletedAt.Before(cutoff) {
			delete(r.runs, id)
		}
	}
}

