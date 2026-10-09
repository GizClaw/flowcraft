package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/delegation/kanban"
	"github.com/GizClaw/flowcraft/core/event"
)

// newTestBoard returns a delegation board publishing onto bus.
func newTestBoard(t *testing.T, bus event.Bus) *kanban.Board {
	t.Helper()
	board := kanban.New("test-scope", kanban.WithBus(bus))
	t.Cleanup(func() { _ = board.Close() })
	return board
}

// submitCard puts one pending delegation on the board.
func submitCard(t *testing.T, board *kanban.Board) string {
	t.Helper()
	id, err := board.Submit(context.Background(), sdkdelegation.AsyncRequest{
		Request: sdkdelegation.Request{
			Mode:   sdkdelegation.ModeAsync,
			Target: "researcher",
			Input:  "summarize the corpus",
		},
		Caller: "planner",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return id
}

// startsAndStops builds a runner whose subagent hooks append to two
// files, one per event.
func startsAndStops(t *testing.T) (*Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	start := filepath.Join(dir, "start.out")
	stop := filepath.Join(dir, "stop.out")
	path := writeHooks(t, `{
		"hooks": {
			"SubagentStart": [{"hooks": [{"command": "`+appendHook(start)+`"}]}],
			"SubagentStop":  [{"hooks": [{"command": "`+appendHook(stop)+`"}]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return manager, start, stop
}

func TestObserverForwardsBoardTransitions(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	manager, start, stop := startsAndStops(t)
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	observer, err := NewObserver(manager, bus)
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	if err := observer.Wire(context.Background()); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	board := newTestBoard(t, bus)
	id := submitCard(t, board)
	// A submission is pending, not started: no hook fires for it.
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(start); !os.IsNotExist(err) {
		t.Fatal("a pending card fired SubagentStart")
	}

	if !board.ClaimCard(id, "researcher") {
		t.Fatal("ClaimCard: card was not claimable")
	}
	received := waitForContent(t, start)
	for _, want := range []string{
		`"event":"SubagentStart"`,
		`"subagent":"researcher"`,
		`"card_id":"` + id + `"`,
		`"status":"claimed"`,
		`"target":"researcher"`,
		`"message":"summarize the corpus"`,
	} {
		if !strings.Contains(string(received), want) {
			t.Fatalf("SubagentStart payload %s is missing %s", received, want)
		}
	}

	if !board.Cancel(id, "no longer needed") {
		t.Fatal("Cancel: card was not cancellable")
	}
	stopped := waitForContent(t, stop)
	for _, want := range []string{
		`"event":"SubagentStop"`,
		`"status":"canceled"`,
		`"card_id":"` + id + `"`,
	} {
		if !strings.Contains(string(stopped), want) {
			t.Fatalf("SubagentStop payload %s is missing %s", stopped, want)
		}
	}
}

func TestSubagentEventMapping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status kanban.Status
		want   string
	}{
		{kanban.StatusClaimed, EventSubagentStart},
		{kanban.StatusDone, EventSubagentStop},
		{kanban.StatusFailed, EventSubagentStop},
		{kanban.StatusCanceled, EventSubagentStop},
		{kanban.StatusPending, ""},
		{kanban.StatusSuspended, ""},
	} {
		got, ok := subagentEvent(tc.status)
		if got != tc.want || ok != (tc.want != "") {
			t.Fatalf("subagentEvent(%q) = %q, %v; want %q",
				tc.status, got, ok, tc.want)
		}
	}
}

func TestObserverCloseStopsFiring(t *testing.T) {
	t.Parallel()
	manager, start, _ := startsAndStops(t)
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	observer, err := NewObserver(manager, bus)
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	if err := observer.Wire(context.Background()); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	board := newTestBoard(t, bus)
	id := submitCard(t, board)
	if !board.ClaimCard(id, "researcher") {
		t.Fatal("ClaimCard: card was not claimable")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(start); !os.IsNotExist(err) {
		t.Fatal("a closed observer still fired hooks")
	}
}

func TestObserverWireIsIdempotent(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	manager, start, _ := startsAndStops(t)
	bus := event.NewMemoryBus()
	t.Cleanup(func() { _ = bus.Close() })
	observer, err := NewObserver(manager, bus)
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	ctx := context.Background()
	if err := observer.Wire(ctx); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	first := observer.sub.ID()
	if err := observer.Wire(ctx); err != nil {
		t.Fatalf("second Wire: %v", err)
	}
	if observer.sub.ID() != first {
		t.Fatalf("Wire subscribed twice: %v -> %v", first, observer.sub.ID())
	}

	// One transition, one hook run: a second subscription would double
	// it.
	board := newTestBoard(t, bus)
	id := submitCard(t, board)
	if !board.ClaimCard(id, "researcher") {
		t.Fatal("ClaimCard: card was not claimable")
	}
	received := waitForContent(t, start)
	time.Sleep(150 * time.Millisecond)
	after, err := os.ReadFile(start)
	if err != nil {
		t.Fatalf("read start output: %v", err)
	}
	if len(after) != len(received) {
		t.Fatalf("SubagentStart ran twice: %q then %q", received, after)
	}
}

func TestNewObserverValidatesArguments(t *testing.T) {
	t.Parallel()
	if _, err := NewObserver(nil, event.NewMemoryBus()); err == nil {
		t.Fatal("nil runner must be rejected")
	}
	manager, _, _ := startsAndStops(t)
	if _, err := NewObserver(manager, nil); err == nil {
		t.Fatal("nil bus must be rejected")
	}
}
