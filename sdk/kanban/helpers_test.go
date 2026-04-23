package kanban

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Shared test helpers
//
// All kanban_test files use these helpers to avoid duplicating
// mockExecutor / drain / scope-id boilerplate. Keep this file small and free
// of test functions — tests live in their topic-specific files.
// ---------------------------------------------------------------------------

// scopeCounter generates unique scope IDs across parallel tests so that
// resource leak diagnostics in failures point at the right test.
var scopeCounter atomic.Uint64

// scopeID returns a per-call unique scope identifier rooted at the test name.
// Use this in any test that creates a Board so failures stay attributable.
func scopeID(t *testing.T) string {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "-")
	return name + "-" + itoa(scopeCounter.Add(1))
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// newBoard returns a Board whose Close is registered with t.Cleanup, so tests
// never leak the board lifecycle goroutine.
func newBoard(t *testing.T, opts ...BoardOption) *Board {
	t.Helper()
	b := NewBoard(scopeID(t), opts...)
	t.Cleanup(b.Close)
	return b
}

// newKanban returns a Kanban + Board pair, both auto-stopped on test cleanup.
func newKanban(t *testing.T, opts ...Option) (*Kanban, *Board) {
	t.Helper()
	b := newBoard(t)
	k := New(context.Background(), b, opts...)
	t.Cleanup(k.Stop)
	return k, b
}

// ---------------------------------------------------------------------------
// mockExecutor — shared across kanban / scheduler / events tests
// ---------------------------------------------------------------------------

type mockExecutor struct {
	fn func(ctx context.Context, scopeID, targetAgentID string, card *Card, query string, inputs map[string]any) error
}

func (m *mockExecutor) ExecuteTask(ctx context.Context, scopeID, targetAgentID string, card *Card, query string, inputs map[string]any) error {
	if m.fn != nil {
		return m.fn(ctx, scopeID, targetAgentID, card, query, inputs)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Channel / event helpers
// ---------------------------------------------------------------------------

// waitCard blocks for one card from ch up to timeout, failing the test on
// timeout with a labelled diagnostic. Use this instead of bare select+t.Fatal.
func waitCard(t *testing.T, ch <-chan *Card, label string) *Card {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed unexpectedly", label)
		}
		return c
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: timed out waiting for card", label)
		return nil
	}
}

// drainEvents collects every event emitted on sub until either matchCount
// events satisfy match (returning early), or the deadline elapses.
//
// match may be nil to collect everything. The returned slice preserves
// arrival order and includes all events seen, not just the matching ones.
func drainEvents(sub event.LegacySubscription, deadline time.Duration, matchCount int, match func(event.Event) bool) []event.Event {
	var out []event.Event
	matched := 0
	timeout := time.After(deadline)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if match != nil && match(ev) {
				matched++
				if matchCount > 0 && matched >= matchCount {
					return out
				}
			}
		case <-timeout:
			return out
		}
	}
}

// subscribeBus subscribes to b.Bus() with a generous buffer and registers
// the subscription's Close with t.Cleanup.
func subscribeBus(t *testing.T, b *Board) event.LegacySubscription {
	t.Helper()
	sub, err := b.Bus().Subscribe(context.Background(), event.EventFilter{}, event.LegacyWithBufferSize(1024))
	if err != nil {
		t.Fatalf("Bus().Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}
