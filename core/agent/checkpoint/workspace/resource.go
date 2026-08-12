package workspace

import (
	"context"
	"fmt"
	"reflect"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// ResourceKind is the deployment resource kind implemented by
// checkpoint stores. It is shared by every backend (workspace now,
// sqlite later) so runtime checkpoint wiring accepts either
// implementation.
const ResourceKind = "checkpoint.Store"

// Settings is the strict settings subtree of the workspace-backed
// checkpoint store factory.
type Settings struct {
	// Prefix is the workspace directory holding checkpoint files.
	// Empty uses the store default ("agent/checkpoints").
	Prefix string `json:"prefix,omitempty"`
}

// Factory builds a workspace-backed checkpoint store as the
// checkpoint.Store resource.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{
		Kind: ResourceKind,
		Impl: "workspace",
		Deps: []resource.DepSpec{{
			Name:     "workspace",
			Type:     "workspace.Workspace",
			Required: true,
		}},
	}
}

// New implements resource.Factory.
func (Factory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint: decode settings: %w", err))
	}
	value, ok := in.Deps["workspace"]
	if !ok || isNilValue(value) {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint: dep %q is required", "workspace"))
	}
	ws, ok := value.(workspace.Workspace)
	if !ok {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint: dep %q has Go type %T, want workspace.Workspace",
			"workspace", value))
	}
	var options []Option
	if settings.Prefix != "" {
		options = append(options, WithPrefix(settings.Prefix))
	}
	return New(ws, options...)
}

// Register adds the workspace-backed checkpoint store factory to the
// registry.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
