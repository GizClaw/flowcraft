package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// flightCatalog is a catalog whose definition epoch the test can move, so a test
// can force a cache miss without building a real registry.
type flightCatalog struct {
	defs  []message.ToolDefinition
	epoch atomic.Uint64
}

func newFlightCatalog(size int) *flightCatalog {
	return &flightCatalog{defs: benchDefinitions(size)}
}

func (c *flightCatalog) Get(string) (tool.Tool, bool) { return nil, false }

func (c *flightCatalog) Definitions() []message.ToolDefinition { return c.defs }

func (c *flightCatalog) DefinitionsEpoch() uint64 { return c.epoch.Load() }

// flightResult is one search's answer, handed back over a channel so the
// concurrent goroutines in these tests own no shared state.
type flightResult struct {
	index *searchIndex
	err   error
}

// flightBuild is a builder a test can hold open: the first call signals started
// and blocks until release closes, which is the window a competing build or a
// blocked hit would show up in. build counts every call.
func flightBuild(catalog *flightCatalog, builds *atomic.Int32) (build func() []searchDoc, started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	build = func() []searchDoc {
		if builds.Add(1) == 1 {
			close(started)
			<-release
		}
		return searchDocs(catalog.Definitions())
	}
	return build, started, release
}

// TestSearchIndexCacheBuildsOnceForConcurrentSearchers: a cold cache plus N
// concurrent searches is one rebuild, and every caller answers from it. The
// build stays open while the other searches arrive, so a second leader would be
// counted rather than merely race a fast build to completion.
func TestSearchIndexCacheBuildsOnceForConcurrentSearchers(t *testing.T) {
	catalog := newFlightCatalog(8)
	cache := &searchIndexCache{}
	var builds atomic.Int32
	build, started, release := flightBuild(catalog, &builds)

	const searchers = 8
	results := make(chan flightResult, searchers)
	var wg sync.WaitGroup
	search := func() {
		defer wg.Done()
		index, err := cache.get(catalog, build)
		results <- flightResult{index: index, err: err}
	}

	wg.Add(1)
	go search()
	<-started // the leader is inside the build
	for i := 1; i < searchers; i++ {
		wg.Add(1)
		go search()
	}
	// Hold the build open long enough for the others to reach the flight; a
	// latecomer reads the published cache instead, which is a different path.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)

	if got := builds.Load(); got != 1 {
		t.Fatalf("builds = %d across %d concurrent searches, want 1", got, searchers)
	}
	var shared *searchIndex
	for result := range results {
		if result.err != nil {
			t.Fatalf("search: %v", result.err)
		}
		if shared == nil {
			shared = result.index
		}
		if result.index != shared {
			t.Fatalf("search = %p, want the flight's %p", result.index, shared)
		}
	}
	if shared == nil {
		t.Fatal("no search returned an index")
	}
}

// TestSearchIndexCacheHitDoesNotWaitOnFlight: a search whose epoch is cached
// answers from the cache even while a build is in flight, and never runs a build
// of its own. One shared cache means a moved epoch misses for everyone, so an
// open flight cannot be reached through get while the cache still describes the
// requested epoch: it is installed directly to pin the hit path against ever
// consulting it. The cost of the lock-held build is pinned separately, by
// TestSearchIndexCacheBuildsOutsideTheLock.
func TestSearchIndexCacheHitDoesNotWaitOnFlight(t *testing.T) {
	catalog := newFlightCatalog(4)
	cache := &searchIndexCache{}
	warm, err := cache.get(catalog, func() []searchDoc { return searchDocs(catalog.Definitions()) })
	if err != nil {
		t.Fatalf("warm search: %v", err)
	}
	cache.mu.Lock()
	cache.flight = &searchBuildFlight{epoch: catalog.DefinitionsEpoch(), done: make(chan struct{})}
	cache.mu.Unlock()

	var rebuilt atomic.Int32
	done := make(chan *searchIndex, 1)
	go func() {
		index, err := cache.get(catalog, func() []searchDoc {
			rebuilt.Add(1)
			return searchDocs(catalog.Definitions())
		})
		if err != nil {
			done <- nil
			return
		}
		done <- index
	}()
	select {
	case index := <-done:
		if index != warm {
			t.Fatalf("cached search = %p, want the cached %p", index, warm)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cached search waited on an in-flight build")
	}
	if got := rebuilt.Load(); got != 0 {
		t.Fatalf("a cached search rebuilt the index %d times, want 0", got)
	}
}

// TestSearchIndexCacheBuildsOutsideTheLock pins the fix itself: while a rebuild
// runs, the cache lock is free, so the build cannot stall the hit path of any
// other session.
func TestSearchIndexCacheBuildsOutsideTheLock(t *testing.T) {
	catalog := newFlightCatalog(4)
	cache := &searchIndexCache{}
	var builds atomic.Int32
	build, started, release := flightBuild(catalog, &builds)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		if _, err := cache.get(catalog, build); err != nil {
			t.Errorf("leader search: %v", err)
		}
	}()
	<-started
	acquired := cache.mu.TryLock()
	if acquired {
		cache.mu.Unlock()
	}
	close(release)
	<-leaderDone
	if !acquired {
		t.Fatal("the cache lock is held across the rebuild")
	}
}

