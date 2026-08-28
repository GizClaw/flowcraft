package luart

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

// ResourceSettings is the settings subtree for a Lua runtime resource.
type ResourceSettings struct {
	// PoolSize is the number of pooled Lua states. It must be positive.
	PoolSize *int `json:"pool_size,omitempty"`
	// MaxExecTime is a Go duration string. Zero disables the additional
	// cap.
	MaxExecTime *string `json:"max_exec_time,omitempty"`
}

type deployFactory struct{}

// NewDeployFactory returns a deployment factory for Lua script runtimes.
//
// The returned runtime implements io.Closer, so an assembly result owns
// and closes its VM pool.
func NewDeployFactory() resource.Factory {
	return deployFactory{}
}

func (deployFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "lua"}
}

func (deployFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[ResourceSettings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"luart: decode resource settings: %w", err))
	}

	var options []Option
	if settings.PoolSize != nil {
		if *settings.PoolSize <= 0 {
			return nil, errdefs.Validationf(
				"luart: resource setting pool_size must be positive")
		}
		options = append(options, WithPoolSize(*settings.PoolSize))
	}
	if settings.MaxExecTime != nil {
		duration, err := time.ParseDuration(*settings.MaxExecTime)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"luart: resource setting max_exec_time: %w", err))
		}
		if duration < 0 {
			return nil, errdefs.Validationf(
				"luart: resource setting max_exec_time must not be negative")
		}
		options = append(options, WithMaxExecTime(duration))
	}
	return New(options...), nil
}

// Register adds the Lua script runtime resource factory to r.
func Register(r *resource.Registry) error {
	return r.Register(deployFactory{})
}
