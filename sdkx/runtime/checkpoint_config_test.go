package runtime

import (
	"context"
	"strings"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/event"
	sqliteconfig "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

func TestCheckpointStoreResourceKindMatchesConfigFactory(t *testing.T) {
	if checkpointStoreResourceKind != sqliteconfig.ResourceKind {
		t.Fatalf("runtime kind %q != sqlite config kind %q",
			checkpointStoreResourceKind, sqliteconfig.ResourceKind)
	}
}

func TestBuildWiresCheckpointStoreResource(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()

	builder := newDeployBuilder(t, bus, &struct{}{}, nil)
	builder.MustRegisterResource(testResourceFactory{
		spec:  sdkconfig.Spec{Kind: checkpointStoreResourceKind, Impl: "test"},
		value: &recordingCheckpointStore{},
	})

	rt, err := NewBuilder(builder).Build(context.Background(),
		checkpointRuntimeDoc(t, checkpointStoreResourceKind))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = rt.Close() }()
}

func TestBuildRejectsWrongCheckpointStoreKind(t *testing.T) {
	bus := event.NewMemoryBus()
	defer func() { _ = bus.Close() }()

	builder := newDeployBuilder(t, bus, &struct{}{}, nil)
	_, err := NewBuilder(builder).Build(context.Background(),
		checkpointRuntimeDoc(t, "test.Store"))
	if err == nil || !strings.Contains(err.Error(), "checkpoint_store") {
		t.Fatalf("Build error = %v, want checkpoint_store kind validation", err)
	}
}

func checkpointRuntimeDoc(t *testing.T, checkpointKind string) deploy.Document {
	t.Helper()
	text := `version: v1
resources:
  events: {kind: event.Bus, impl: test}
  cps: {kind: ` + checkpointKind + `, impl: test}
agents:
  bot:
    engine: {kind: test}
runtime:
  event_bus: events
  checkpoint_store: cps
  sessions: {idle_timeout: 1s, sink_buffer: 8, resume: true}
`
	doc, err := deploy.Parse([]byte(text))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}
