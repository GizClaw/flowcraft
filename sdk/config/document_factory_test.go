package config

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestDocumentFactory_SpecClones(t *testing.T) {
	spec := ResourceSpec{Kind: "report.Assembly", Impl: "yaml", ItemType: "report.Item"}
	f := NewDocumentFactory(spec, func(context.Context, []byte, map[string]any) (any, error) {
		return nil, nil
	})
	got := f.Spec()
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("Spec() = %+v, want %+v", got, spec)
	}
	// Mutating the returned spec must not affect the factory.
	got.Kind = "changed"
	if again := f.Spec(); again.Kind != spec.Kind {
		t.Fatalf("Spec() not cloned: %+v", again)
	}
}

func TestDocumentFactory_NewResolvesAndBuilds(t *testing.T) {
	var gotData []byte
	var gotDeps map[string]any
	f := NewDocumentFactory(ResourceSpec{Kind: "report.Assembly", Impl: "yaml"},
		func(_ context.Context, data []byte, deps map[string]any) (any, error) {
			gotData = data
			gotDeps = deps
			return "value", nil
		})

	var o Opaque
	if err := json.Unmarshal([]byte(`{"version":"v1"}`), &o); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	in := Input{
		Settings: &o,
		Deps:     map[string]any{"store": "s"},
		Resolve:  NewLoader().Load,
	}
	v, err := f.New(context.Background(), in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v != "value" {
		t.Fatalf("New returned %#v", v)
	}
	if string(gotData) != `{"version":"v1"}` {
		t.Fatalf("build received data %q", gotData)
	}
	if gotDeps["store"] != "s" {
		t.Fatalf("build received deps %#v", gotDeps)
	}
}

func TestDocumentFactory_NilBuildRejected(t *testing.T) {
	f := NewDocumentFactory(ResourceSpec{Kind: "a", Impl: "b"}, nil)
	var o Opaque
	if err := json.Unmarshal([]byte(`"x"`), &o); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	_, err := f.New(context.Background(), Input{
		Settings: &o,
		Resolve:  NewLoader().Load,
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("nil build error = %v, want validation", err)
	}
}
