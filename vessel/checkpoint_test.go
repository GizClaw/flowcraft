package vessel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// recordingStore captures every Save call so the test can assert
// the sandbox host actually routed the engine's Checkpoint emission
// to the persistence backend wired via WithCheckpointStore.
type recordingStore struct {
	mu     sync.Mutex
	saves  []engine.Checkpoint
	saveOK bool
	loadCP *engine.Checkpoint
}

func (s *recordingStore) Save(_ context.Context, cp engine.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.saveOK {
		return errors.New("recordingStore: save disabled")
	}
	s.saves = append(s.saves, cp)
	return nil
}

func (s *recordingStore) Load(_ context.Context, _ string) (*engine.Checkpoint, error) {
	return s.loadCP, nil
}

// TestCheckpoint_SandboxRoutesToStore: an engine that calls
// host.Checkpoint must persist through the wired CheckpointStore.
// Without the WithCheckpointStore plumbing the call would silently
// hit the default NoopHost and the durability promise on
// WithCheckpointStore would be a lie.
func TestCheckpoint_SandboxRoutesToStore(t *testing.T) {
	t.Parallel()
	store := &recordingStore{saveOK: true}

	want := engine.Checkpoint{ExecID: "run-x", Step: "step-3", Timestamp: time.Now().UTC()}
	emitter := engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		if err := h.Checkpoint(ctx, want); err != nil {
			return b, err
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, err := New(vs, WithEngine(emitter), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	res, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status=%v", res.Status)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 1 {
		t.Fatalf("got %d saves, want 1", len(store.saves))
	}
	if got := store.saves[0]; got.ExecID != want.ExecID || got.Step != want.Step {
		t.Fatalf("checkpoint mismatch: got %+v want %+v", got, want)
	}
}

// TestCheckpoint_SaveErrorPropagates: when the store returns an
// error from Save, the engine sees it. Telemetry-only Checkpointers
// MUST NOT mask a real persistence failure; engines decide the
// retry / fail policy.
func TestCheckpoint_SaveErrorPropagates(t *testing.T) {
	t.Parallel()
	store := &recordingStore{saveOK: false} // every Save returns error

	var observed error
	probe := engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		observed = h.Checkpoint(ctx, engine.Checkpoint{ExecID: "x", Step: "n"})
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, _ := New(vs, WithEngine(probe), WithCheckpointStore(store))
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if _, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")}); err != nil {
		t.Fatal(err)
	}
	if observed == nil {
		t.Fatal("Save error did not surface to engine")
	}
}

// TestCheckpoint_DepsExposesStore: factories that need to call
// Load (resume) must be able to reach the store via Deps. Without
// this hook, the only way to read prior state would be to capture
// the store in a closure, defeating the EngineFactory abstraction.
func TestCheckpoint_DepsExposesStore(t *testing.T) {
	t.Parallel()
	store := &recordingStore{saveOK: true, loadCP: &engine.Checkpoint{ExecID: "prior"}}

	var depsStore engine.CheckpointStore
	factory := EngineFactory(func(_ spec.Agent, deps Deps) (engine.Engine, error) {
		depsStore = deps.CheckpointStore
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		}), nil
	})

	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	if _, err := New(vs, WithEngineFactory(factory), WithCheckpointStore(store)); err != nil {
		t.Fatal(err)
	}
	if depsStore == nil {
		t.Fatal("Deps.CheckpointStore not propagated")
	}
	got, _ := depsStore.Load(context.Background(), "prior")
	if got == nil || got.ExecID != "prior" {
		t.Fatalf("Load did not round-trip: %+v", got)
	}
}
