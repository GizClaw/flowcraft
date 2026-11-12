package host

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// captureKind records the deps each deployment built its engine from: the two
// observation policies only reach an engine through them.
func captureKind(captured *[]deploy.EngineDeps) deploy.EngineKind {
	return deploy.EngineKind{
		Name:  "capture",
		Local: true,
		Build: func(_ context.Context, deps deploy.EngineDeps) (agent.RunnerFactory, error) {
			*captured = append(*captured, deps)
			return func(context.Context, string, string) (agent.Runner, error) {
				return &textRunner{text: "hi"}, nil
			}, nil
		},
	}
}

// The document states the policy; the host's bindings are the default for the
// fields it leaves out. A field the document states wins outright — including
// switching bodies off under a host that keeps them on.
func TestObservabilityResolvesDocumentOverHostDefaults(t *testing.T) {
	var captured []deploy.EngineDeps
	kinds := deploy.Kinds{Engines: map[string]deploy.EngineKind{
		"capture": captureKind(&captured),
	}}
	bindings := testHost(t)
	bindings.Trace = telemetry.TraceStep
	bindings.Bodies = true
	manager := newManager(t, kinds, bindings)

	silent := testSpec(t, "silent", "capture")
	loud := testSpec(t, "loud", "capture")
	off := false
	loud.Observability = deploy.ObservabilitySpec{Trace: "node", Bodies: &off}

	for _, spec := range []deploy.Spec{silent, loud} {
		if err := manager.Add(context.Background(), spec); err != nil {
			t.Fatalf("Add %s: %v", spec.Name, err)
		}
	}
	if len(captured) != 2 {
		t.Fatalf("engines built = %d, want 2", len(captured))
	}
	if got := captured[0]; got.Trace != telemetry.TraceStep || !got.Bodies {
		t.Errorf("a silent document resolved to %s/%v, want the host's step/true",
			got.Trace, got.Bodies)
	}
	if got := captured[1]; got.Trace != telemetry.TraceNode || got.Bodies {
		t.Errorf("a stated document resolved to %s/%v, want node/false",
			got.Trace, got.Bodies)
	}
}

// The run span follows the same level: an agent whose document asks for a run
// span gets one, and one that asks for nothing gets nothing.
func TestObservabilityReachesTheAgentTable(t *testing.T) {
	var captured []deploy.EngineDeps
	kinds := deploy.Kinds{Engines: map[string]deploy.EngineKind{
		"capture": captureKind(&captured),
	}}
	bindings := testHost(t)
	bindings.Trace = telemetry.TraceOff
	manager := newManager(t, kinds, bindings)

	spec := testSpec(t, "quiet", "capture")
	spec.Observability = deploy.ObservabilitySpec{Trace: "run"}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("engines built = %d, want 1", len(captured))
	}
	if captured[0].Trace != telemetry.TraceRun {
		t.Errorf("engine trace = %s, want run", captured[0].Trace)
	}
}
