package kanban

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Board concurrency — race / deadlock guards.
//
// Always run with -race. None of these tests assert ordering; they assert
// invariants that must hold under any interleaving (no panics, all events
// delivered, indexes stay consistent).
// ---------------------------------------------------------------------------

func TestBoard_Concurrent_ProduceAndQuery(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.Produce("task", "p", nil)
		}()
		go func() {
			defer wg.Done()
			_ = b.Query(CardFilter{Type: "task"})
		}()
	}
	wg.Wait()

	if got := b.Len(); got != n {
		t.Fatalf("Len()=%d, want %d", got, n)
	}
}

func TestBoard_Concurrent_FullLifecycle(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	const n = 100

	cardIDs := make([]string, n)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c := b.Produce("task", fmt.Sprintf("p-%d", idx), map[string]any{"idx": idx})
			cardIDs[idx] = c.ID
		}(i)
	}
	wg.Wait()
	if got := b.Len(); got != n {
		t.Fatalf("after Produce: Len()=%d, want %d", got, n)
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if !b.Claim(cardIDs[idx], fmt.Sprintf("c-%d", idx)) {
				t.Errorf("claim %d failed", idx)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.Done(cardIDs[idx], fmt.Sprintf("result-%d", idx))
		}(i)
	}
	wg.Wait()

	if got := len(b.Query(CardFilter{Status: CardDone})); got != n {
		t.Fatalf("done cards = %d, want %d", got, n)
	}
}

func TestBoard_Concurrent_WatchAndProduce(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	const watchers = 5
	const produces = 20

	channels := make([]<-chan *Card, watchers)
	for i := 0; i < watchers; i++ {
		channels[i] = b.WatchFiltered(ctx, CardFilter{})
	}

	var wg sync.WaitGroup
	for i := 0; i < produces; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.Produce("task", "p", map[string]any{"i": idx})
		}(i)
	}
	wg.Wait()

	for wi, ch := range channels {
		count := 0
		timeout := time.After(time.Second)
	drain:
		for {
			select {
			case <-ch:
				count++
				if count == produces {
					break drain
				}
			case <-timeout:
				break drain
			}
		}
		if count != produces {
			t.Fatalf("watcher %d: got %d events, want %d", wi, count, produces)
		}
	}
}

func TestBoard_Concurrent_BusSubscribe(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	ctx := context.Background()
	const subs = 20

	var wg sync.WaitGroup
	errs := make(chan error, subs)
	for i := 0; i < subs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Bus().Subscribe(ctx, event.Pattern(">")); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Subscribe failed: %v", err)
	}
}

// CountByStatus must remain consistent with Query() under concurrent
// state transitions — guards the cardIndex / statusIndex pair.
func TestBoard_Concurrent_IndexConsistency(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	const n = 50

	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = b.Produce("task", "p", nil).ID
	}

	if got := b.CountByStatus(CardPending, ""); got != n {
		t.Fatalf("initial pending=%d, want %d", got, n)
	}

	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(id string) {
			b.Claim(id, "a")
			b.Done(id, "r")
			done <- struct{}{}
		}(ids[i])
	}
	for i := 0; i < n; i++ {
		<-done
	}

	for status, want := range map[CardStatus]int{
		CardDone:    n,
		CardPending: 0,
		CardClaimed: 0,
	} {
		if got := b.CountByStatus(status, ""); got != want {
			t.Errorf("CountByStatus(%s)=%d, want %d", status, got, want)
		}
	}
}
