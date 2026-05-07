package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const fleetConfig = `
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
    ref: noop
`

func TestFleet_Build_Submit_Stop(t *testing.T) {
	t.Parallel()
	objs, err := apispec.DecodeAll(strings.NewReader(fleetConfig), "<test>")
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New()
	cat.RegisterEngine("noop", func(_ string, _ map[string]any, _ catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "hello"))
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
	if err := f.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Stop(ctx)
	}()

	if names := f.Names(); len(names) != 1 || names[0] != "support" {
		t.Fatalf("Names = %v", names)
	}

	h, err := f.Submit(context.Background(), "support", "helper", agent.Request{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	res, err := h.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(res.Messages) == 0 || res.Messages[0].Content() != "hello" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestFleet_NotFound(t *testing.T) {
	t.Parallel()
	f := &Fleet{captains: map[string]*captainEntry{}}
	if _, err := f.Captain("missing"); err == nil {
		t.Fatal("expected NotFound")
	}
}
