package config_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/tool/config"
)

func TestDeployFactorySpec(t *testing.T) {
	got := config.NewDeployFactory(
		config.NewBuilder(config.Deps{}),
	).Spec()
	want := sdkconfig.ResourceSpec{
		Kind: config.ResourceKind,
		Impl: "yaml",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewBuildsAssemblyAndRejectsInvalidInput(t *testing.T) {
	factory := config.NewDeployFactory(
		config.NewBuilder(config.Deps{}),
	)
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"inline":{"version":"v1"}}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*config.Assembly); !ok {
		t.Fatalf("New returned %T, want *config.Assembly", value)
	}

	if _, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"unknown":true}`),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
	if _, err := config.NewDeployFactory(nil).New(
		context.Background(),
		sdkconfig.Input{},
	); err == nil {
		t.Fatal("New with nil tool builder succeeded")
	}
}

func settingsJSON(t *testing.T, raw string) *sdkconfig.Opaque {
	t.Helper()
	var opaque sdkconfig.Opaque
	if err := json.Unmarshal([]byte(raw), &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &opaque
}
