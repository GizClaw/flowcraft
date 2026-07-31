package kanban_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/kanban"
)

// recv reads one card with a deadline so a hang fails loudly instead of
// stalling the suite.
func recv(t *testing.T, ch <-chan *kanban.Card) *kanban.Card {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed early")
		}
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a card")
		return nil
	}
}

func TestWatchObservesEveryTransition(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := k.Watch(ctx, kanban.Filter{})

	card := mustSubmit(t, k, "w")
	k.Claim(card.ID, "worker")
	k.Done(card.ID, kanban.Result{Output: "out"})

	want := []kanban.Status{
		kanban.StatusPending, kanban.StatusClaimed, kanban.StatusDone,
	}
	for _, w := range want {
		got := recv(t, ch)
		if got.Status != w {
			t.Fatalf("Status = %q, want %q", got.Status, w)
		}
		if got.ID != card.ID {
			t.Fatalf("ID = %q, want %q", got.ID, card.ID)
		}
	}
}

func TestWatchFilterSelects(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := k.Watch(ctx, kanban.Filter{TargetAgentID: "beta"})

	mustSubmit(t, k, "alpha")
	want := mustSubmit(t, k, "beta")

	got := recv(t, ch)
	if got.ID != want.ID {
		t.Fatalf("received the card for alpha; filter did not select")
	}
}

func TestWatchDeliversSuspendAndResume(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := k.Watch(ctx, kanban.Filter{})

	card := mustSubmit(t, k, "w")
	k.Claim(card.ID, "worker")
	k.Suspend(card.ID, "ckpt")
	k.Resume(card.ID)

	want := []kanban.Status{
		kanban.StatusPending,
		kanban.StatusClaimed,
		kanban.StatusSuspended,
		// A resumed card is once again work waiting to be claimed.
		kanban.StatusPending,
	}
	for i, w := range want {
		got := recv(t, ch)
		if got.Status != w {
			t.Fatalf("transition %d: Status = %q, want %q", i, got.Status, w)
		}
	}
}

// A slow consumer must not lose transitions: a missed terminal event
// would leave whoever is waiting on it stuck forever.
func TestWatchQueueIsUnbounded(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := k.Watch(ctx, kanban.Filter{})

	const n = 200
	for range n {
		mustSubmit(t, k, "w")
	}

	for i := range n {
		got := recv(t, ch)
		if got.Status != kanban.StatusPending {
			t.Fatalf("card %d: Status = %q", i, got.Status)
		}
	}
}

func TestWatchClosesOnContextCancel(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := k.Watch(ctx, kanban.Filter{})
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch channel not closed after ctx cancel")
		}
	}
}

func TestWatchClosesOnBoardClose(t *testing.T) {
	k := kanban.New("scope")
	ch := k.Watch(context.Background(), kanban.Filter{})
	_ = k.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("watch channel not closed after board close")
		}
	}
}

func TestWatchOnClosedBoardReturnsClosedChannel(t *testing.T) {
	k := kanban.New("scope")
	_ = k.Close()
	ch := k.Watch(context.Background(), kanban.Filter{})
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("received a card from a closed board")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel from a closed board never closed")
	}
}

// The canonical executor loop: several workers watch for pending cards
// and race to claim. Every card must be executed exactly once.
func TestConcurrentWorkersEachCardOnce(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		workers = 8
		cards   = 50
	)

	var (
		mu       sync.Mutex
		executed = make(map[string]int)
		wg       sync.WaitGroup
	)
	done := make(chan struct{})

	// Subscribe every worker before submitting: Watch reports
	// transitions from the moment it is called and does not replay
	// history, so a card produced before a watcher exists is invisible
	// to it.
	channels := make([]<-chan *kanban.Card, workers)
	for w := range workers {
		channels[w] = k.Watch(ctx, kanban.Filter{Status: kanban.StatusPending})
	}

	for w := range workers {
		wg.Add(1)
		go func(ch <-chan *kanban.Card) {
			defer wg.Done()
			for card := range ch {
				if !k.Claim(card.ID, "worker") {
					continue
				}
				mu.Lock()
				executed[card.ID]++
				n := len(executed)
				mu.Unlock()
				k.Done(card.ID, kanban.Result{Output: "ok"})
				if n == cards {
					select {
					case <-done:
					default:
						close(done)
					}
				}
			}
		}(channels[w])
	}

	for range cards {
		mustSubmit(t, k, "w")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not drain the board")
	}
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(executed) != cards {
		t.Fatalf("executed %d distinct cards, want %d", len(executed), cards)
	}
	for id, n := range executed {
		if n != 1 {
			t.Errorf("card %s executed %d times, want exactly 1", id, n)
		}
	}
}

// Waiting for a specific card requires subscribing before submitting.
// This is the pattern the package doc prescribes in place of a
// board-owned synchronous Call.
func TestWatchThenSubmitCatchesFastCompletion(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := k.Watch(ctx, kanban.Filter{})
	card := mustSubmit(t, k, "w")

	// Complete immediately, before the waiter reads anything.
	k.Claim(card.ID, "worker")
	k.Done(card.ID, kanban.Result{Output: "fast"})

	for {
		got := recv(t, ch)
		if got.ID == card.ID && got.Status.IsTerminal() {
			if got.Result.Output != "fast" {
				t.Errorf("Output = %q", got.Result.Output)
			}
			return
		}
	}
}

// Watch is a live subscription, not a log: it reports transitions that
// happen after it is created. An executor starting against a board that
// already holds work must therefore drain the backlog with Query before
// relying on the stream, or those cards sit pending forever.
func TestWatchDoesNotReplayHistory(t *testing.T) {
	k := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	early := mustSubmit(t, k, "w")
	ch := k.Watch(ctx, kanban.Filter{Status: kanban.StatusPending})
	late := mustSubmit(t, k, "w")

	got := recv(t, ch)
	if got.ID == early.ID {
		t.Fatal("Watch replayed a card submitted before subscription")
	}
	if got.ID != late.ID {
		t.Fatalf("received %q, want %q", got.ID, late.ID)
	}

	// The missed card is still reachable, which is how an executor
	// catches up on startup.
	var found bool
	for _, c := range k.Query(kanban.Filter{Status: kanban.StatusPending}) {
		if c.ID == early.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the card Watch missed is not reachable via Query")
	}
}
