package vessel_test

// This file holds the cross-package E2E tests for the Resume lifecycle.
// It lives in vessel_test (external) — not the internal vessel
// package — so that:
//
//   - It can import sdk/graph/runner without pulling a runner-side
//     dependency back into the production vessel package.
//   - It exercises the Captain through its public surface (Submit /
//     Resume / Wait), the same API consumers use.
//
// The audit that motivated this PR (internal-docs/contract-audit.md)
// flagged that the previous quality suite covered only happy-path
// chat scenarios — every lifecycle promise (resume, interrupt,
// capabilities gating, ask-user) was exercised by unit tests with
// hand-rolled stubs but never end-to-end against a real engine. These
// tests close that gap for Resume + Capabilities gating.

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestResume_RoundTrip_GraphRunner is the headline E2E for the
// Captain → agent.Run → graph runner Resumer chain. It wires a real
// graph runner (3 sequential nodes A→B→C) into a Captain backed by an
// in-memory engine.CheckpointStore. The middle node B trips an
// engine.Interrupted on its FIRST execution; the executor saves
// cp(Step=A) right before invoking B, returns the interrupt, agent.Run
// classifies it as StatusInterrupted. We then call Captain.Resume(runID)
// — this must load cp(Step=A), dispatch the same agent with
// engine.Run.ResumeFrom set, and the runner must continue from the
// downstream of A: B (now no longer tripping) then C.
//
// The test asserts node-execution counts: A ran once total (NOT
// re-executed on resume), B's success body ran once (the interrupt
// branch did not record), C ran once. Any silent regression in the
// resume path — store wiring dropped, ResumeFrom not propagated,
// runner restarting from entry, executor double-counting A — surfaces
// here as a counter mismatch.
func TestResume_RoundTrip_GraphRunner(t *testing.T) {
	t.Parallel()

	const (
		runID         = "resume-roundtrip-1"
		agentName     = "primary"
		agentAttrName = "vessel.agent_name"
	)

	var (
		bMu          sync.Mutex
		bInterrupted bool
		visits       = newVisitCounter()
	)

	factory := node.NewFactory()
	factory.RegisterBuilder("step", func(def graph.NodeDefinition) (graph.Node, error) {
		id := def.ID
		return resumeStepNode{
			id: id,
			run: func(_ graph.ExecutionContext, _ *graph.Board) error {
				if id == "B" {
					bMu.Lock()
					alreadyInterrupted := bInterrupted
					bInterrupted = true
					bMu.Unlock()
					if !alreadyInterrupted {
						return engine.Interrupted(engine.Interrupt{
							Cause:  engine.CauseHostShutdown,
							Detail: "test pause before B",
						})
					}
				}
				visits.record(id)
				return nil
			},
		}, nil
	})

	graphDef := &graph.GraphDefinition{
		Name:  "abc",
		Entry: "A",
		Nodes: []graph.NodeDefinition{
			{ID: "A", Type: "step"},
			{ID: "B", Type: "step"},
			{ID: "C", Type: "step"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "A", To: "B"},
			{From: "B", To: "C"},
			{From: "C", To: graph.END},
		},
	}

	store := newMemCheckpointStore()
	captain := launchedCaptain(t, spec.Spec{
		ID:     "v-resume",
		Agents: []spec.Agent{{Name: agentName}},
	},
		vessel.WithEngineFactory(func(_ spec.Agent, _ vessel.Deps) (engine.Engine, error) {
			return runner.New(graphDef, factory)
		}),
		vessel.WithCheckpointStore(store),
	)

	res1, err := captain.Call(context.Background(), agentName, agent.Request{
		RunID:   runID,
		Message: model.NewTextMessage(model.RoleUser, "go"),
	})
	if err != nil {
		t.Fatalf("phase 1 Call: %v", err)
	}
	if res1.Status != agent.StatusInterrupted {
		t.Fatalf("phase 1 status = %q, want StatusInterrupted (engine returned engine.Interrupted)", res1.Status)
	}

	if got := visits.count("A"); got != 1 {
		t.Fatalf("phase 1: A executed %d times, want 1", got)
	}
	if got := visits.count("B"); got != 0 {
		t.Fatalf("phase 1: B success body ran %d times, want 0 (B should have been interrupted)", got)
	}
	if got := visits.count("C"); got != 0 {
		t.Fatalf("phase 1: C executed %d times, want 0 (never reached because B interrupted)", got)
	}

	saved, _ := store.Load(context.Background(), runID)
	if saved == nil {
		t.Fatal("no checkpoint persisted under runID; resume cannot proceed")
	}
	if saved.Step != "A" {
		t.Fatalf("latest checkpoint Step = %q, want %q (only A completed before B interrupted)", saved.Step, "A")
	}
	if got := saved.Attributes[agentAttrName]; got != agentName {
		t.Fatalf("checkpoint missing %q=%q (got %q); Captain.Resume cannot route", agentAttrName, agentName, got)
	}

	h, err := captain.Resume(context.Background(), runID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	res2, err := h.Wait(context.Background())
	if err != nil {
		t.Fatalf("Resume Wait: %v", err)
	}
	if res2.Status != agent.StatusCompleted {
		t.Fatalf("phase 2 status = %q, want StatusCompleted", res2.Status)
	}

	if got := visits.count("A"); got != 1 {
		t.Errorf("after resume: A executed %d times, want 1 (resume must NOT re-execute completed upstream)", got)
	}
	if got := visits.count("B"); got != 1 {
		t.Errorf("after resume: B success body ran %d times, want 1", got)
	}
	if got := visits.count("C"); got != 1 {
		t.Errorf("after resume: C executed %d times, want 1", got)
	}
}

// TestResume_E2E_NoStoreReturnsNotAvailable asserts the Captain
// refuses Resume when no CheckpointStore has been wired. The same
// property is asserted by an internal vessel package unit test; we
// duplicate at the external-package layer because a regression that
// quietly degraded Resume into "fresh run" would be invisible to
// consumers — and consumers go through the same vessel_test surface.
func TestResume_E2E_NoStoreReturnsNotAvailable(t *testing.T) {
	t.Parallel()
	captain := launchedCaptain(t, spec.Spec{
		ID:     "v-resume-nostore",
		Agents: []spec.Agent{{Name: "primary"}},
	},
		vessel.WithEngine(engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		})),
	)

	if _, err := captain.Resume(context.Background(), "any"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Resume without store: want NotAvailable, got %v", err)
	}
}

