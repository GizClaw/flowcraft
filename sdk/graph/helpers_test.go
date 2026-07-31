package graph

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
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

// checkpointHost records stamped checkpoints.
type checkpointHost struct {
	agent.NoopHost
	cps []agent.Checkpoint
}

func (h *checkpointHost) Checkpoint(_ context.Context, cp agent.Checkpoint) error {
	h.cps = append(h.cps, cp)
	return nil
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
