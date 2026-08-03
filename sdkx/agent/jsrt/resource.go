package jsrt

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// ResourceKind is the deploy resource kind implemented by script runtimes.
const ResourceKind = "agent.ScriptRuntime"

// ResourceSettings is the settings subtree for a JavaScript runtime resource.
type ResourceSettings struct {
	// PoolSize is the number of pooled JavaScript VMs. It must be positive.
	PoolSize *int `yaml:"pool_size,omitempty"`
	// MaxCallStackSize bounds script call-stack depth. It must be positive.
	MaxCallStackSize *int `yaml:"max_call_stack_size,omitempty"`
	// MaxExecTime is a Go duration string. Zero disables the additional cap.
	MaxExecTime *string `yaml:"max_exec_time,omitempty"`
}

type deployFactory struct{}

// NewDeployFactory returns a deploy factory for JavaScript script runtimes.
func NewDeployFactory() deploy.ResourceFactory {
	return deployFactory{}
}

func (deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{Kind: ResourceKind, Impl: "js"}
}

func (deployFactory) New(_ context.Context, in deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
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
