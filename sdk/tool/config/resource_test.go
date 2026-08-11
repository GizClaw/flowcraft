package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/config"
)

func TestDeployFactorySpec(t *testing.T) {
	got := config.NewBuilder(config.Deps{}).Spec()
	want := sdkconfig.Spec{
		Kind: config.ResourceKind,
		Impl: "yaml",
		Deps: []sdkconfig.DepSpec{{
			Name:     config.DepSandbox,
			Type:     "sandbox.Runner",
			Required: false,
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewPassesDepsToBuiltinFactories(t *testing.T) {
	factory := config.NewBuilder(config.Deps{})
	var got any
	factory.RegisterBuiltinFactory("made", func(
		_ context.Context,
		in sdkconfig.Input,
	) (tool.Tool, error) {
		got, _ = in.Dep(config.DepSandbox)
		return echoTool("made"), nil
	})
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Resolve: resolveLiteral(t),
		Settings: literalSettings(t, `{
			"version": "v1",
			"sources": [{"kind": "builtin", "spec": {"tools": ["made"]}}]
		}`),
		Deps: map[string]any{config.DepSandbox: "the-runner"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	assembly := value.(*config.Assembly)
	if _, ok := assembly.Catalog.Get("made"); !ok {
		t.Fatal("builtin factory tool missing from catalog")
	}
	if got != "the-runner" {
		t.Fatalf("factory dep = %#v, want %q", got, "the-runner")
	}
}

func TestDeployFactoryNewBuiltinFactoryErrorsPropagate(t *testing.T) {
	factory := config.NewBuilder(config.Deps{})
	factory.RegisterBuiltinFactory("needy", func(
		_ context.Context,
		in sdkconfig.Input,
	) (tool.Tool, error) {
		if _, ok := in.Dep(config.DepSandbox); !ok {
			return nil, errors.New("needs sandbox dep")
		}
		return echoTool("needy"), nil
	})
	_, err := factory.New(context.Background(), sdkconfig.Input{
		Resolve: resolveLiteral(t),
		Settings: literalSettings(t, `{
			"version": "v1",
			"sources": [{"kind": "builtin", "spec": {"tools": ["needy"]}}]
		}`),
	})
	if err == nil || !strings.Contains(err.Error(), "needs sandbox dep") {
		t.Fatalf("New error = %v, want missing-dep failure", err)
	}
}

func TestDeployFactoryNewBuildsAssemblyAndRejectsInvalidInput(t *testing.T) {
	factory := config.NewBuilder(config.Deps{})
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Resolve:  resolveLiteral(t),
		Settings: literalSettings(t, `{"version":"v1"}`),
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
	if _, err := (*config.Builder)(nil).New(
		context.Background(),
		sdkconfig.Input{},
	); err == nil {
		t.Fatal("New with nil tool builder succeeded")
	}
}

func settingsJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	var opaque json.RawMessage
	if err := json.Unmarshal([]byte(raw), &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return opaque
}

func literalSettings(t *testing.T, doc string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal literal settings: %v", err)
	}
	return json.RawMessage(raw)
}

func resolveLiteral(t *testing.T) func(context.Context, sdkconfig.Source) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, src sdkconfig.Source) ([]byte, error) {
		return sdkconfig.NewLoader().Load(ctx, src)
	}
}