// TestSearchIndexCacheFlightFailsEverySearcher: a build that cannot produce an
// index releases its waiters with the error instead of stranding them on done.
// Duplicate document ids are the kernel's rejected input, not a fixture.
func TestSearchIndexCacheFlightFailsEverySearcher(t *testing.T) {
	catalog := newFlightCatalog(2)
	cache := &searchIndexCache{}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	build := func() []searchDoc {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return []searchDoc{{name: "read_file"}, {name: "read_file"}}
	}

	const searchers = 4
	results := make(chan flightResult, searchers)
	var wg sync.WaitGroup
	search := func() {
		defer wg.Done()
		index, err := cache.get(catalog, build)
		results <- flightResult{index: index, err: err}
	}
	wg.Add(1)
	go search()
	<-started
	for i := 1; i < searchers; i++ {
		wg.Add(1)
		go search()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)

	failures := 0
	for result := range results {
		if result.err == nil {
			t.Fatalf("a search returned %p with no error, want the build's", result.index)
		}
		failures++
	}
	if failures != searchers {
		t.Fatalf("%d searches saw the build's error, want %d", failures, searchers)
	}
	if cache.built != nil {
		t.Fatal("a failed build populated the cache")
	}
}

// TestSearchIndexCacheConcurrentSearchersShareOneSlowBuild measures the property
// the lock-held build cost. This catalog reports no definition epoch, so nothing
// is cached between calls and only the flight keeps twenty concurrent searches
// from each paying the build: wall clock stays near one build, not twenty.
func TestSearchIndexCacheConcurrentSearchersShareOneSlowBuild(t *testing.T) {
	const (
		searchers  = 20
		buildDelay = 20 * time.Millisecond
	)
	catalog := benchCatalogOf(16)
	cache := &searchIndexCache{}
	var builds atomic.Int32
	build := func() []searchDoc {
		builds.Add(1)
		time.Sleep(buildDelay)
		return searchDocs(catalog.Definitions())
	}

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < searchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.get(catalog, build); err != nil {
				t.Errorf("search: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("%d concurrent searches over a %v build: %v wall clock, %d builds",
		searchers, buildDelay, elapsed, builds.Load())

	if ceiling := 10 * buildDelay; elapsed > ceiling {
		t.Fatalf("concurrent searches took %v, want <= %v: each search is paying its own build", elapsed, ceiling)
	}
	if got := builds.Load(); got >= searchers {
		t.Fatalf("builds = %d for %d concurrent searches, want one shared build", got, searchers)
	}
}

// TestSearchIndexCacheWaiterForNewerEpochRebuilds: a flight answers the epoch it
// was started for and nothing else. A search that missed at a later epoch takes
// its own build rather than the older flight's index, which it can prove stale.
func TestSearchIndexCacheWaiterForNewerEpochRebuilds(t *testing.T) {
	catalog := newFlightCatalog(3)
	cache := &searchIndexCache{}
	var builds atomic.Int32
	build, started, release := flightBuild(catalog, &builds)

	leader := make(chan *searchIndex, 1)
	go func() {
		index, err := cache.get(catalog, build)
		if err != nil {
			t.Errorf("leader search: %v", err)
		}
		leader <- index
	}()
	<-started
	catalog.epoch.Store(1) // the catalog moves while the first build runs

	waiter := make(chan *searchIndex, 1)
	go func() {
		index, err := cache.get(catalog, build)
		if err != nil {
			t.Errorf("waiter search: %v", err)
		}
		waiter <- index
	}()
	time.Sleep(20 * time.Millisecond) // let the waiter reach the open flight
	close(release)

	leaderIndex, waiterIndex := <-leader, <-waiter
	if got := builds.Load(); got != 2 {
		t.Fatalf("builds = %d, want 2: the newer epoch needs its own build", got)
	}
	if leaderIndex == waiterIndex {
		t.Fatalf("the newer epoch was answered by the older flight's index %p", waiterIndex)
	}
	if cache.epoch != 1 {
		t.Fatalf("cached epoch = %d, want 1", cache.epoch)
	}
}
