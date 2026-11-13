//go:build unix

package sessionkit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The sampler is shared, so its behaviour is about counting: how many host passes
// one tick costs, and which watchers they were applied to.

// samplerHarness replaces the host sample and the kill, and silences the automatic
// ticker so the test drives every pass itself.
type samplerHarness struct {
	mu       sync.Mutex
	samples  int
	kills    []int
	usage    map[int]groupUsage
	sampleEr error
}

func newSamplerHarness(t *testing.T) *samplerHarness {
	t.Helper()
	h := &samplerHarness{usage: make(map[int]groupUsage)}

	prevInterval := groupWatchInterval
	groupWatchInterval = time.Hour
	prevSample := sampleHostFn
	prevKill := killGroupFn
	t.Cleanup(func() {
		groupWatchInterval = prevInterval
		sampleHostFn = prevSample
		killGroupFn = prevKill
	})

	sampleHostFn = func() (map[int]groupUsage, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.samples++
		if h.sampleEr != nil {
			return nil, h.sampleEr
		}
		out := make(map[int]groupUsage, len(h.usage))
		for pgid, u := range h.usage {
			out[pgid] = u
		}
		return out, nil
	}
	killGroupFn = func(pgid int) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.kills = append(h.kills, pgid)
		return nil
	}
	return h
}

func (h *samplerHarness) counts() (samples int, kills []int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.samples, append([]int(nil), h.kills...)
}

// Ten sessions cost one host pass per tick, not ten: the pass is the expensive
// half and the answer is the same for every group.
func TestHostSamplerSharesOnePassPerTick(t *testing.T) {
	h := newSamplerHarness(t)
	h.usage = map[int]groupUsage{
		11: {rssKB: 2 << 10}, // 2 MiB, over the cap below
		12: {rssKB: 2 << 10},
		13: {rssKB: 2 << 10},
	}

	limits := sandbox.ResourceLimits{MemoryBytes: 1 << 20}
	watchers := []*GroupCapsWatcher{
		StartGroupCapsWatcher(context.Background(), 11, limits, 0),
		StartGroupCapsWatcher(context.Background(), 12, limits, 0),
		StartGroupCapsWatcher(context.Background(), 13, limits, 0),
	}
	for i, w := range watchers {
		if w == nil {
			t.Fatalf("watcher %d was not started", i)
		}
		defer w.Stop()
	}

	sharedSampler.tick()

	samples, kills := h.counts()
	if samples != 1 {
		t.Fatalf("one tick sampled the host %d times, want 1", samples)
	}
	if len(kills) != len(watchers) {
		t.Fatalf("killed %v, want all three groups", kills)
	}
	for i, w := range watchers {
		if got := w.Exceeded(); got != "memory" {
			t.Errorf("watcher %d: Exceeded = %q, want memory", i, got)
		}
		if err := w.Unenforceable(); err != nil {
			t.Errorf("watcher %d: Unenforceable = %v", i, err)
		}
	}
}

// A watcher under its cap is left alone, and the sampler keeps serving the others.
func TestHostSamplerLeavesGroupsUnderTheCap(t *testing.T) {
	h := newSamplerHarness(t)
	h.usage = map[int]groupUsage{21: {rssKB: 16}, 22: {rssKB: 2 << 10}}

	under := StartGroupCapsWatcher(context.Background(), 21, sandbox.ResourceLimits{MemoryBytes: 1 << 20}, 0)
	over := StartGroupCapsWatcher(context.Background(), 22, sandbox.ResourceLimits{MemoryBytes: 1 << 20}, 0)
	defer under.Stop()
	defer over.Stop()

	sharedSampler.tick()

	_, kills := h.counts()
	if len(kills) != 1 || kills[0] != 22 {
		t.Fatalf("kills = %v, want only group 22", kills)
	}
	if got := under.Exceeded(); got != "" {
		t.Errorf("a group under its cap was marked %q", got)
	}
}

