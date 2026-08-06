package config

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestResourceSpec_Validate(t *testing.T) {
	tests := []struct {
		name string
		spec ResourceSpec
		want string
	}{
		{
			name: "valid",
			spec: ResourceSpec{
				Kind: "k", Impl: "i",
				Deps: []ResourceDepSpec{{Name: "d", Type: "T"}},
			},
		},
		{
			name: "empty kind",
			spec: ResourceSpec{Impl: "i"},
			want: "kind is empty",
		},
		{
			name: "empty impl",
			spec: ResourceSpec{Kind: "k"},
			want: "impl is empty",
		},
		{
			name: "dep without name",
			spec: ResourceSpec{Kind: "k", Impl: "i", Deps: []ResourceDepSpec{{Type: "T"}}},
			want: "name is empty",
		},
		{
			name: "dep without type",
			spec: ResourceSpec{Kind: "k", Impl: "i", Deps: []ResourceDepSpec{{Name: "d"}}},
			want: "type is empty",
		},
		{
			name: "duplicate dep",
			spec: ResourceSpec{
				Kind: "k", Impl: "i",
				Deps: []ResourceDepSpec{
					{Name: "d", Type: "T"},
					{Name: "d", Type: "T"},
				},
			},
			want: "duplicate dep",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() succeeded, want error containing %q", tt.want)
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("Validate() error = %v, want validation classification", err)
			}
		})
	}
}

func TestResourceSpec_Clone(t *testing.T) {
	spec := ResourceSpec{
		Kind: "k", Impl: "i",
		Deps: []ResourceDepSpec{{Name: "d", Type: "T"}},
	}
	clone := spec.Clone()
	clone.Deps[0].Name = "changed"
	if spec.Deps[0].Name != "d" {
		t.Fatalf("Clone shares backing array: original dep = %q", spec.Deps[0].Name)
	}
}

func TestDepAccessors(t *testing.T) {
	ri := Input{Deps: map[string]any{"a": 1}}
	if v, ok := ri.Dep("a"); !ok || v != 1 {
		t.Fatalf("Input.Dep(a) = %v, %v; want 1, true", v, ok)
	}
	if _, ok := ri.Dep("missing"); ok {
		t.Fatal("Input.Dep(missing) reported present")
	}
	hi := Input{Deps: map[string]any{"b": 2}}
	if v, ok := hi.Dep("b"); !ok || v != 2 {
		t.Fatalf("Input.Dep(b) = %v, %v; want 2, true", v, ok)
	}
}

func TestResourceFactoryInterface(t *testing.T) {
	var _ ResourceFactory = staticFactory{}
}

type staticFactory struct{}

func (staticFactory) Spec() ResourceSpec {
	return ResourceSpec{Kind: "k", Impl: "i"}
}

func (staticFactory) New(context.Context, Input) (any, error) {
	return struct{}{}, nil
}
