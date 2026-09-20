package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

func steerText(t *testing.T, text string) message.Message {
	t.Helper()
	return message.NewTextMessage(message.RoleUser, text)
}

func textOf(t *testing.T, msg message.Message) string {
	t.Helper()
	if len(msg.Content.Parts) != 1 {
		t.Fatalf("message parts = %#v, want exactly one", msg.Content.Parts)
	}
	part, ok := msg.Content.Parts[0].(message.TextPart)
	if !ok {
		t.Fatalf("message part = %T, want message.TextPart", msg.Content.Parts[0])
	}
	return part.Text
}

// ---------- Turn-owned queue ----------

func TestTurn_SteerDrainRoundTrip(t *testing.T) {
	turn := newTurn(nil, "run-steer", context.Background())
	turn.state = TurnRunning

	if err := turn.Steer(steerText(t, "use the vendored copy")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if err := turn.Steer(steerText(t, "skip the tests")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if got := turn.PendingSteer(); got != 2 {
		t.Fatalf("PendingSteer = %d, want 2", got)
	}

	drained := turn.DrainSteer()
	if len(drained) != 2 {
		t.Fatalf("DrainSteer returned %d messages, want 2", len(drained))
	}
	for i, want := range []string{"use the vendored copy", "skip the tests"} {
		if got := textOf(t, drained[i]); got != want {
			t.Fatalf("drained[%d] = %q, want %q", i, got, want)
		}
	}
	if got := turn.PendingSteer(); got != 0 {
		t.Fatalf("PendingSteer after drain = %d, want 0", got)
	}
	// Take-all is destructive: the same message is never returned twice.
	if again := turn.DrainSteer(); len(again) != 0 {
		t.Fatalf("second DrainSteer = %#v, want empty", again)
	}
}

func TestTurn_DrainSteerEmptyIsNonBlocking(t *testing.T) {
	turn := newTurn(nil, "run-steer", context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if msgs := turn.DrainSteer(); len(msgs) != 0 {
			t.Errorf("DrainSteer on a fresh turn = %#v, want empty", msgs)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DrainSteer blocked on an empty queue")
	}
}

func TestTurn_SteerIsolatesCallerAndConsumerCopies(t *testing.T) {
	turn := newTurn(nil, "run-steer", context.Background())
	turn.state = TurnRunning

	msg := message.NewTextMessage(message.RoleUser, "original")
	if err := turn.Steer(msg); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	// Mutating the caller's copy after Steer must not rewrite the queue.
	msg.Content.Parts[0] = message.TextPart{Text: "mutated by caller"}
	if got := textOf(t, turn.DrainSteer()[0]); got != "original" {
		t.Fatalf("queued text = %q, want the value at submit time", got)
	}

	// Each drain answers for the interval since the last one: a
	// snapshot the consumer still holds cannot be replayed by draining.
	if err := turn.Steer(steerText(t, "second")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	snapshot := turn.DrainSteer()
	snapshot[0].Content.Parts[0] = message.TextPart{Text: "rewritten by consumer"}
	if err := turn.Steer(steerText(t, "third")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if got := textOf(t, turn.DrainSteer()[0]); got != "third" {
		t.Fatalf("queued text = %q, want third", got)
	}
}

func TestTurn_SteerRejections(t *testing.T) {
	t.Run("invalid message", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		err := turn.Steer(message.Message{Role: message.RoleUser})
		if !errdefs.IsValidation(err) {
			t.Fatalf("Steer(invalid) = %v, want a validation error", err)
		}
		if got := turn.PendingSteer(); got != 0 {
			t.Fatalf("PendingSteer = %d after a rejected submit, want 0", got)
		}
	})

	t.Run("oversized message", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		huge := message.NewTextMessage(message.RoleUser, strings.Repeat("x", maxSteerMessageBytes))
		err := turn.Steer(huge)
		if !errors.Is(err, ErrSteerTooLarge) {
			t.Fatalf("Steer(oversized) = %v, want ErrSteerTooLarge", err)
		}
		if !errdefs.IsBudgetExceeded(err) {
			t.Fatalf("Steer(oversized) error class = %v, want budget exceeded", err)
		}
	})

	t.Run("queue full", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		for i := 0; i < maxSteerQueue; i++ {
			if err := turn.Steer(steerText(t, "msg")); err != nil {
				t.Fatalf("Steer #%d: %v", i, err)
			}
		}
		err := turn.Steer(steerText(t, "one too many"))
		if !errors.Is(err, ErrSteerQueueFull) {
			t.Fatalf("Steer over capacity = %v, want ErrSteerQueueFull", err)
		}
		if got := turn.PendingSteer(); got != maxSteerQueue {
			t.Fatalf("PendingSteer = %d, want the rejected message not counted (%d)", got, maxSteerQueue)
		}
		// Draining frees space, so a full queue is recoverable.
		turn.DrainSteer()
		if err := turn.Steer(steerText(t, "after drain")); err != nil {
			t.Fatalf("Steer after drain: %v", err)
		}
	})

	t.Run("terminal turn", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		turn.finish(&agent.Result{Status: agent.StatusCompleted}, nil)

		err := turn.Steer(steerText(t, "too late"))
		if !errors.Is(err, ErrSteerClosed) {
			t.Fatalf("Steer(terminal) = %v, want ErrSteerClosed", err)
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("Steer(terminal) error class = %v, want not available", err)
		}
	})

	t.Run("interrupted turn", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		err := turn.Steer(steerText(t, "stop, wait"))
		if err == nil || !errdefs.IsInterrupted(err) {
			t.Fatalf("Steer(interrupting) = %v, want an interrupted error", err)
		}
		// The class is pinned deliberately: a refused submission reports
		// the pending interrupt, and callers recover it with errors.As
		// instead of reading the message.
		var pending agent.InterruptedError
		if !errors.As(err, &pending) || pending.Cause != agent.CauseUserInput {
			t.Fatalf("Steer(interrupting) = %v, want the pending interrupt recoverable", err)
		}
		if got := turn.PendingSteer(); got != 0 {
			t.Fatalf("PendingSteer = %d after a rejected submit, want 0", got)
		}
	})
}

