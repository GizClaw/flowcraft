//go:build linux

package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The cost of the inotify source, held still.
//
// A kernel event names the entry it is about, so one event costs one
// amortised read() plus a parse and a map lookup — and the width of the
// directory the event landed in must not appear in that number at all.
// That is the property this file exists to keep: a source that re-reads
// the directory to find out what changed (the shape macOS needs, see
// source_darwin.go) pays for the directory on every event instead,
// which is what the benchmark below makes visible and the test below
// refuses.
//
// One measurement is one batch: files are written with the watch
// attached and nothing is drained, then the queue is drained while the
// clock runs. That shape is what makes the numbers attributable: the
// writer's own work (and the kernel's event generation, which is
// charged to the write) happens outside the measured section, so what
// is left is the source's half — the read(), the parse and the
// bookkeeping. The batch is sized from the kernel's own queue limit so
// the measured drain is never the overflow path.
//
// Run with:
//
//	go test -bench=BenchmarkLinuxDrain -run '^$' ./sandbox/journal

// drainCost is one measurement: how many events the source handed over,
// how long that took, and what it allocated doing it.
type drainCost struct {
	events int
	took   time.Duration
	bytes  uint64
}

func (c drainCost) perEvent() time.Duration {
	if c.events == 0 {
		return 0
	}
	return c.took / time.Duration(c.events)
}

func (c drainCost) bytesPerEvent() float64 {
	if c.events == 0 {
		return 0
	}
	return float64(c.bytes) / float64(c.events)
}

// batchWrites is how many files one measurement writes. It is derived
// from the kernel's queue limit — a full write is three events, and a
// full queue turns into an overflow event rather than into the events
// being measured — and capped so a measurement stays a measurement.
func batchWrites(tb testing.TB) int {
	const (
		fallback  = 512
		ceiling   = 2000
		perWrite  = 3
		headroom  = 64
		minWrites = 32
	)
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		return fallback
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || limit <= 0 {
		return fallback
	}
	writes := min(limit/perWrite-headroom, ceiling)
	if writes < minWrites {
		tb.Skipf("inotify queues only %d events: too few to measure a per-event cost", limit)
	}
	return writes
}

// fillDirectory puts n files in dir, so the directory has the width a
// caller wants to measure the cost of.
func fillDirectory(tb testing.TB, dir string, n int) {
	tb.Helper()
	for i := range n {
		name := filepath.Join(dir, fmt.Sprintf("entry%06d", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

// measureDrain writes one batch into a directory of the given width and
// times the drain of everything those writes queued. The width is the
// only difference between two calls, which is what makes the pair a
// statement about the width.
func measureDrain(tb testing.TB, width int) drainCost {
	tb.Helper()
	writes := batchWrites(tb)
	root := tb.TempDir()
	fillDirectory(tb, root, width)

	src, err := openSource()
	if err != nil {
		tb.Fatalf("openSource: %v", err)
	}
	tb.Cleanup(func() { _ = src.Close() })
	if _, err := src.Add(root); err != nil {
		tb.Fatalf("Add: %v", err)
	}
	linux := src.(*linuxSource)

	for i := range writes {
		name := filepath.Join(root, fmt.Sprintf("written%06d.tmp", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			tb.Fatal(err)
		}
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	var cost drainCost
	for {
		// drainLocked is the read-and-parse half of Poll without the
		// wait: what a Poll costs once the queue holds events, which is
		// exactly the cost the width must not touch.
		events, err := linux.drainLocked()
		if err != nil {
			tb.Fatalf("drain: %v", err)
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			if ev.Op == rawOverflow {
				tb.Fatalf("the kernel overflowed its queue: %d writes do not fit, so this measured a gap",
					writes)
			}
		}
		cost.events += len(events)
	}
	cost.took = time.Since(start)
	runtime.ReadMemStats(&after)
	cost.bytes = after.TotalAlloc - before.TotalAlloc
	if cost.events == 0 {
		tb.Fatal("drained no events: the writes produced none")
	}
	return cost
}

func reportDrain(b *testing.B, width int, cost drainCost) {
	b.Helper()
	b.ReportMetric(float64(cost.perEvent().Nanoseconds()), "ns/event")
	b.ReportMetric(cost.bytesPerEvent(), "B/event")
	b.Logf("dir width %6d: %d events, %v/event, %.1f B/event",
		width, cost.events, cost.perEvent().Round(time.Nanosecond), cost.bytesPerEvent())
}

// benchDrainDirectory is the whole point: the same workload in a
// directory of a different width. The two numbers are worth comparing
// to each other, not to the machine that produced them.
func benchDrainDirectory(b *testing.B, width int) {
	b.Helper()
	reportDrain(b, width, measureDrain(b, width))
}

// BenchmarkLinuxDrainDirectoryNarrow is the baseline: a directory small
// enough that re-reading it would still be cheap.
func BenchmarkLinuxDrainDirectoryNarrow(b *testing.B) {
	benchDrainDirectory(b, 64)
}

// BenchmarkLinuxDrainDirectoryWide is the shape that punishes a source
// which looks at the directory: thousands of entries, where a re-read
// per event costs thousands of times more than the event itself.
func BenchmarkLinuxDrainDirectoryWide(b *testing.B) {
	benchDrainDirectory(b, 4096)
}

// TestLinuxDrainCostDoesNotScaleWithDirectoryWidth pins the property in
// a form that fails the build, not just a graph.
//
// The allocation half is the sharp one and the reason it is here: a
// source that derived events from the directory would have to hold the
// directory's entries — tens of thousands of bytes per event in the
// wide shape — while this source's per-event allocation is one name
// string and 40 bytes of event, whatever the width is.
//
// The timing half is deliberately generous. The failure it guards
// against is a directory walk, which is two orders of magnitude, not a
// few percent; a tight bound on a shared CI runner would only buy
// flakes.
func TestLinuxDrainCostDoesNotScaleWithDirectoryWidth(t *testing.T) {
	narrow := measureDrain(t, 64)
	wide := measureDrain(t, 4096)
	t.Logf("narrow: %v/event, %.1f B/event over %d events",
		narrow.perEvent().Round(time.Nanosecond), narrow.bytesPerEvent(), narrow.events)
	t.Logf("wide:   %v/event, %.1f B/event over %d events",
		wide.perEvent().Round(time.Nanosecond), wide.bytesPerEvent(), wide.events)

	if wide.events != narrow.events {
		t.Fatalf("the two shapes saw %d and %d events: the comparison needs the same work on both sides",
			narrow.events, wide.events)
	}
	if limit := narrow.bytesPerEvent()*2 + 512; wide.bytesPerEvent() > limit {
		t.Errorf("wide directory allocated %.1f B/event, over the %.1f B/event limit derived from the narrow one (%.1f B/event): the source is looking at the directory",
			wide.bytesPerEvent(), limit, narrow.bytesPerEvent())
	}
	if limit := narrow.perEvent()*8 + 2*time.Microsecond; wide.perEvent() > limit {
		t.Errorf("wide directory cost %v/event, over the %v/event limit derived from the narrow one (%v/event): the source is paying for the directory's width",
			wide.perEvent(), limit, narrow.perEvent())
	}
}
