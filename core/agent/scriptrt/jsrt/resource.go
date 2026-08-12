package jsrt

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by script
// runtimes.
const ResourceKind = "agent.ScriptRuntime"

// ResourceSettings is the settings subtree for a JavaScript runtime
// resource.
type ResourceSettings struct {
	// PoolSize is the number of pooled JavaScript VMs. It must be
	// positive.
	PoolSize *int `json:"pool_size,omitempty"`
	// MaxCallStackSize bounds script call-stack depth. It must be
	// positive.
	MaxCallStackSize *int `json:"max_call_stack_size,omitempty"`
	// MaxExecTime is a Go duration string. Zero disables the additional
	// cap.
	MaxExecTime *string `json:"max_exec_time,omitempty"`
}

type deployFactory struct{}

// NewDeployFactory returns a deployment factory for JavaScript script
// runtimes.
func NewDeployFactory() resource.Factory {
	return deployFactory{}
}

func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "js"}
}

func (deployFactory) New(_ context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"jsrt: decode resource settings: %w", err))
	}

	var options []Option
	if settings.PoolSize != nil {
		if *settings.PoolSize <= 0 {
			return nil, errdefs.Validationf(
				"jsrt: resource setting pool_size must be positive")
		}
		options = append(options, WithPoolSize(*settings.PoolSize))
	}
	if settings.MaxCallStackSize != nil {
		if *settings.MaxCallStackSize <= 0 {
			return nil, errdefs.Validationf(
				"jsrt: resource setting max_call_stack_size must be positive")
		}
		options = append(options, WithMaxCallStackSize(*settings.MaxCallStackSize))
	}
	if settings.MaxExecTime != nil {
		duration, err := time.ParseDuration(*settings.MaxExecTime)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"jsrt: resource setting max_exec_time: %w", err))
		}
		if duration < 0 {
			return nil, errdefs.Validationf(
				"jsrt: resource setting max_exec_time must not be negative")
		}
		options = append(options, WithMaxExecTime(duration))
	}
	return New(options...), nil
}

// Register adds the JavaScript script runtime resource factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}
