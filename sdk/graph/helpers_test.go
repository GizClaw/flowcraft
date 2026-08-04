package graph

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// echoCfg is the config of the "echo" test node type.
type echoCfg struct {
	// SetVar, when set, makes the node write SetVal into this board
	// var. Doubles as the ConfigKey for the node's write role.
	SetVar string `json:"set_var,omitempty"`
	SetVal any    `json:"set_val,omitempty"`

	// Message, when set, is appended to the main channel as an
	// assistant message.
	Message string `json:"message,omitempty"`

	// Fail makes the handler return this error.
	Fail string `json:"fail,omitempty"`
}

// echoNode returns a NodeType that writes vars / messages per config.
// failsBeforeSuccess, when non-nil, makes the handler error that many
// times before succeeding (retry testing).
func echoNode(failsBeforeSuccess *atomic.Int32) NodeType[echoCfg] {
	return NodeType[echoCfg]{
		Meta: Meta{
			Desc: "test echo node",
			Writes: []Role{
				{Kind: RoleVar, ConfigKey: "set_var"},
			},
		},
		Handler: func(ec ExecutionContext, board *agent.Board, cfg echoCfg) error {
			if failsBeforeSuccess != nil && failsBeforeSuccess.Load() > 0 {
				failsBeforeSuccess.Add(-1)
				return errors.New("transient boom")
			}
			if cfg.Fail != "" {
				return errors.New(cfg.Fail)
			}
			if cfg.SetVar != "" {
				board.SetVar(cfg.SetVar, cfg.SetVal)
			}
			if cfg.Message != "" {
				board.AppendChannelMessage(agent.MainChannel,
					inference.NewTextMessage(inference.RoleAssistant, cfg.Message))
			}
			return nil
		},
	}
}

// newTestRegistry registers the echo type; extra types may be added by
// the caller.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterType(reg, "echo", echoNode(nil)); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	return reg
}

// mustBuild builds or fails the test.
func mustBuild(t *testing.T, def *GraphDefinition, reg *Registry, opts ...BuildOption) *Graph {
	t.Helper()
	g, err := Build(def, reg, opts...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

// mustRun executes a fresh run or fails the test.
func mustRun(t *testing.T, g *Graph, board *agent.Board) *agent.Board {
	t.Helper()
	out, err := g.Execute(context.Background(), testRun(), agent.NoopHost{}, board)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func testRun() agent.Run {
	return agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}}
}

// TestAmbientRunInfo proves the executor injects the run identity
// into the handler's context: a handler deep in the call tree can
// pull it out via agent.RunInfoFromContext without any field wiring.
func TestAmbientRunInfo(t *testing.T) {
	reg := NewRegistry()
	var got agent.RunInfo
	var ok bool
	err := RegisterType(reg, "probe", NodeType[struct{}]{
		Meta: Meta{Desc: "ambient identity probe"},
		Handler: func(ec ExecutionContext, _ *agent.Board, _ struct{}) error {
			got, ok = agent.RunInfoFromContext(ec.Context)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register probe: %v", err)
	}
	g := mustBuild(t, &GraphDefinition{
		Name:  "probe-graph",
		Entry: "p",
		Nodes: []NodeDefinition{{ID: "p", Type: "probe"}},
	}, reg)
	mustRun(t, g, agent.NewBoard())

	if !ok {
		t.Fatal("handler context carried no RunInfo")
	}
	if got.AgentID != "test-agent" || got.RunID != "run-1" {
		t.Fatalf("ambient identity = %+v", got.Identity)
	}
}

// checkpointHost records stamped checkpoints.
type checkpointHost struct {
	agent.NoopHost
	cps []agent.Checkpoint
}

func (h *checkpointHost) Checkpoint(_ context.Context, cp agent.Checkpoint) error {
	h.cps = append(h.cps, cp)
	return nil
}

// publishHost records published envelopes.
type publishHost struct {
	agent.NoopHost
	mu   sync.Mutex
	envs []event.Envelope
}

type runEndFailHost struct {
	publishHost
}

func (h *runEndFailHost) Publish(ctx context.Context, env event.Envelope) error {
	if env.Subject == agent.SubjectRunEnd(env.RunID()) {
		return errors.New("run-end unavailable")
	}
	return h.publishHost.Publish(ctx, env)
}

func (h *publishHost) Publish(_ context.Context, env event.Envelope) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.envs = append(h.envs, env)
	return nil
}

// subjectsOf returns the recorded envelope subjects in publish order.
func (h *publishHost) subjectsOf() []event.Subject {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]event.Subject, len(h.envs))
	for i, env := range h.envs {
		out[i] = env.Subject
	}
	return out
}

// decodePayloads unmarshals the payloads of envelopes whose subject
// matches want, in publish order.
func decodePayloads[T any](t *testing.T, h *publishHost, want event.Subject) []T {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []T
	for _, env := range h.envs {
		if env.Subject != want {
			continue
		}
		var p T
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// interruptHost fires a cooperative interrupt after limit calls to
// Interrupts.
type interruptHost struct {
	agent.NoopHost
	ch chan agent.Interrupt
}

func newInterruptHost() *interruptHost {
	ch := make(chan agent.Interrupt, 1)
	ch <- agent.Interrupt{Detail: "stop requested"}
	return &interruptHost{ch: ch}
}

func (h *interruptHost) Interrupts() <-chan agent.Interrupt { return h.ch }
