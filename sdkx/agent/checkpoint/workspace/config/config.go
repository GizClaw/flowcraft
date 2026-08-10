// Package config adapts the workspace-backed checkpoint store to
// deployment resources. Register [NewFactory] on a deploy.Builder:
//
//	resources:
//	  cps:
//	    kind: agent.CheckpointStore
//	    impl: workspace
//	    deps: {workspace: ws/project}
//	    settings: {prefix: agent/checkpoints}
package config

import (
	"context"
	"fmt"
	"reflect"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	checkpointworkspace "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/workspace"
)

// ResourceKind is the deployment resource kind implemented by
// checkpoint stores. It matches the sqlite implementation so runtime
// checkpoint wiring accepts either backend.
const ResourceKind = "agent.CheckpointStore"

// Settings is the strict settings subtree for the workspace resource.
type Settings struct {
	// Prefix is the workspace directory holding checkpoint files.
	// Empty uses the store default ("agent/checkpoints").
	Prefix string `json:"prefix,omitempty"`
}

type factory struct{}

// NewFactory returns a deployment resource factory for
// [checkpointworkspace.Store].
func NewFactory() sdkconfig.Factory {
	return factory{}
}

// Spec implements sdkconfig.Factory.
func (factory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{
		Kind: ResourceKind,
		Impl: "workspace",
		Deps: []sdkconfig.DepSpec{{
			Name:     "workspace",
			Type:     "workspace.Workspace",
			Required: true,
		}},
	}
}

// New implements sdkconfig.Factory.
func (factory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	settings, err := sdkconfig.DecodeSettings[Settings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint config: decode settings: %w", err))
	}
	value, ok := in.Deps["workspace"]
	if !ok || isNilValue(value) {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint config: dep %q is required", "workspace"))
	}
	ws, ok := value.(workspace.Workspace)
	if !ok {
		return nil, errdefs.Validation(fmt.Errorf(
			"workspace checkpoint config: dep %q has Go type %T, want workspace.Workspace",
			"workspace", value))
	}
	var options []checkpointworkspace.Option
	if settings.Prefix != "" {
		options = append(options, checkpointworkspace.WithPrefix(settings.Prefix))
	}
	return checkpointworkspace.New(ws, options...)
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
