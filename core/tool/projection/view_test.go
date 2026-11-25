package projection_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/projection"
)

// countingCatalog counts how often the view enumerates the catalog, so a test can tell a
// cache hit from a rebuild. It answers DefinitionsEpoch through the embedded registry,
// which is what lets the view key its snapshot in the first place.
type countingCatalog struct {
	*tool.Registry
	definitions atomic.Int64
}

func (c *countingCatalog) Definitions() []message.ToolDefinition {
	c.definitions.Add(1)
	return c.Registry.Definitions()
}

func (c *countingCatalog) definitionsCalls() int64 { return c.definitions.Load() }

// gatedCatalog holds its first Definitions call open until the test releases it, the way
// a server-backed catalog answers: the read takes real time. Later calls pass straight
// through, so the test can keep working while one build is in flight.
type gatedCatalog struct {
	*tool.Registry
	held    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newGatedCatalog(registry *tool.Registry) *gatedCatalog {
	return &gatedCatalog{
		Registry: registry,
		entered:  make(chan struct{}, 1),
		release:  make(chan struct{}),
	}
}

func (c *gatedCatalog) Definitions() []message.ToolDefinition {
	if c.held.CompareAndSwap(false, true) {
		c.entered <- struct{}{}
		<-c.release
	}
	return c.Registry.Definitions()
}

// TestSnapshotBuildRunsOutsideTheViewLock states the fix directly: while the catalog is
// still answering a Definitions call, a RecordCall from the same session completes. A
// snapshot built under View.mu held the lock for the whole catalog read, so every call a
// round records — [tool.Dispatcher.ExecuteAll] records several concurrently — waited for
// it.
func TestSnapshotBuildRunsOutsideTheViewLock(t *testing.T) {
	registry := catalogOf(t, "a", "b")
	catalog := newGatedCatalog(registry)
	p, err := projection.New(catalog, projection.Policy{
		Default:   projection.ExposureDeferred,
		Discovery: projection.DiscoveryPolicy{MaxTools: 4, MaxBytes: 1 << 20, IdleRounds: 10},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view := p.NewView()

	built := make(chan struct{})
	go func() {
		defer close(built)
		view.Definitions() // a cold cache: this call builds the snapshot
	}()
	<-catalog.entered // the build is inside the catalog, with whatever lock it took

	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		view.RecordCall(message.ToolCall{Name: "a"})
	}()
	select {
	case <-recorded:
	case <-time.After(10 * time.Second):
		close(catalog.release) // let the build finish, so the deadline fails one test
		t.Fatal("RecordCall waited for the catalog read: the snapshot is built under View.mu")
	}

	close(catalog.release)
	<-built
	// The recorded call landed in the pool, so the deferred tool is now eligible: the
	// writer did its work rather than returning early.
	if !view.Visible("a") {
		t.Fatal("the recorded call did not reach the discovery pool")
	}
}

// TestSnapshotRebuildDoesNotStarveTheSession is the concurrent shape of a live round: one
// goroutine forces epoch changes — recorded calls, advanced rounds, and the catalog
// re-lists that move the catalog's own epoch — while another loops over Definitions and
// Visible. Both must keep moving until the deadline: a rebuild that blocks a writer, or a
// publish loop that never settles, shows up as the deadline expiring instead of as a
// timing measurement.
func TestSnapshotRebuildDoesNotStarveTheSession(t *testing.T) {
	const tools = 24
	toolNames := make([]string, 0, tools)
	for i := range tools {
		toolNames = append(toolNames, fmt.Sprintf("tool_%02d", i))
	}
	registry := catalogOf(t, toolNames...)
	catalog := &countingCatalog{Registry: registry}
	p, err := projection.New(catalog, projection.Policy{
		Default:   projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{projection.ToolName: projection.ExposureAlways},
		Budget:    projection.Budget{MaxDefinitions: 8, MaxBytes: 1 << 20},
		Discovery: projection.DiscoveryPolicy{MaxTools: 8, MaxBytes: 1 << 20, IdleRounds: 3},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view := p.NewView()
	view.Discover("tool_00")
	view.Require("tool_07")

	const runFor = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	var reads, writes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if len(view.Definitions()) == 0 {
				t.Error("the required tool left the visible set")
				return
			}
			view.Visible("tool_00")
			reads.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			view.RecordCall(message.ToolCall{Name: "tool_00"})
			view.AdvanceRound()
			// A re-list: the catalog's epoch moves, so the reader cannot reuse the
			// snapshot it holds.
			churn := fmt.Sprintf("churn_%d", i)
			if err := registry.Add(tool.TextTool(messageDef(churn, "churn"), func(context.Context, string) (string, error) {
				return "", nil
			})); err != nil {
				t.Errorf("Add(%q): %v", churn, err)
				return
			}
			registry.Remove(churn)
			writes.Add(1)
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(runFor + 10*time.Second):
		t.Fatalf("the session stalled: %d definitions reads, %d epoch changes",
			reads.Load(), writes.Load())
	}
	if reads.Load() == 0 || writes.Load() == 0 {
		t.Fatalf("progress = %d reads, %d changes; want both to move", reads.Load(), writes.Load())
	}
	if catalog.definitionsCalls() == 0 {
		t.Fatal("the reader never rebuilt the snapshot, so the churn was not exercised")
	}
}

// TestDefinitionsEnumeratesTheCatalogOncePerEpoch pins the cache boundary: the snapshot
// survives every state change, and only the catalog's own definition epoch invalidates it.
func TestDefinitionsEnumeratesTheCatalogOncePerEpoch(t *testing.T) {
	registry := catalogOf(t, "a", "b", "c")
	catalog := &countingCatalog{Registry: registry}
	p, err := projection.New(catalog, projection.Policy{
		Default:   projection.ExposureDeferred,
		Exposures: map[string]projection.Exposure{"a": projection.ExposureAlways},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view := p.NewView()

	for range 5 {
		if got := names(view.Definitions()); len(got) != 1 || got[0] != "a" {
			t.Fatalf("definitions = %v, want [a]", got)
		}
	}
	if got := catalog.definitionsCalls(); got != 1 {
		t.Fatalf("Definitions enumerated the catalog %d times, want 1", got)
	}

	// The state half moves, the catalog stands still: still the one enumeration, and
	// Visible reads the same snapshot rather than building its own.
	view.Discover("b")
	view.RecordCall(message.ToolCall{Name: "b"})
	if !view.Visible("b") {
		t.Fatal("the discovered tool is not visible")
	}
	if got := catalog.definitionsCalls(); got != 1 {
		t.Fatalf("a state change re-enumerated the catalog: %d calls, want 1", got)
	}

	// A catalog change is the one thing that must rebuild it — once, not once per call.
	if err := registry.Add(tool.TextTool(messageDef("d", "does d"), func(context.Context, string) (string, error) {
		return "d", nil
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	view.Definitions()
	view.Definitions()
	if got := catalog.definitionsCalls(); got != 2 {
		t.Fatalf("a catalog change enumerated the catalog %d times, want 2", got)
	}
}