func TestTurn_SteerConcurrentWithDrainAndFinish(t *testing.T) {
	turn := newTurn(nil, "run-steer", context.Background())
	turn.state = TurnRunning

	const writers, submits, readers, drains = 16, 8, 4, 16
	var delivered, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < submits; j++ {
				if err := turn.Steer(steerText(t, "correction")); err != nil {
					rejected.Add(1)
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < drains; j++ {
				delivered.Add(int64(len(turn.DrainSteer())))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		turn.finish(&agent.Result{Status: agent.StatusCompleted}, nil)
	}()
	wg.Wait()

	// Whatever interleaving the race detector observes, no accepted
	// message may be lost or handed out twice: every drained message is
	// accounted for exactly once, and the post-finish drain collects
	// the tail.
	delivered.Add(int64(len(turn.DrainSteer())))
	accepted := int64(writers*submits) - rejected.Load()
	if delivered.Load() != accepted {
		t.Fatalf("delivered=%d accepted=%d pending=%d", delivered.Load(), accepted, turn.PendingSteer())
	}
}

func TestTurn_UndeliveredSteerIsRecordedOnResult(t *testing.T) {
	t.Run("pending messages", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		if err := turn.Steer(steerText(t, "never drained")); err != nil {
			t.Fatalf("Steer: %v", err)
		}
		result := &agent.Result{Status: agent.StatusInterrupted}
		turn.finish(result, nil)

		if got := result.State[steerPendingStateKey]; got != 1 {
			t.Fatalf("result state %q = %#v, want 1", steerPendingStateKey, got)
		}
		// The messages stay retrievable so the owner can report what
		// never made it into the conversation.
		if msgs := turn.DrainSteer(); len(msgs) != 1 {
			t.Fatalf("DrainSteer after finish = %#v, want the undelivered message", msgs)
		}
	})

	t.Run("drained messages", func(t *testing.T) {
		turn := newTurn(nil, "run-steer", context.Background())
		turn.state = TurnRunning
		if err := turn.Steer(steerText(t, "delivered")); err != nil {
			t.Fatalf("Steer: %v", err)
		}
		turn.DrainSteer()
		result := &agent.Result{Status: agent.StatusCompleted}
		turn.finish(result, nil)

		if got, ok := result.State[steerPendingStateKey]; ok {
			t.Fatalf("result state %q = %#v, want the key absent", steerPendingStateKey, got)
		}
	})
}

// ---------- Session integration ----------

// steerableHost is a HostFactory product that already owns a steer
// queue — the case the session must reject instead of shadowing.
type steerableHost struct {
	testHost
}

func (h steerableHost) DrainSteer() []message.Message { return nil }

func TestSessionInstallsSteerSourceOnTurnHost(t *testing.T) {
	turnStarted := make(chan struct{})
	steerAccepted := make(chan struct{})
	var (
		got     agent.Host
		drained []message.Message
	)
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		got = host
		close(turnStarted)
		// Drain happens at a boundary the engine owns — here, when the
		// test says a message has been steered in.
		select {
		case <-steerAccepted:
		case <-ctx.Done():
			return board, ctx.Err()
		}
		source, ok := agent.SteerFromHost(host)
		if !ok {
			t.Error("SteerFromHost(turn host) = false, want the session-installed source")
			return board, nil
		}
		drained = source.DrainSteer()
		for _, msg := range drained {
			board.AppendChannelMessage(agent.MainChannel, msg)
		}
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine,
		func(bus event.Bus) HostFactory {
			// A base host without SteerSource: the session adds it.
			return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		})

	turn, err := session.Start(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-turnStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("engine never started")
	}

	// The turn-owned source is reachable in one step ...
	if _, ok := got.(agent.SteerSource); !ok {
		t.Fatal("turn host must implement SteerSource directly, not only through traversal")
	}
	// ... and the rest of the host's capabilities survive the wrapper.
	if _, ok := agent.EventBusFromHost(got); !ok {
		t.Fatal("EventBusProvider must remain reachable through the steer wrapper")
	}

	if err := turn.Steer(steerText(t, "actually, use the vendored copy")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if pending := turn.PendingSteer(); pending != 1 {
		t.Fatalf("PendingSteer = %d, want 1", pending)
	}
	close(steerAccepted)

	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(drained) != 1 || textOf(t, drained[0]) != "actually, use the vendored copy" {
		t.Fatalf("engine drained %#v", drained)
	}
	// The deliverable proof: the steered message is in the board the
	// turn produced (and the session commits next turn).
	channel := result.LastBoard.Channel(agent.MainChannel)
	if len(channel) != 2 {
		t.Fatalf("main channel = %d messages, want seed + steered", len(channel))
	}
	if got := textOf(t, channel[1]); got != "actually, use the vendored copy" {
		t.Fatalf("committed steered message = %q", got)
	}
	if _, ok := result.State[steerPendingStateKey]; ok {
		t.Fatalf("result state %q set for a drained steer: %#v", steerPendingStateKey, result.State)
	}
}

func TestSessionSteerSourceOnEphemeralTurn(t *testing.T) {
	var (
		got agent.SteerSource
		ok  bool
	)
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		got, ok = agent.SteerFromHost(host)
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine,
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
				return testHost{bus: bus}, nil
			})
		})

	turn, err := session.StartWithOptions(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	}, WithEphemeral())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !ok || got == nil {
		t.Fatal("ephemeral turn host must expose the turn-owned steer source")
	}
}

func TestSessionRejectsSteerSourceFromHostFactory(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		t.Error("engine must not run when the factory host owns a steer queue")
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine,
		func(bus event.Bus) HostFactory {
			return HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
				return steerableHost{testHost{bus: bus}}, nil
			})
		})

	_, err := session.Start(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("Start with a steerable factory host = %v, want a conflict", err)
	}
}