// Sampling that stays broken is fail-closed: the group is killed and the watcher
// reports why, because a cap nobody can measure is a cap nobody enforces.
func TestHostSamplerFailsClosedAfterPersistentErrors(t *testing.T) {
	h := newSamplerHarness(t)
	h.sampleEr = errors.New("ps vanished")

	w := StartGroupCapsWatcher(context.Background(), 31, sandbox.ResourceLimits{MemoryBytes: 1 << 20}, 0)
	defer w.Stop()

	for range maxSampleFailures - 1 {
		sharedSampler.tick()
		if err := w.Unenforceable(); err != nil {
			t.Fatalf("gave up after fewer than %d failures: %v", maxSampleFailures, err)
		}
	}
	sharedSampler.tick()

	if err := w.Unenforceable(); err == nil {
		t.Fatalf("still guarding after %d consecutive sampling failures", maxSampleFailures)
	}
	if _, kills := h.counts(); len(kills) != 1 || kills[0] != 31 {
		t.Fatalf("kills = %v, want group 31 once", kills)
	}
	if got := w.Exceeded(); got != "" {
		t.Errorf("Exceeded = %q; a sampling failure is not an exceeded cap", got)
	}
}

// The sampler runs only while something is watching: an idle process pays for no
// ps invocation at all.
func TestHostSamplerRunsOnlyWhileWatched(t *testing.T) {
	newSamplerHarness(t)

	if running := samplerRunning(); running {
		t.Fatal("the sampler was already running before any watcher")
	}
	w := StartGroupCapsWatcher(context.Background(), 41, sandbox.ResourceLimits{MemoryBytes: 1 << 20}, 0)
	if !samplerRunning() {
		t.Fatal("the sampler did not start for a watcher")
	}
	w.Stop()
	if samplerRunning() {
		t.Fatal("the sampler kept running after its last watcher stopped")
	}
	// Stopping twice is safe, and a watcher that never had caps is nil.
	w.Stop()
	if StartGroupCapsWatcher(context.Background(), 42, sandbox.ResourceLimits{}, time.Second) != nil {
		t.Fatal("a watcher was created for limits with no caps")
	}
}

func samplerRunning() bool {
	sharedSampler.mu.Lock()
	defer sharedSampler.mu.Unlock()
	return sharedSampler.running
}

// A quiet host is sampled less often, and the first jump snaps the cadence back.
// The rule is a pure function so it can be pinned without a clock.
func TestWatchIntervalBacksOffWhenIdle(t *testing.T) {
	for idle := range groupWatchIdleTicks {
		if got := watchIntervalFor(idle); got != groupWatchInterval {
			t.Errorf("idle %d: interval = %v, want the base cadence %v", idle, got, groupWatchInterval)
		}
	}
	for _, idle := range []int{groupWatchIdleTicks, groupWatchIdleTicks + 1, 100} {
		if got := watchIntervalFor(idle); got != groupWatchIdleInterval {
			t.Errorf("idle %d: interval = %v, want the idle cadence %v", idle, got, groupWatchIdleInterval)
		}
	}
	if groupWatchIdleInterval <= groupWatchInterval {
		t.Fatalf("idle cadence %v is not slower than the base %v", groupWatchIdleInterval, groupWatchInterval)
	}
}

// tick reports movement only for the groups it watches, which is what decides
// whether the sampler slows down.
func TestHostSamplerNoticesMovementPerGroup(t *testing.T) {
	h := newSamplerHarness(t)
	h.usage = map[int]groupUsage{51: {rssKB: 100}, 52: {rssKB: 200}}

	first := StartGroupCapsWatcher(context.Background(), 51, sandbox.ResourceLimits{MemoryBytes: 1 << 30}, 0)
	defer first.Stop()
	second := StartGroupCapsWatcher(context.Background(), 52, sandbox.ResourceLimits{MemoryBytes: 1 << 30}, 0)
	defer second.Stop()

	if !sharedSampler.tick() {
		t.Fatal("the first sample reported no movement")
	}
	if sharedSampler.tick() {
		t.Fatal("an unchanged sample reported movement")
	}

	// One group grows while the other is untouched: still movement.
	h.mu.Lock()
	h.usage[52] = groupUsage{rssKB: 900}
	h.mu.Unlock()
	if !sharedSampler.tick() {
		t.Fatal("a growing group reported no movement")
	}

	// An unwatched group moving does not count.
	h.mu.Lock()
	h.usage[99] = groupUsage{rssKB: 1 << 20}
	h.mu.Unlock()
	if sharedSampler.tick() {
		t.Fatal("a group nobody watches kept the sampler busy")
	}
}
