package journal

import (
	"testing"
	"time"
)

// pollSource is the half of a platform watch source these helpers need:
// enough to drain raw events without knowing which kernel produced
// them, so one copy serves inotify, kqueue and ReadDirectoryChangesW.
type pollSource interface {
	Poll(timeout time.Duration) ([]rawEvent, error)
}

// collect polls until want events have been seen or the deadline
// expires, and returns everything it saw.
func collect(t *testing.T, src pollSource, want int, timeout time.Duration) []rawEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var seen []rawEvent
	for time.Now().Before(deadline) {
		events, err := src.Poll(5 * time.Millisecond)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		seen = append(seen, events...)
		if len(seen) >= want {
			return seen
		}
	}
	t.Fatalf("saw %d events (%v), want %d", len(seen), describeRaw(seen), want)
	return nil
}

// settle polls for d and returns whatever showed up: how "and nothing
// else" gets asserted at the source level.
func settle(t *testing.T, src pollSource, d time.Duration) []rawEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	var seen []rawEvent
	for time.Now().Before(deadline) {
		events, err := src.Poll(5 * time.Millisecond)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		seen = append(seen, events...)
	}
	return seen
}

func describeRaw(events []rawEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Op.String() + " " + ev.Name
	}
	return out
}
