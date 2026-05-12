package api

// E2E for the daemon HTTP surface of the lifecycle/resume contract.
// Existing api/server_test.go covers each endpoint in isolation
// (Resume validation, NoStore, UnknownVessel) using the noop engine
// stub. None of those tests actually persist a checkpoint and round
// it back through /resume — the audit (internal-docs/contract-audit.md)
// flagged that as a P2 gap because every regression in the
// fleet→captain→agent.Run→Resumer chain that surfaces over HTTP
// (CheckpointStore not threaded into captains, Resume returning
// 503 in production, run_id not echoed back, etc.) was invisible
// from end-to-end tests.
//
// This file closes that gap. The test wires a real sdk/graph/runner
// engine into the catalog, hands the fleet an in-memory
// engine.CheckpointStore via the new fleet.WithCheckpointStore
// option, and drives the /v1/vessels/{id}/call → /v1/vessels/{id}/resume
// → /v1/runs/{run_id} chain over HTTP using net/http/httptest.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/fleet"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
)

const resumeAPIConfig = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-default
spec:
  control:
    socket: /tmp/v.sock
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: support
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: helper
spec:
  engine:
    ref: graph-runner-test
`

// memCheckpointStore is the simplest engine.CheckpointStore: a map
// keyed by ExecID. We define it here too (rather than reuse vessel's
// equivalent) because that one lives in vessel_test and importing
// across test packages would couple the api binary to vessel test
// helpers. Keep the duplicate small + obvious.
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

func (s *memCheckpointStore) snapshot() map[string]engine.Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]engine.Checkpoint, len(s.by))
	for k, v := range s.by {
		out[k] = v
	}
	return out
}

// resumeStepNode mimics the runner-side step node used by other
// lifecycle E2E tests: a closure-driven node so the test can wire
// per-invocation behaviour (interrupt-once, record visit) without
// ad-hoc node types.
type resumeStepNode struct {
	id  string
	run func(graph.ExecutionContext, *graph.Board) error
}

func (n resumeStepNode) ID() string   { return n.id }
func (n resumeStepNode) Type() string { return "step" }
func (n resumeStepNode) ExecuteBoard(ctx graph.ExecutionContext, b *graph.Board) error {
	return n.run(ctx, b)
}

// newResumeTestServer is a parallel of newTestServer (server_test.go)
// that wires:
//   - a real graph runner engine into the catalog under ref
//     "graph-runner-test"; the graph is A→B→C with B interrupting
//     on its first call (engine.Interrupted) and succeeding
//     thereafter.
//   - an in-memory CheckpointStore via the fleet.Build option, so
//     the captain Captain.Resume path actually has somewhere to
//     load from.
//
// Returns the server, the underlying store, and per-node visit
// recorders so the test can assert on the cross-resume execution
// trace.
func newResumeTestServer(t *testing.T) (*Server, *memCheckpointStore, *visitCounter) {
	t.Helper()

	objs, err := apispec.DecodeAll(strings.NewReader(resumeAPIConfig), "<test>")
	if err != nil {
		t.Fatal(err)
	}

	visits := newVisitCounter()
	var (
		bMu          sync.Mutex
		bInterrupted bool
	)

	cat := catalog.New()
	cat.RegisterEngine("graph-runner-test", func(_ string, _ map[string]any, _ catalog.Deps) (engine.Engine, error) {
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
		def := &graph.GraphDefinition{
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
		return runner.New(def, factory)
	})

	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs)
	}

	store := newMemCheckpointStore()
	f, err := fleet.Build(*plan, fleet.WithCheckpointStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background()) })

	s := New(Config{Version: "test"}, f)
	s.MarkReady()
	return s, store, visits
}

// TestAPI_ResumeRoundTrip_HTTPLifecycle drives the full
//
//	POST /v1/vessels/{id}/call          → status=interrupted
//	POST /v1/vessels/{id}/resume        → 202 Accepted
//	GET  /v1/runs/{run_id}              → poll until state=completed
//
// chain over HTTP using a real sdk/graph/runner engine and an
// in-memory CheckpointStore. Asserts:
//
//   - /call surfaces StatusInterrupted in its JSON response when
//     the engine returns engine.Interrupted partway.
//   - the CheckpointStore captured a cp at Step=A (the only node
//     that completed before the interrupt).
//   - /resume returns 202 + the same run_id (per the route's
//     wire-form contract).
//   - Polling /v1/runs/{run_id} eventually sees state="completed",
//     i.e. the resume actually finished the rest of the graph
//     through the daemon's runs registry.
//   - The execution trace shows A ran exactly once (NOT
//     re-executed by resume), B's success body ran exactly once,
//     C ran exactly once.
//
// This is the test that would have failed BEFORE the fleet.Build
// CheckpointStore option existed (resume would return 503), and
// would also fail if the daemon stopped passing the store down
// into captains, if the resume route lost its run_id in transit,
// or if the runs registry stopped tracking resumed runs.
func TestAPI_ResumeRoundTrip_HTTPLifecycle(t *testing.T) {
	t.Parallel()

	s, store, visits := newResumeTestServer(t)

	// Phase 1: synchronous /call. The graph interrupts on B so we
	// expect StatusInterrupted in the JSON body. submitBody on this
	// route does not carry run_id, so the daemon mints a fresh one
	// and echoes it back; we read it to drive the subsequent
	// /resume request against the same run.
	callResp := httpDo(t, s, http.MethodPost, "/v1/vessels/support/call",
		`{"agent":"helper","query":"please draft"}`)
	if callResp.code != http.StatusOK {
		t.Fatalf("/call status = %d body=%s, want 200", callResp.code, callResp.body)
	}
	var callBody struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(callResp.body), &callBody); err != nil {
		t.Fatalf("/call decode: %v body=%s", err, callResp.body)
	}
	if callBody.Status != "interrupted" {
		t.Fatalf("/call status = %q, want interrupted (engine returns engine.Interrupted on first B)", callBody.Status)
	}
	runID := callBody.RunID
	if runID == "" {
		t.Fatal("/call response missing run_id; cannot drive /resume")
	}

	if got := visits.count("A"); got != 1 {
		t.Fatalf("phase 1: A executed %d times, want 1", got)
	}
	if got := visits.count("B"); got != 0 {
		t.Fatalf("phase 1: B success body ran %d times, want 0 (interrupted)", got)
	}

	// Phase 2: verify a checkpoint actually landed in the store.
	saved := store.snapshot()
	cp, ok := findCheckpointAtStep(saved, "A")
	if !ok {
		t.Fatalf("no checkpoint at Step=A in store; entries=%v — fleet.WithCheckpointStore wiring broken", checkpointSteps(saved))
	}
	if cp.Attributes["vessel.agent_name"] != "helper" {
		t.Fatalf("cp missing vessel.agent_name=helper attribute; got %q", cp.Attributes["vessel.agent_name"])
	}

	// Phase 3: async /resume. Should return 202 + the same run_id.
	resumeResp := httpDo(t, s, http.MethodPost, "/v1/vessels/support/resume",
		`{"run_id":"`+runID+`"}`)
	if resumeResp.code != http.StatusAccepted {
		t.Fatalf("/resume status = %d body=%s, want 202", resumeResp.code, resumeResp.body)
	}
	var resumeBody struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(resumeResp.body), &resumeBody); err != nil {
		t.Fatalf("/resume decode: %v body=%s", err, resumeResp.body)
	}
	if resumeBody.RunID != runID {
		t.Fatalf("/resume returned run_id=%q, want %q (resume must keep the original run_id stable)", resumeBody.RunID, runID)
	}

	// Phase 4: poll /v1/runs/{run_id} until the resumed run ends.
	// Bound the wait so a hung resume surfaces as a test failure
	// rather than a hung CI.
	deadline := time.Now().Add(2 * time.Second)
	var lastState string
	for time.Now().Before(deadline) {
		statusResp := httpDo(t, s, http.MethodGet, "/v1/runs/"+runID, "")
		if statusResp.code != http.StatusOK {
			t.Fatalf("/v1/runs/%s status = %d body=%s", runID, statusResp.code, statusResp.body)
		}
		var statusBody struct {
			RunID string `json:"run_id"`
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(statusResp.body), &statusBody); err != nil {
			t.Fatalf("/v1/runs decode: %v body=%s", err, statusResp.body)
		}
		lastState = statusBody.State
		if statusBody.State == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastState != "completed" {
		t.Fatalf("resumed run never reached state=completed (last seen %q); /resume → registry chain broken", lastState)
	}

	// Phase 5: cross-resume execution invariants.
	if got := visits.count("A"); got != 1 {
		t.Errorf("after resume: A executed %d times, want 1 (resume re-executed completed upstream)", got)
	}
	if got := visits.count("B"); got != 1 {
		t.Errorf("after resume: B success body ran %d times, want 1", got)
	}
	if got := visits.count("C"); got != 1 {
		t.Errorf("after resume: C executed %d times, want 1", got)
	}
}

// httpDo executes a single request against the server, captures
// the response code + body string, and t.Fatals if the recorder
// reports an internal panic. Centralised so each test phase reads
// linearly without recorder boilerplate.
func httpDo(t *testing.T, s *Server, method, path, body string) httpResp {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	s.Handler().ServeHTTP(w, req)
	return httpResp{code: w.Code, body: w.Body.String()}
}

type httpResp struct {
	code int
	body string
}

// findCheckpointAtStep looks for the first checkpoint matching the
// given Step in the store's snapshot. Returns ok=false when none
// match — tests use that to assert "the executor did persist after
// node A".
func findCheckpointAtStep(store map[string]engine.Checkpoint, step string) (engine.Checkpoint, bool) {
	for _, cp := range store {
		if cp.Step == step {
			return cp, true
		}
	}
	return engine.Checkpoint{}, false
}

func checkpointSteps(store map[string]engine.Checkpoint) []string {
	out := make([]string, 0, len(store))
	for _, cp := range store {
		out = append(out, cp.Step)
	}
	return out
}

// visitCounter is a per-node hit counter shared by lifecycle tests.
// Tests inspect it after each phase to assert which nodes
// executed how many times.
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
