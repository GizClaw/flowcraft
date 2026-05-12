package vessel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
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

// TestResume_RoutesByCheckpointAttribute: a Save stamps
// vessel.agent_name on cp.Attributes; Resume(runID) reads it back
// to dispatch the original agent with WithResumeFrom set.
func TestResume_RoutesByCheckpointAttribute(t *testing.T) {
	t.Parallel()
	store := &recordingStore{saveOK: true}

	var (
		mu          sync.Mutex
		dispatches  []dispatchRecord
		resumeRunID = "resumed-run-77"
	)
	type runRecord struct {
		runID  string
		resume *engine.Checkpoint
	}
	_ = runRecord{}

	emitter := engine.EngineFunc(func(ctx context.Context, r engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		mu.Lock()
		dispatches = append(dispatches, dispatchRecord{runID: r.ID, resume: r.ResumeFrom})
		mu.Unlock()
		// On the first (fresh) call: emit a checkpoint so the
		// resume call has something to load. The store will
		// remember it via loadCP for the subsequent Load.
		if r.ResumeFrom == nil {
			cp := engine.Checkpoint{ExecID: r.ID, Step: "step-after-A", Timestamp: time.Now().UTC()}
			if err := h.Checkpoint(ctx, cp); err != nil {
				return b, err
			}
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{Agents: []spec.Agent{{Name: "worker"}}}
	c, err := New(vs, WithEngine(emitter), WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	// Phase 1: fresh run that emits a checkpoint. Use an explicit
	// RunID so the subsequent Resume can address it directly.
	res, err := c.Call(context.Background(), "worker",
		agent.Request{RunID: resumeRunID, Message: model.NewTextMessage(model.RoleUser, "go")})
	if err != nil {
		t.Fatalf("phase 1 Call: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("phase 1 status=%v", res.Status)
	}

	store.mu.Lock()
	if len(store.saves) != 1 {
		store.mu.Unlock()
		t.Fatalf("phase 1 expected 1 save, got %d", len(store.saves))
	}
	saved := store.saves[0]
	store.mu.Unlock()

	if got := saved.Attributes[checkpointAttrAgentName]; got != "worker" {
		t.Fatalf("checkpoint missing %s attribute (got %q); Resume cannot route", checkpointAttrAgentName, got)
	}

	// Make Load return the saved checkpoint so Resume can find it.
	store.mu.Lock()
	cpCopy := saved
	store.loadCP = &cpCopy
	store.mu.Unlock()

	// Phase 2: Resume by runID. The agent should be re-dispatched
	// with engine.Run.ResumeFrom set to the loaded checkpoint.
	h, err := c.Resume(context.Background(), resumeRunID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Resume Wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dispatches) != 2 {
		t.Fatalf("expected 2 dispatches (fresh + resume), got %d", len(dispatches))
	}
	resumeCall := dispatches[1]
	if resumeCall.resume == nil {
		t.Fatal("resume dispatch had no ResumeFrom set")
	}
	if resumeCall.runID != resumeRunID {
		t.Errorf("resume Run.ID = %q, want %q (must equal cp.ExecID)", resumeCall.runID, resumeRunID)
	}
	if resumeCall.resume.ExecID != resumeRunID {
		t.Errorf("resume cp.ExecID = %q, want %q", resumeCall.resume.ExecID, resumeRunID)
	}
}

type dispatchRecord struct {
	runID  string
	resume *engine.Checkpoint
}

// TestResume_NoStoreReturnsNotAvailable: the durability promise of
// Resume requires a CheckpointStore. Without one, returning
// NotAvailable up-front is the correct contract — silently
// degrading to "fresh run" would surprise callers who explicitly
// asked to resume.
func TestResume_NoStoreReturnsNotAvailable(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, err := New(vs,
		WithEngine(engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if _, err := c.Resume(context.Background(), "any"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Resume without store: want NotAvailable, got %v", err)
	}
}

// TestResume_MissingCheckpointReturnsNotFound: a runID with no
// stored checkpoint is a NotFound, not NotAvailable — the engine
// is wired correctly, the lookup just turned up empty.
func TestResume_MissingCheckpointReturnsNotFound(t *testing.T) {
	t.Parallel()
	store := &recordingStore{saveOK: true} // loadCP nil = nothing stored

	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, _ := New(vs,
		WithEngine(engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		})),
		WithCheckpointStore(store))
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if _, err := c.Resume(context.Background(), "missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("Resume missing cp: want NotFound, got %v", err)
	}
}

// TestResume_UnknownAgentReturnsNotFound: a checkpoint that names an
// agent the current vessel topology no longer hosts must surface
// NotFound rather than silently dispatch to some other agent.
func TestResume_UnknownAgentReturnsNotFound(t *testing.T) {
	t.Parallel()
	store := &recordingStore{
		saveOK: true,
		loadCP: &engine.Checkpoint{
			ExecID:     "r1",
			Step:       "s",
			Attributes: map[string]string{checkpointAttrAgentName: "ghost"},
		},
	}
	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, _ := New(vs,
		WithEngine(engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		})),
		WithCheckpointStore(store))
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if _, err := c.Resume(context.Background(), "r1"); !errdefs.IsNotFound(err) {
		t.Fatalf("Resume against drifted topology: want NotFound, got %v", err)
	}
}
