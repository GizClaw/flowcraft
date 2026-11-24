package middleware

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
	"github.com/GizClaw/flowcraft/tool/dispatch"
)

// terminalOK is a chain's innermost stage without a catalog lookup, so these
// tests observe the middleware alone rather than the executor's own Get.
func terminalOK(_ context.Context, _ tool.Binding, call message.ToolCall) message.ToolResult {
	return message.NewToolResult(call.ID, message.NewTextContent("ok"))
}

// countingCatalog records how often Get is asked for each name, so a test can
// tell a cached decision from a repeated lookup.
type countingCatalog struct {
	inner tool.Catalog
	mu    sync.Mutex
	gets  map[string]int
}

func newCountingCatalog(inner tool.Catalog) *countingCatalog {
	return &countingCatalog{inner: inner, gets: make(map[string]int)}
}

func (c *countingCatalog) Get(name string) (tool.Tool, bool) {
	c.mu.Lock()
	c.gets[name]++
	c.mu.Unlock()
	return c.inner.Get(name)
}

func (c *countingCatalog) Definitions() []message.ToolDefinition { return c.inner.Definitions() }

func (c *countingCatalog) getCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets[name]
}

// blockedCatalog holds Get for one name until the test lets it through, so a
// test can ask whether a lookup for another tool waits behind it.
type blockedCatalog struct {
	inner   tool.Catalog
	name    string
	entered chan struct{} // closed once Get(name) is in flight
	release chan struct{} // closed by the test to let Get(name) return
}

func (c *blockedCatalog) Get(name string) (tool.Tool, bool) {
	if name == c.name {
		close(c.entered)
		<-c.release
	}
	return c.inner.Get(name)
}

func (c *blockedCatalog) Definitions() []message.ToolDefinition { return c.inner.Definitions() }

// TestRateLimitCache_ResolvesEachToolOnce pins the memo: a paced tool and an
// unpaced one are each looked up a single time, however many calls follow. The
// nil entry for the unpaced tool has to be cached too, or the common case would
// pay a catalog lookup on every call.
func TestRateLimitCache_ResolvesEachToolOnce(t *testing.T) {
	registry := catalogWith(
		ratedTool{def: message.ToolDefinition{Name: "paced"}, rate: 1000},
		ratedTool{def: message.ToolDefinition{Name: "unpaced"}},
	)
	catalog := newCountingCatalog(registry)
	run := dispatchThrough([]tool.Middleware{RateLimit(catalog)}, terminalOK)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		for _, name := range []string{"paced", "unpaced"} {
			if res := run(ctx, tooltest.Binding(), call(name)); res.IsError {
				t.Fatalf("%s call %d: %s", name, i, res.Content.Text())
			}
		}
	}
	for _, name := range []string{"paced", "unpaced"} {
		if got := catalog.getCount(name); got != 1 {
			t.Errorf("catalog.Get(%q) called %d times, want 1", name, got)
		}
	}
}

// TestRateLimit_BlockedLookupDoesNotStallOtherTools is the reason the lookup
// happens outside the cache's lock: catalog.Get takes the registry lock (and
// for a lazy tool the proxy's), so a slow one must not hold up a call to any
// other tool.
func TestRateLimit_BlockedLookupDoesNotStallOtherTools(t *testing.T) {
	registry := catalogWith(
		ratedTool{def: message.ToolDefinition{Name: "blocked"}, rate: 1},
		ratedTool{def: message.ToolDefinition{Name: "free"}},
	)
	catalog := &blockedCatalog{
		inner:   registry,
		name:    "blocked",
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	run := dispatchThrough([]tool.Middleware{RateLimit(catalog)}, terminalOK)
	ctx := context.Background()

	holding := make(chan message.ToolResult, 1)
	go func() { holding <- run(ctx, tooltest.Binding(), call("blocked")) }()
	<-catalog.entered // "blocked" is now inside catalog.Get

	resolved := make(chan message.ToolResult, 1)
	go func() { resolved <- run(ctx, tooltest.Binding(), call("free")) }()
	select {
	case res := <-resolved:
		if res.IsError {
			t.Fatalf("call to the other tool failed: %s", res.Content.Text())
		}
	case <-time.After(5 * time.Second):
		// Unblock the lookup first, or the failure would hang the suite.
		close(catalog.release)
		t.Fatal("a call to another tool waited on an unrelated tool's catalog lookup")
	}
	close(catalog.release)
	<-holding
}

// TestRateLimit_PerToolPacersDoNotQueue pins that pacing is per tool: one tool's
// exhausted slot does not delay a call to another. They share the cache, so a
// single pacer behind it would show up here as a deadline that fires.
func TestRateLimit_PerToolPacersDoNotQueue(t *testing.T) {
	registry := catalogWith(
		ratedTool{def: message.ToolDefinition{Name: "alpha"}, rate: 1}, // one call per second
		ratedTool{def: message.ToolDefinition{Name: "beta"}, rate: 1},
	)
	run := dispatchThrough([]tool.Middleware{RateLimit(registry)}, terminalOK)

	if res := run(context.Background(), tooltest.Binding(), call("alpha")); res.IsError {
		t.Fatalf("alpha: %s", res.Content.Text())
	}
	// alpha's schedule is now a second out; beta has its own.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if res := run(ctx, tooltest.Binding(), call("beta")); res.IsError {
		t.Fatalf("beta queued behind alpha's pacer: %s", res.Content.Text())
	}
}

