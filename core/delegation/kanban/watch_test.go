package kanban_test

import (
	"context"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/delegation/kanban"
)

func receiveCard(t *testing.T, channel <-chan *kanban.Card) *kanban.Card {
	t.Helper()
	select {
	case card, ok := <-channel:
		if !ok {
			t.Fatal("watch closed early")
		}
		return card
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for card")
		return nil
	}
}

func TestWatchObservesTypedTransitions(t *testing.T) {
	board := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := board.Watch(ctx, kanban.Filter{Target: "worker"})
	id := submit(t, board, "worker")
	work, err := board.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
		Status: sdkdelegation.StatusSucceeded,
		Output: "done",
	}); err != nil {
		t.Fatal(err)
	}

	for _, status := range []kanban.Status{
		kanban.StatusPending,
		kanban.StatusClaimed,
		kanban.StatusDone,
	} {
		card := receiveCard(t, watch)
		if card.ID != id || card.Status != status ||
			card.Task.Request.Request.Target != "worker" {
			t.Fatalf("watch card = %+v", card)
		}
	}
}

func TestWatchDoesNotLoseSlowConsumerTransitions(t *testing.T) {
	board := newBoard(t)
	watch := board.Watch(context.Background(), kanban.Filter{})
	const total = 200
	for range total {
		submit(t, board, "worker")
	}
	for index := range total {
		if card := receiveCard(t, watch); card.Status != kanban.StatusPending {
			t.Fatalf("card %d status = %q", index, card.Status)
		}
	}
}

func TestWatchClosesOnContextAndBoardClose(t *testing.T) {
	tests := []struct {
		name  string
		close func(*kanban.Board, context.CancelFunc)
	}{
		{"context", func(_ *kanban.Board, cancel context.CancelFunc) { cancel() }},
		{"board", func(board *kanban.Board, _ context.CancelFunc) { _ = board.Close() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			board := kanban.New("watch-close")
			ctx, cancel := context.WithCancel(context.Background())
			watch := board.Watch(ctx, kanban.Filter{})
			test.close(board, cancel)
			defer cancel()
			defer func() { _ = board.Close() }()
			select {
			case _, ok := <-watch:
				if ok {
					t.Fatal("watch delivered after closure")
				}
			case <-time.After(time.Second):
				t.Fatal("watch did not close promptly")
			}
		})
	}
}
