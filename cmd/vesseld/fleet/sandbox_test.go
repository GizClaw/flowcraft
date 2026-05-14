package fleet

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
	exectool "github.com/GizClaw/flowcraft/sdkx/tool/exec"
)

const sandboxConfigTmpl = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata: {name: vesseld-default}
spec:
  control: {socket: /tmp/vsbx.sock}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Sandbox
metadata: {name: dev-shell}
spec:
  backend: local
  rootDir: %s
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata: {name: support}
spec: {agents: [helper]}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata: {name: helper}
spec:
  engine: {ref: probe-tools}
  sandbox: dev-shell
`

// TestFleet_Sandbox_ExecToolReachableFromEngine is the end-to-end
// assertion for kind: Sandbox. The engine reads its tool registry
// via Deps.ToolRegistry, looks up the auto-injected `exec` tool,
// confirms its Definition().Name, and the test asserts the tool
// landed there. Any break in the chain (resolver did not build a
// runner, fleet did not overlay it onto the per-Captain registry,
// or the agent's Tools allow-list missed the auto-injection) shows
// up here as either a missing tool or a missing allow-list entry.
func TestFleet_Sandbox_ExecToolReachableFromEngine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	objs, err := apispec.DecodeAll(strings.NewReader(fmt.Sprintf(sandboxConfigTmpl, root)), "<test>")
	if err != nil {
		t.Fatal(err)
	}

	var sawExec atomic.Bool
	var sawToolInAllowList atomic.Bool
	cat := catalog.New()
	cat.RegisterEngine("probe-tools", func(_ string, _ map[string]any, deps catalog.Deps) (engine.Engine, error) {
		// The fleet hands the auto-injected exec tool name into
		// aspec.Tools at captain-build time; the engine builder
		// surfaces it through deps.AgentTools. We capture both
		// observations so a future regression that breaks only
		// one half (e.g. registry overlay missing, but allow-
		// list present) is still caught.
		for _, name := range deps.AgentTools {
			if name == exectool.Name {
				sawToolInAllowList.Store(true)
			}
		}
		if _, ok := deps.ToolRegistry.Get(exectool.Name); ok {
			sawExec.Store(true)
		}
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
			return b, nil
		}), nil
	})

	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	if _, ok := plan.SharedSandboxes["dev-shell"]; !ok {
		t.Fatal("plan.SharedSandboxes missing dev-shell")
	}

	f, err := Build(*plan)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Stop(ctx)
	}()
	h, err := f.Submit(context.Background(), "support", "helper", agent.Request{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !sawExec.Load() {
		t.Fatal("engine never saw `exec` tool in deps.ToolRegistry (sandbox overlay missing)")
	}
	if !sawToolInAllowList.Load() {
		t.Fatal("engine never saw `exec` in deps.AgentTools (auto-injection missing)")
	}
}

// TestFleet_NoSandbox_SharedRegistryReused covers the cheap path:
// a vessel without any sandbox-using agent must keep using the
// daemon-shared registry verbatim. A regression that always
// allocates a per-Captain registry would still pass other tests
// but waste memory on every vessel; this pin keeps that quiet
// regression visible.
func TestFleet_NoSandbox_SharedRegistryReused(t *testing.T) {
	t.Parallel()
	objs, err := apispec.DecodeAll(strings.NewReader(fleetConfig), "<test>")
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New()
	cat.RegisterEngine("noop", func(_ string, _ map[string]any, _ catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
			return b, nil
		}), nil
	})
	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs)
	}
	f, err := Build(*plan)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	reg, err := f.buildCaptainToolRegistry(plan.Vessels[0])
	if err != nil {
		t.Fatalf("buildCaptainToolRegistry: %v", err)
	}
	if reg != plan.SharedToolRegistry {
		t.Fatal("buildCaptainToolRegistry returned a fresh registry for a non-sandbox vessel; expected the shared instance")
	}
}
