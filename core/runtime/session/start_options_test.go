package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

// sessionRecordingStore is a minimal CheckpointDeleter that records every
// saved checkpoint, distinguishing session-state records from run
// checkpoints.
type sessionRecordingStore struct {
	mu    sync.Mutex
	saved []agent.Checkpoint
}

func (s *sessionRecordingStore) Save(_ context.Context, cp agent.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, cp.Clone())
	return nil
}

func (s *sessionRecordingStore) Load(context.Context, string) (*agent.Checkpoint, error) {
	return nil, nil
}

func (s *sessionRecordingStore) Delete(context.Context, string) error {
	return nil
}

func (s *sessionRecordingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

func (s *sessionRecordingStore) isEmpty() bool {
	return s.count() == 0
}

func TestStartWithOptionsAttachesSinks(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := session.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithSinks(SinkSpec{ID: "s1", Sink: discardSink{}}),
	)
	if err != nil {
		t.Fatalf("StartWithOptions: %v", err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q, want completed", result.Status)
	}
}

func TestStartWithOptionsRejectsNilOption(t *testing.T) {
	_, session, _, _ := newTurnSession(t, noopTestEngine(), turnHostFactory)
	if _, err := session.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		nil,
	); !errdefs.IsValidation(err) {
		t.Fatalf("nil option error = %v, want validation", err)
	}
}

func TestEphemeralSessionPropertyFixedOnFirstStart(t *testing.T) {
	_, session, _, _ := newTurnSession(t, noopTestEngine(), turnHostFactory)
	if err := session.setEphemeralProperty(false); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := session.setEphemeralProperty(true); !errdefs.IsValidation(err) {
		t.Fatalf("mixing set error = %v, want validation", err)
	}

	// A plain Start on an ephemeral session is an implicit non-ephemeral
	// start and must be rejected.
	_, session2, _, _ := newTurnSession(t, noopTestEngine(), turnHostFactory)
	turn, err := session2.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithEphemeral(),
	)
	if err != nil {
		t.Fatalf("ephemeral start: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session2.Start(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "again")},
	); !errdefs.IsValidation(err) {
		t.Fatalf("mixing Start error = %v, want validation", err)
	}
}

func TestEphemeralSessionWritesNoState(t *testing.T) {
	store := &sessionRecordingStore{}
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		// The engine checkpoints mid-run; the ephemeral host must swallow it.
		if checkpointer, ok := host.(agent.Checkpointer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = checkpointer.Checkpoint(ctx, agent.Checkpoint{
				ExecID: "run-checkpoint",
			})
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory,
		WithResume(true), WithCheckpointStore(store))
	turn, err := session.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithEphemeral(),
	)
	if err != nil {
		t.Fatalf("StartWithOptions: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.isEmpty() {
		t.Fatalf("ephemeral session wrote %d checkpoints, want none", store.count())
	}
	if _, ok, _ := session.Resumable(context.Background()); ok {
		t.Fatal("ephemeral session reports resumable state")
	}
	if _, err := session.Resume(context.Background()); !errdefs.IsNotFound(err) {
		t.Fatalf("Resume on ephemeral session error = %v, want not found", err)
	}
}

func TestEphemeralHostPreservesOptionalCapabilities(t *testing.T) {
	store := &sessionRecordingStore{}
	var busFound bool
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		// The ephemeral wrapper must not hide optional host capabilities
		// (delegation service, event bus, ...) from CapabilityFromHost.
		_, busFound = agent.EventBusFromHost(host)
		// Checkpoint suppression must still hold through the wrapper.
		if checkpointer, ok := host.(agent.Checkpointer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = checkpointer.Checkpoint(ctx, agent.Checkpoint{
				ExecID: "run-checkpoint",
			})
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory,
		WithResume(true), WithCheckpointStore(store))
	turn, err := session.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithEphemeral(),
	)
	if err != nil {
		t.Fatalf("StartWithOptions: %v", err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !busFound {
		t.Fatal("EventBusFromHost(ephemeral host) = not found, want the inner host's bus")
	}
	if !store.isEmpty() {
		t.Fatalf("ephemeral host wrote %d checkpoints, want none", store.count())
	}
}

func TestEphemeralCanceledTurnNeverParks(t *testing.T) {
	store := &sessionRecordingStore{}
	blocking := make(chan struct{})
	engine := agent.EngineFunc(func(ctx context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		select {
		case <-blocking:
		case <-ctx.Done():
			return board, ctx.Err()
		}
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory,
		WithResume(true), WithCheckpointStore(store))
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := session.StartWithOptions(ctx,
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithEphemeral(),
	)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.isEmpty() {
		t.Fatalf("canceled ephemeral turn wrote %d checkpoints, want none", store.count())
	}
	if _, ok, _ := session.Resumable(context.Background()); ok {
		t.Fatal("canceled ephemeral turn left a parked run")
	}
}

func TestEphemeralSessionSkipsHistorySeeding(t *testing.T) {
	store := &sessionRecordingStore{}
	var sawHistory bool
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		for _, msg := range board.Channel(agent.MainChannel) {
			if msg.Content.Text() == "old history" {
				sawHistory = true
				break
			}
		}
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory,
		WithResume(true), WithCheckpointStore(store))
	// Pre-seed a durable record under the session key; an ephemeral Start
	// must ignore it entirely.
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleUser, "old history"))
	state := sessionState{Board: board.Snapshot()}
	session.saveSessionState(&state, store, true)

	turn, err := session.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithEphemeral(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sawHistory {
		t.Fatal("ephemeral session seeded history from persisted state")
	}
}

func TestResumeWithOptionsRejectsEphemeral(t *testing.T) {
	store := &sessionRecordingStore{}
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory,
		WithResume(true), WithCheckpointStore(store))
	if _, err := session.ResumeWithOptions(
		context.Background(),
		WithEphemeral(),
	); !errdefs.IsValidation(err) {
		t.Fatalf("ResumeWithOptions(WithEphemeral) error = %v, want validation", err)
	}
}

func TestWithAskUserOverride(t *testing.T) {
	engine := agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		reply, err := host.AskUser(context.Background(), agent.UserPrompt{Source: "override"})
		if err != nil {
			return board, err
		}
		if len(reply.Parts) != 1 {
			return board, errdefs.Internalf("unexpected reply parts: %d", len(reply.Parts))
		}
		text, ok := reply.Parts[0].(message.TextPart)
		if !ok {
			return board, errdefs.Internalf("reply part is %T, want TextPart", reply.Parts[0])
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, text.Text))
		return board, nil
	})
	_, session, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := session.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithAskUserOverride(func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{Parts: []message.Part{
				message.TextPart{Text: "override-reply"},
			}}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted || result.Text() != "override-reply" {
		t.Fatalf("result = %q %q", result.Status, result.Text())
	}
}

func noopTestEngine() agent.Engine {
	return agent.EngineFunc(func(
		_ context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		return board, nil
	})
}

type discardSink struct{}

func (discardSink) OnDelta(
	context.Context,
	event.Envelope,
	agent.StreamDeltaPayload,
) error {
	return nil
}
