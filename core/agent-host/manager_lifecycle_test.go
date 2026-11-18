package host

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
	"github.com/GizClaw/flowcraft/core/message"
)

// The loader a manager hands out is wired to the same kinds it deploys, so a
// kind cannot be loadable and not deployable.
func TestManagerLoaderSharesTheKinds(t *testing.T) {
	manager := newManager(t, builtin.Kinds(), testHost(t))
	if _, ok := manager.Loader().Kinds.Engines["a2a"]; !ok {
		t.Error("the loader does not know the manager's engines")
	}
}

// A zero deploy.Kinds registers nothing, and New does not invent a table to
// replace it: a manager handed no kinds offers no kinds, which is what makes the
// registration an import the host makes rather than a default it inherits.
func TestZeroValueKindsRegistersNothing(t *testing.T) {
	manager := newManager(t, deploy.Kinds{}, testHost(t))
	if names := manager.EngineNames(); len(names) != 0 {
		t.Errorf("engines = %v, want none", names)
	}
	// The other half of the same contract: a document naming a kind this
	// process never registered fails to deploy, and the error says what was
	// offered. Only the import can widen the table, which is the point.
	err := manager.Add(context.Background(), testSpec(t, "graphless", "graph"))
	if err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("Add = %v, want an unregistered-kind complaint", err)
	}
}

func TestReplaceOnAnUnknownAgentIsRefused(t *testing.T) {
	manager := newManager(t, builtin.Kinds(), testHost(t))
	if err := manager.Replace(context.Background(), testSpec(t, "absent", "a2a")); err == nil {
		t.Fatal("Replace created an agent that was never deployed")
	}
}

// Replace builds the new deployment before touching the old one, so a spec that
// cannot be built leaves the running agent exactly as it was.
func TestReplaceKeepsTheOldAgentWhenTheNewSpecIsBad(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "still here"}
	}))
	manager := newManager(t, kinds, testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "echo")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := manager.Replace(context.Background(), testSpec(t, "a", "missing")); err == nil {
		t.Fatal("a replacement with an unregistered engine was accepted")
	}
	if names := manager.Names(); len(names) != 1 || names[0] != "a" {
		t.Fatalf("agents = %v, want the old one still deployed", names)
	}
	session, ok := manager.Session("a", "conv-1")
	if !ok {
		t.Fatal("the old agent has no session after a failed Replace")
	}
	for _, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run after failed Replace: %v", err)
		}
	}
}

// An agent with no protocol is deployed but not exposed: the handler is nil,
// and the bool says the agent exists.
func TestHandlerIsNilForAnUnexposedAgent(t *testing.T) {
	kinds := builtin.Kinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hi"}
	}))
	manager := newManager(t, kinds, testHost(t))
	if err := manager.Add(context.Background(), testSpec(t, "a", "echo")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	handler, ok := manager.Handler("a")
	if !ok {
		t.Fatal("the agent was not found")
	}
	if handler != nil {
		t.Error("an agent without a protocol serves a handler")
	}
	if _, ok := manager.Handler("absent"); ok {
		t.Error("an unknown agent has a handler")
	}
}

// The host sees every finished run named by agent, so one exporter can serve a
// multi-agent host.
func TestHostOnRunNamesTheAgent(t *testing.T) {
	var names []string
	host := testHost(t)
	host.OnRun = func(name string, record agent.RunRecord) {
		names = append(names, name)
	}
	kinds := builtin.Kinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hi"}
	}))
	manager := newManager(t, kinds, host)
	if err := manager.Add(context.Background(), testSpec(t, "a", "echo")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("a", "conv-1")
	if !ok {
		t.Fatal("no session")
	}
	for _, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if len(names) != 1 || names[0] != "a" {
		t.Fatalf("OnRun = %v, want the agent name", names)
	}
}
