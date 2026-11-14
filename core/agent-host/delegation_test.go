package host

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
)

func TestManagerInjectsDelegationTools(t *testing.T) {
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hello"}
	})), testHost(t))
	spec := testSpec(t, "a", "echo")
	spec.Delegation = &deploy.DelegationSpec{Targets: []string{"worker"}}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deployed := manager.agents["a"]
	if deployed == nil || deployed.rt == nil {
		t.Fatal("deployment has no runtime")
	}
	names := deployed.rt.Tools().Names()
	for _, want := range []string{"delegate", "delegation_targets"} {
		if !containsName(names, want) {
			t.Errorf("tools = %v, want %s", names, want)
		}
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestManagerRejectsDelegationOnARemoteAgent(t *testing.T) {
	remote := deploy.EngineKind{
		Name:  "remote",
		Local: false,
		Build: func(context.Context, deploy.EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) {
				return &textRunner{}, nil
			}, nil
		},
	}
	manager := newManager(t, deploy.BuiltinKinds().WithEngines(remote), testHost(t))
	spec := deploy.Spec{
		Name:       "peer",
		Engine:     deploy.EngineSpec{Kind: "remote"},
		Delegation: &deploy.DelegationSpec{},
	}
	if err := manager.Add(context.Background(), spec); err == nil {
		t.Fatal("Add accepted delegation on a remote agent")
	}
}
