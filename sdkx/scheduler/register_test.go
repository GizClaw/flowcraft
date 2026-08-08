package scheduler_test

import (
	"context"
	"encoding/json"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
	"github.com/GizClaw/flowcraft/sdkx/scheduler"
)

func TestRegisterBuildsLocalServer(t *testing.T) {
	builder := schedulerconfig.NewBuilder()
	if err := scheduler.Register(builder); err != nil {
		t.Fatalf("Register: %v", err)
	}
	value, err := schedulerconfig.NewDeployFactory(
		scheduler.BackendName,
		builder,
	).New(context.Background(), sdkconfig.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(sdkscheduler.Server); !ok {
		t.Fatalf("New returned %T, want scheduler.Server", value)
	}
}

func TestRegisterRejectsSettings(t *testing.T) {
	builder := schedulerconfig.NewBuilder()
	if err := scheduler.Register(builder); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var settings json.RawMessage
	if err := json.Unmarshal([]byte(`{"x":1}`), &settings); err != nil {
		t.Fatal(err)
	}
	if _, err := schedulerconfig.NewDeployFactory(
		scheduler.BackendName,
		builder,
	).New(context.Background(), sdkconfig.Input{Settings: settings}); err == nil {
		t.Fatal("local scheduler accepted settings")
	}
}
