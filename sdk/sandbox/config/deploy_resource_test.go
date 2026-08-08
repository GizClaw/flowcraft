package config_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
)

func TestDeployFactorySpec(t *testing.T) {
	got := sandboxconfig.NewBuilder(sandboxconfig.Deps{}).Spec()
	want := sdkconfig.Spec{
		Kind: sandboxconfig.ResourceKind,
		Impl: "yaml",
		Deps: []sdkconfig.DepSpec{{
			Name:     sandboxconfig.WorkspacesDep,
			Type:     workspaceconfig.ResourceKind,
			Required: true,
		}},
		ItemType: "sandbox.Runner",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

// TestRegistriesWireTogether is the direct shape of the resource area:
// the sandbox resource reaches its workspaces through deps, and one
// runner can be resolved out of the built sandbox registry.
func TestRegistriesWireTogether(t *testing.T) {
	workspaceFactory := workspaceconfig.NewBuilder(workspaceconfig.Deps{})
	wsValue, err := workspaceFactory.New(context.Background(), sdkconfig.Input{
		Resolve: resolveLiteral(t),
		Settings: literalSettings(t, `{
			"version": "v1",
			"workspaces": {
				"project": {"driver": "local", "settings": {"root": "project"}}
			}
		}`),
	})
	if err != nil {
		t.Fatalf("workspace New: %v", err)
	}
	workspaces := wsValue.(*workspaceconfig.Registry)

	sandboxFactory := sandboxconfig.NewBuilder(sandboxconfig.Deps{})
	boxValue, err := sandboxFactory.New(context.Background(), sdkconfig.Input{
		Resolve: resolveLiteral(t),
		Settings: literalSettings(t, `{
			"version": "v1",
			"sandboxes": {
				"coding": {"backend": "local", "workspace": "project"}
			}
		}`),
		Deps: map[string]any{
			sandboxconfig.WorkspacesDep: workspaces,
		},
	})
	if err != nil {
		t.Fatalf("sandbox New: %v", err)
	}
	boxes := boxValue.(*sandboxconfig.Registry)
	item, ok := boxes.ResolveItem("coding")
	if !ok {
		t.Fatal("ResolveItem(coding) did not find runner")
	}
	if _, ok := item.(coresandbox.Runner); !ok {
		t.Fatalf("ResolveItem returned %T, want sandbox.Runner", item)
	}
}

func TestDeployFactoryNewRejectsInvalidDependenciesAndSettings(t *testing.T) {
	factory := sandboxconfig.NewBuilder(sandboxconfig.Deps{})
	settings := literalSettings(t, `{"version": "v1", "sandboxes": {}}`)
	for name, deps := range map[string]map[string]any{
		"missing": nil,
		"wrong type": {
			sandboxconfig.WorkspacesDep: "not a registry",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), sdkconfig.Input{
				Resolve:  resolveLiteral(t),
				Settings: settings,
				Deps:     deps,
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New error = %v, want validation", err)
			}
		})
	}
	if _, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsOpaque(t, `{"unknown": true}`),
		Deps: map[string]any{
			sandboxconfig.WorkspacesDep: (*workspaceconfig.Registry)(nil),
		},
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
}

func settingsOpaque(t *testing.T, raw string) json.RawMessage {
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