// TestToolMemo_ConcurrentMissesAgree pins the half of the copy-on-write cache
// that matters for correctness: callers racing on one name all get the value
// that was stored, so a tool never ends up paced by two schedulers.
func TestToolMemo_ConcurrentMissesAgree(t *testing.T) {
	var resolved atomic.Int32
	memo := newToolMemo(func(string) *pacer {
		resolved.Add(1)
		return &pacer{interval: time.Second}
	})

	const callers = 8
	results := make(chan *pacer, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- memo.get("api")
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var first *pacer
	for p := range results {
		if first == nil {
			first = p
			continue
		}
		if p != first {
			t.Fatalf("one name resolved to two pacers: %p and %p", first, p)
		}
	}
	if first == nil {
		t.Fatal("no caller got a pacer")
	}
	before := resolved.Load()
	memo.get("api")
	if after := resolved.Load(); after != before {
		t.Errorf("a cached name was resolved again: %d -> %d", before, after)
	}
}

// TestTimeoutCache_CachesExemptionDecision pins that both answers are memoised.
// A deadline no self-timing tool could meet proves the first call read the right
// answer; the repeat calls prove neither answer is re-derived.
func TestTimeoutCache_CachesExemptionDecision(t *testing.T) {
	registry := catalogWith(
		selfTimingTool{name: "self", selfTimeout: true, sleep: 20 * time.Millisecond},
		selfTimingTool{name: "wrapped", selfTimeout: false, sleep: 20 * time.Millisecond},
	)
	catalog := newCountingCatalog(registry)
	exec := dispatch.New(registry, TimeoutWithCatalog(catalog, time.Millisecond, nil))

	for i := 0; i < 2; i++ {
		if res := exec.Execute(context.Background(), tooltest.Binding(), call("self")); res.IsError {
			t.Fatalf("self call %d was cut short: %s", i, res.Content.Text())
		}
	}
	if got := catalog.getCount("self"); got != 1 {
		t.Errorf("catalog.Get(%q) called %d times, want 1", "self", got)
	}

	// The cached answer for a tool with no claim has to stay "not exempt": were
	// the entry lost or misread, these calls would run to completion instead.
	for i := 0; i < 2; i++ {
		if res := exec.Execute(context.Background(), tooltest.Binding(), call("wrapped")); !res.IsError {
			t.Fatalf("wrapped call %d should have timed out, got %q", i, res.Content.Text())
		}
	}
	if got := catalog.getCount("wrapped"); got != 1 {
		t.Errorf("catalog.Get(%q) called %d times, want 1", "wrapped", got)
	}
}

// TestPacer_CancelledWaiterReturnsItsSlot is the slow-motion stall: a caller
// that gives up while waiting must hand its reservation back, or every later
// call starts one interval further out. The interval is an hour, so the schedule
// itself, not elapsed time, is the assertion.
func TestPacer_CancelledWaiterReturnsItsSlot(t *testing.T) {
	p := &pacer{interval: time.Hour}
	if err := p.wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	p.mu.Lock()
	reserved := p.next // the slot the next caller is offered
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v, want context.Canceled", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.next.Equal(reserved) {
		t.Fatalf("p.next = %v after a cancelled wait, want %v: the burned slot was not returned",
			p.next, reserved)
	}
}

// TestPacer_ReleaseKeepsALaterReservation pins the guard on the rollback: once
// another caller has queued behind the withdrawn one, giving the slot back would
// hand it to a caller that arrived later, out of order.
func TestPacer_ReleaseKeepsALaterReservation(t *testing.T) {
	p := &pacer{interval: time.Hour}
	p.mu.Lock()
	withdrawn := time.Now().Add(time.Hour) // the slot the cancelling caller held
	// A later caller took the next slot and pushed the tail past it, so the
	// reservation is no longer the last one made.
	queued := withdrawn.Add(2 * p.interval)
	p.next = queued
	p.mu.Unlock()

	p.release(withdrawn)

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.next.Equal(queued) {
		t.Fatalf("release moved p.next to %v, want %v: a later caller's reservation was reordered",
			p.next, queued)
	}
}
