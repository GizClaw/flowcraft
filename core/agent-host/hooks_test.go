package host

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/message"
)

// probeHook records what it was built for and what it saw, so the test can check
// both the wiring and the lifecycle.
type probeHook struct {
	mu     sync.Mutex
	agents []string
	turns  []string
	after  []agent.TaskState
}

func (p *probeHook) kind() deploy.HookKind {
	return deploy.HookKind{
		Name: "probe",
		Build: func(_ context.Context, deps deploy.HookDeps) (agent.Hooks, error) {
			p.mu.Lock()
			p.agents = append(p.agents, deps.Agent)
			p.mu.Unlock()
			return agent.Hooks{
				BeforeTurn: func(_ context.Context, turn *agent.Turn) error {
					p.mu.Lock()
					p.turns = append(p.turns, turn.Agent+":"+turn.Message.Content.Text())
					p.mu.Unlock()
					turn.Message = message.NewTextMessage(message.RoleUser, "brief: "+turn.Message.Content.Text())
					return nil
				},
				AfterTurn: func(_ context.Context, turn agent.Turn, result agent.Result) error {
					p.mu.Lock()
					p.after = append(p.after, result.State)
					p.mu.Unlock()
					return nil
				},
			}, nil
		},
	}
}

func (p *probeHook) snapshot() (agents, turns []string, after []agent.TaskState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.agents...), append([]string(nil), p.turns...), append([]agent.TaskState(nil), p.after...)
}

func TestDocumentHooksRunAroundTheTurn(t *testing.T) {
	probe := &probeHook{}
	manager := newManager(t, deploy.BuiltinKinds().
		WithEngines(engineFor("done", func() agent.Runner { return &completedRunner{} })).
		WithHooks(probe.kind()), testHost(t))

	spec := testSpec(t, "seat_5", "done")
	spec.Hooks = []deploy.HookSpec{{Kind: "probe", Settings: json.RawMessage(`{}`)}}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("seat_5", "conv-1")
	if !ok {
		t.Fatal("no handle for a deployed agent")
	}
	run(t, session)

	agents, turns, after := probe.snapshot()
	if len(agents) != 1 {
		// A hook is built once per deployment, not once per session: the session
		// table is the agent's, and the policy belongs to the deployment.
		t.Fatalf("built %d times, want once per deployment", len(agents))
	}
	if agents[0] != "seat_5" {
		t.Fatalf("agent = %q, want the deployment name", agents[0])
	}
	if len(turns) != 1 || turns[0] != "seat_5:go" {
		t.Fatalf("before-turn saw %v", turns)
	}
	if len(after) != 1 || after[0] != agent.StateCompleted {
		t.Fatalf("after-turn saw %v", after)
	}
}

func TestHostHooksRunBeforeTheDocument(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(label string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, label)
	}
	hostHook := func(string) agent.Hooks {
		return agent.Hooks{BeforeTurn: func(context.Context, *agent.Turn) error {
			record("host")
			return nil
		}}
	}
	documentHook := deploy.HookKind{
		Name: "doc",
		Build: func(context.Context, deploy.HookDeps) (agent.Hooks, error) {
			return agent.Hooks{BeforeTurn: func(context.Context, *agent.Turn) error {
				record("document")
				return nil
			}}, nil
		},
	}
	bindings := testHost(t)
	bindings.Hooks = hostHook
	manager := newManager(t, deploy.BuiltinKinds().
		WithEngines(engineFor("done", func() agent.Runner { return &completedRunner{} })).
		WithHooks(documentHook), bindings)

	spec := testSpec(t, "seat_5", "done")
	spec.Hooks = []deploy.HookSpec{{Kind: "doc", Settings: json.RawMessage(`{}`)}}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, _ := manager.Session("seat_5", "conv-1")
	run(t, session)

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "host,document" {
		t.Fatalf("order = %v, want the host's hook first", order)
	}
}
