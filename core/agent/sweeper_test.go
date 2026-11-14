package agent

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
)

// probeRunner finishes as soon as it starts, so a test measures the table rather
// than an engine.
type probeRunner struct{}

func (r *probeRunner) State() TaskState { return StateCompleted }

func (r *probeRunner) Run(context.Context, message.Message) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if !yield(Status{State: StateWorking}, nil) {
			return
		}
		yield(Status{State: StateCompleted}, nil)
	}
}

func (r *probeRunner) Cancel(context.Context, string) error { return nil }
func (r *probeRunner) Close() error                         { return nil }

func probeAgent(t *testing.T, opts AgentOptions) *Agent {
	t.Helper()
	if opts.NewRunner == nil {
		opts.NewRunner = func(context.Context, string, string) (Runner, error) {
			return &probeRunner{}, nil
		}
	}
	a, err := NewAgent(opts)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func drainSeq(t *testing.T, sequence iter.Seq2[Event, error]) {
	t.Helper()
	for _, err := range sequence {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
}

// insertIdle puts a session in the table without ever running it, which is what
// an eviction test needs: a session whose last use is in the past.
func insertIdle(t *testing.T, a *Agent, id string, lastUsed time.Time) *session {
	t.Helper()
	entry := newSession(id)
	entry.runner = &probeRunner{}
	entry.lastUsed = lastUsed
	shard := a.table.shard(id)
	shard.mu.Lock()
	shard.insert(entry)
	shard.mu.Unlock()
	return entry
}

func holds(a *Agent, id string) bool {
	shard := a.table.shard(id)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	_, found := shard.lookup(id)
	return found
}

// The message path enforces the cap and nothing else. Scanning the table for
// idle sessions is the sweeper's work: a run must not pay for sessions nothing
// has asked about.
func TestRunDoesNotSweepIdleSessions(t *testing.T) {
	a := probeAgent(t, AgentOptions{IdleTimeout: time.Hour})
	insertIdle(t, a, "stale", time.Now().Add(-2*time.Hour))

	drainSeq(t, a.Run(context.Background(), "live", message.NewTextMessage(message.RoleUser, "hi")))

	if !holds(a, "stale") {
		t.Fatal("a run swept an idle session off the message path")
	}
}

// The background loop is what drops them, and it reports each one.
func TestSweeperEvictsAnIdleSession(t *testing.T) {
	var mu sync.Mutex
	var evicted []string
	a := probeAgent(t, AgentOptions{
		IdleTimeout: 40 * time.Millisecond,
		OnEvict: func(id string) {
			mu.Lock()
			evicted = append(evicted, id)
			mu.Unlock()
		},
	})
	insertIdle(t, a, "stale", time.Now().Add(-time.Hour))

	deadline := time.Now().Add(2 * time.Second)
	for holds(a, "stale") {
		if time.Now().After(deadline) {
			t.Fatal("the sweeper never evicted the idle session")
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Fatalf("evicted = %v, want the idle session", evicted)
	}
}

// A run in flight is never evicted, however idle its clock looks: it is not
// reconstructible mid-step.
func TestSweepSkipsARunningSession(t *testing.T) {
	a := probeAgent(t, AgentOptions{IdleTimeout: 40 * time.Millisecond})
	entry := insertIdle(t, a, "busy", time.Now().Add(-time.Hour))
	shard := a.table.shard("busy")
	shard.mu.Lock()
	entry.running = true
	shard.mu.Unlock()

	// Several sweeps pass, since the interval is a quarter of the timeout.
	time.Sleep(100 * time.Millisecond)
	if !holds(a, "busy") {
		t.Fatal("a running session was swept")
	}

	shard.mu.Lock()
	entry.running = false
	shard.mu.Unlock()
}

// The cap is the one bound the message path enforces, because a bound checked
// once a minute is not a bound.
func TestRunTrimsOverTheCap(t *testing.T) {
	var mu sync.Mutex
	evictions := 0
	a := probeAgent(t, AgentOptions{
		MaxSessions: 2,
		OnEvict: func(string) {
			mu.Lock()
			evictions++
			mu.Unlock()
		},
	})
	for i, id := range []string{"a", "b", "c", "d"} {
		// Each is older than the one before, so "a" is the first to go.
		insertIdle(t, a, id, time.Now().Add(-time.Duration(4-i)*time.Second))
	}
	if got := a.table.count(); got != 4 {
		t.Fatalf("table holds %d sessions before the run, want 4", got)
	}

	drainSeq(t, a.Run(context.Background(), "e", message.NewTextMessage(message.RoleUser, "hi")))

	if got := a.table.count(); got > 2 {
		t.Fatalf("table holds %d sessions after the run, want at most 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if evictions == 0 {
		t.Error("no eviction was reported")
	}
}

// Close stops the loop before it closes what a sweep would have closed.
func TestCloseStopsTheSweeper(t *testing.T) {
	a := probeAgent(t, AgentOptions{IdleTimeout: 40 * time.Millisecond})
	if a.sweeper.stop == nil {
		t.Fatal("no sweeper was started for a bounded table")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.sweeper.stop != nil {
		t.Error("the sweeper was not stopped")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// An unbounded table runs no sweeper at all: nothing to enforce, nothing to pay.
func TestUnboundedTableStartsNoSweeper(t *testing.T) {
	a := probeAgent(t, AgentOptions{})
	if a.sweeper.stop != nil {
		t.Fatal("an unbounded table started a sweeper loop")
	}
	if a.sweeper.interval != 0 {
		t.Fatalf("sweep interval = %v, want none", a.sweeper.interval)
	}
}

// The cadence follows the timeout it enforces, with a floor that keeps a
// pathological configuration from spinning.
func TestSweepInterval(t *testing.T) {
	cases := []struct {
		name string
		opts AgentOptions
		want time.Duration
	}{
		{"unbounded", AgentOptions{}, 0},
		{"cap only", AgentOptions{MaxSessions: 10}, time.Minute},
		{"quarter of the timeout", AgentOptions{IdleTimeout: 2 * time.Second}, 500 * time.Millisecond},
		{"floor", AgentOptions{IdleTimeout: 4 * time.Millisecond}, 10 * time.Millisecond},
		{"ceiling", AgentOptions{IdleTimeout: time.Hour}, time.Minute},
	}
	for _, tc := range cases {
		if got := sweepInterval(tc.opts); got != tc.want {
			t.Errorf("%s: interval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// BenchmarkSweep is the cost of one full pass, which is what the background loop
// pays every interval. It is monitoring, not a gate: the point of the loop is
// that a run no longer pays this.
func BenchmarkSweep(b *testing.B) {
	for _, sessions := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			a, err := NewAgent(AgentOptions{
				IdleTimeout: time.Hour,
				NewRunner: func(context.Context, string, string) (Runner, error) {
					return &probeRunner{}, nil
				},
			})
			if err != nil {
				b.Fatalf("NewAgent: %v", err)
			}
			defer func() { _ = a.Close() }()
			for i := 0; i < sessions; i++ {
				id := fmt.Sprintf("session-%d", i)
				entry := newSession(id)
				entry.lastUsed = time.Now().Add(-time.Minute)
				shard := a.table.shard(id)
				shard.mu.Lock()
				shard.insert(entry)
				shard.mu.Unlock()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = a.sweep(time.Now())
			}
		})
	}
}

// BenchmarkRunWithABoundedTable is the message path with a large table behind it:
// the number that was proportional to the session count before the sweep moved
// off this path.
func BenchmarkRunWithABoundedTable(b *testing.B) {
	for _, sessions := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			a, err := NewAgent(AgentOptions{
				IdleTimeout: time.Hour,
				NewRunner: func(context.Context, string, string) (Runner, error) {
					return &probeRunner{}, nil
				},
			})
			if err != nil {
				b.Fatalf("NewAgent: %v", err)
			}
			defer func() { _ = a.Close() }()
			for i := 0; i < sessions; i++ {
				id := fmt.Sprintf("session-%d", i)
				entry := newSession(id)
				entry.lastUsed = time.Now()
				shard := a.table.shard(id)
				shard.mu.Lock()
				shard.insert(entry)
				shard.mu.Unlock()
			}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				for _, err := range a.Run(ctx, fmt.Sprintf("live-%d", b.N), message.NewTextMessage(message.RoleUser, "hi")) {
					if err != nil {
						b.Fatalf("run: %v", err)
					}
				}
			}
		})
	}
}

// TestTrimKeepsTheMostRecentlyUsed: the cap drops the least recently used idle
// sessions first, and reports exactly those. The order used to be re-derived per
// victim by scanning the whole table; it is now one sorted pass, so the survivors
// are asserted rather than just the count.
func TestTrimKeepsTheMostRecentlyUsed(t *testing.T) {
	var mu sync.Mutex
	var evicted []string
	a := probeAgent(t, AgentOptions{
		MaxSessions: 3,
		OnEvict: func(id string) {
			mu.Lock()
			evicted = append(evicted, id)
			mu.Unlock()
		},
	})
	// oldest -> newest: a, b, c, d, e, f
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertIdle(t, a, id, time.Now().Add(-time.Duration(6-i)*time.Second))
	}
	a.trim()

	if got := a.table.count(); got != 3 {
		t.Fatalf("table holds %d sessions, want 3", got)
	}
	for _, gone := range []string{"a", "b", "c"} {
		if holds(a, gone) {
			t.Errorf("%s survived, want the three oldest evicted", gone)
		}
	}
	for _, kept := range []string{"d", "e", "f"} {
		if !holds(a, kept) {
			t.Errorf("%s was evicted, want the three newest kept", kept)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(evicted)
	if got, want := strings.Join(evicted, ","), "a,b,c"; got != want {
		t.Fatalf("evicted = %s, want %s", got, want)
	}
}

// TestTrimBreaksTiesDeterministically: when several sessions share a last-used
// timestamp the eviction order is still fixed (by id), where the old per-victim
// scan inherited the shard map's random iteration order.
func TestTrimBreaksTiesDeterministically(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	for attempt := 0; attempt < 5; attempt++ {
		a := probeAgent(t, AgentOptions{MaxSessions: 2})
		for _, id := range []string{"a", "b", "c", "d"} {
			insertIdle(t, a, id, at)
		}
		a.trim()
		for _, kept := range []string{"c", "d"} {
			if !holds(a, kept) {
				t.Fatalf("attempt %d: %s was evicted, want the id order to break the tie", attempt, kept)
			}
		}
	}
}

// TestTrimSkipsRunningSessions: a session with a run in flight is never a
// candidate, however old its clock looks.
func TestTrimSkipsRunningSessions(t *testing.T) {
	a := probeAgent(t, AgentOptions{MaxSessions: 1})
	busy := insertIdle(t, a, "busy", time.Now().Add(-time.Hour))
	shard := a.table.shard("busy")
	shard.mu.Lock()
	busy.running = true
	shard.mu.Unlock()
	insertIdle(t, a, "normal", time.Now())

	a.trim()

	if !holds(a, "busy") {
		t.Fatal("a running session was evicted")
	}
	if holds(a, "normal") {
		t.Fatal("the idle session survived while a running one held the cap")
	}
}

// BenchmarkTrimOverCap is the message path's cost when a table sits over its
// cap. over is how many times the cap: the steady state of a capped deployment is
// just over it, where one victim is needed, and that case must stay as cheap as
// the single scan it replaced.
//
// The refill between iterations re-inserts the sessions trim just evicted rather
// than minting new ones: allocations made while the timer is stopped are still
// counted by the benchmark, so newSession/Sprintf churn there would drown out
// trim's own (tens of) allocations.
func BenchmarkTrimOverCap(b *testing.B) {
	for _, sessions := range []int{1000, 5000} {
		for _, over := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("sessions=%d/over=%dx", sessions, over), func(b *testing.B) {
				max := sessions / over
				a, err := NewAgent(AgentOptions{
					MaxSessions: max,
					NewRunner: func(context.Context, string, string) (Runner, error) {
						return &probeRunner{}, nil
					},
				})
				if err != nil {
					b.Fatalf("NewAgent: %v", err)
				}
				defer func() { _ = a.Close() }()
				base := time.Now()
				for i := 0; i < sessions; i++ {
					insertIdleB(b, a, fmt.Sprintf("s-%05d", i), base.Add(-time.Duration(i)*time.Millisecond))
				}
				var refill []*session
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					for _, evicted := range refill {
						shard := a.table.shard(evicted.id)
						shard.mu.Lock()
						shard.insert(evicted)
						shard.mu.Unlock()
					}
					b.StartTimer()
					refill = a.trim()
				}
			})
		}
	}
}

// insertIdleB is insertIdle for benchmarks: it takes a testing.TB and fails the
// benchmark rather than a test.
func insertIdleB(tb testing.TB, a *Agent, id string, lastUsed time.Time) *session {
	tb.Helper()
	entry := newSession(id)
	entry.runner = &probeRunner{}
	entry.lastUsed = lastUsed
	shard := a.table.shard(id)
	shard.mu.Lock()
	shard.insert(entry)
	shard.mu.Unlock()
	return entry
}

// TestTrimIsRaceFreeUnderConcurrentRuns pins the reason trim copies each
// candidate's timestamp while holding the shard lock. `lastUsed` is written by
// acquire and release under that lock; the first version of the single-pass trim
// collected entry pointers and sorted on the live field afterwards, which -race
// reports as a data race as soon as two runs touch the same table.
func TestTrimIsRaceFreeUnderConcurrentRuns(t *testing.T) {
	a := probeAgent(t, AgentOptions{MaxSessions: 2})
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		runErrs []error
	)
	for round := 0; round < 3; round++ {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			// The loop variables are passed in: Go 1.22 makes them per-iteration,
			// but an explicit parameter keeps the intent readable.
			go func(round, i int) {
				defer wg.Done()
				sessionID := fmt.Sprintf("s-%d-%d", round, i)
				// t.Fatalf is illegal off the test goroutine, so a failed run is
				// collected and reported once the goroutines have joined.
				for _, err := range a.Run(context.Background(), sessionID, message.NewTextMessage(message.RoleUser, "hi")) {
					if err != nil {
						mu.Lock()
						runErrs = append(runErrs, err)
						mu.Unlock()
						return
					}
				}
			}(round, i)
		}
	}
	wg.Wait()
	for _, err := range runErrs {
		t.Errorf("run: %v", err)
	}
	// A session is only a trim candidate once its run released it, and release
	// runs on the run's own goroutine, so poll rather than assume the table is
	// settled the instant the sequences end.
	deadline := time.Now().Add(2 * time.Second)
	for a.table.count() > 2 {
		if time.Now().After(deadline) {
			t.Fatalf("table holds %d sessions, want at most 2", a.table.count())
		}
		a.trim()
		time.Sleep(2 * time.Millisecond)
	}
}