// TestResume_CapabilitiesGating_RejectsResumeOnNonResumerEngine asserts
// the engine-side admission contract: when the underlying engine does
// not implement engine.Resumer, the Captain still routes the resume
// dispatch (it is a runtime decision, not a static one), but the
// engine MUST surface the mismatch as a failed run rather than
// silently dispatching a fresh execution.
//
// This catches a real regression class — if a future refactor decides
// to "be lenient" and silently drop ResumeFrom for engines that don't
// claim SupportsResume, callers would think their resume succeeded
// while actually losing the partial work the checkpoint represented.
// That is far more dangerous than a loud failure, hence the test.
func TestResume_CapabilitiesGating_RejectsResumeOnNonResumerEngine(t *testing.T) {
	t.Parallel()

	const (
		runID         = "gating-run"
		agentName     = "primary"
		agentAttrName = "vessel.agent_name"
	)

	store := newMemCheckpointStore()
	if err := store.Save(context.Background(), engine.Checkpoint{
		ExecID: runID,
		Step:   "anything",
		Attributes: map[string]string{
			agentAttrName: agentName,
		},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var sawResumeFrom bool
	bareEngine := engine.EngineFunc(func(_ context.Context, r engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		if r.ResumeFrom != nil {
			sawResumeFrom = true
			return b, errdefs.NotAvailablef("test engine: resume not supported (cp=%s)", r.ResumeFrom.Step)
		}
		return b, nil
	})

	captain := launchedCaptain(t, spec.Spec{
		ID:     "v-caps-gating",
		Agents: []spec.Agent{{Name: agentName}},
	},
		vessel.WithEngine(bareEngine),
		vessel.WithCheckpointStore(store),
	)

	h, err := captain.Resume(context.Background(), runID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	res, _ := h.Wait(context.Background())

	if !sawResumeFrom {
		t.Fatal("engine never observed ResumeFrom; vessel/agent dropped the resume payload silently")
	}
	if res.Status == agent.StatusCompleted {
		t.Fatalf("status = StatusCompleted; engines that reject resume must surface a non-OK status (got %+v)", res)
	}
}

// resumeStepNode is a tiny graph.Node shim used only by the resume E2E
// test. We keep it local so the quality test does not depend on
// internal-test fixtures of the runner package.
type resumeStepNode struct {
	id  string
	run func(graph.ExecutionContext, *graph.Board) error
}

func (n resumeStepNode) ID() string   { return n.id }
func (n resumeStepNode) Type() string { return "step" }
func (n resumeStepNode) ExecuteBoard(ctx graph.ExecutionContext, b *graph.Board) error {
	return n.run(ctx, b)
}

// memCheckpointStore is the simplest engine.CheckpointStore: a
// concurrent map keyed by ExecID. Real deployments use durable
// stores; the e2e test only needs round-trip semantics.
type memCheckpointStore struct {
	mu sync.Mutex
	by map[string]engine.Checkpoint
}

func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{by: make(map[string]engine.Checkpoint)}
}

func (s *memCheckpointStore) Save(_ context.Context, cp engine.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by[cp.ExecID] = cp
	return nil
}

func (s *memCheckpointStore) Load(_ context.Context, execID string) (*engine.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.by[execID]
	if !ok {
		return nil, nil
	}
	c := cp
	return &c, nil
}

// visitCounter is a per-node hit counter used by resume / revise
// E2E tests to assert which nodes (or attempts) actually executed.
type visitCounter struct {
	mu     sync.Mutex
	visits map[string]int
}

func newVisitCounter() *visitCounter { return &visitCounter{visits: map[string]int{}} }

func (v *visitCounter) record(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.visits[id]++
}

func (v *visitCounter) count(id string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.visits[id]
}

// launchedCaptain mirrors the tests/quality/vessel helper of the same
// name: build a Captain, Launch, register Stop on cleanup. Kept local
// to vessel_test so the in-workspace E2E tests don't depend on the
// out-of-workspace quality suite (which is pinned to a released tag
// and cannot import new APIs).
func launchedCaptain(t *testing.T, vs spec.Spec, opts ...vessel.Option) *vessel.Captain {
	t.Helper()
	c, err := vessel.New(vs, opts...)
	if err != nil {
		t.Fatalf("vessel.New: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Stop(context.Background())
	})
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("vessel.Launch: %v", err)
	}
	return c
}
