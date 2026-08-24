package windows

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// ResourceKind is the deployment resource kind implemented by this
// package.
const ResourceKind = "sandbox.Runner"

// BackendName is the sandbox impl name for the Windows backend.
const BackendName = "windows"

// Level selects which Windows enforcement backend a deployment wants.
// P1 ships the unelevated restricted-token backend only.
const (
	LevelRestrictedToken = "restricted-token"
	LevelElevated        = "elevated"
)

type settings struct {
	Root          string   `json:"root"`
	WritablePaths []string `json:"writable_paths,omitempty"`
	// Level defaults to LevelRestrictedToken. LevelElevated enables
	// the P2 path: dedicated accounts + WFP network filters via the
	// re-executed helper.
	Level string `json:"level,omitempty"`
}

type factory struct{}

// NewFactory returns a deployment resource factory for the Windows
// sandbox backend.
func NewFactory() resource.Factory { return factory{} }

// Spec implements resource.Factory.
func (factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: BackendName}
}

// New implements resource.Factory.
func (factory) New(_ context.Context, in resource.Input) (any, error) {
	s, err := resource.DecodeTyped[settings](in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, errdefs.Validationf("decode windows settings: %v", err)
	}
	if s.Root == "" {
		return nil, errdefs.Validationf("windows settings.root is required")
	}
	level := s.Level
	if level == "" {
		level = LevelRestrictedToken
	}
	switch level {
	case LevelRestrictedToken, LevelElevated:
		// Both backends are implemented; elevated requires the helper
		// binary to be present at spawn time.
	default:
		return nil, errdefs.Validationf(
			"windows: unknown level %q (want %q or %q)", level, LevelRestrictedToken, LevelElevated)
	}

	var options []RunnerOption
	writable, err := sandbox.ResolveMany(s.Root, s.WritablePaths)
	if err != nil {
		return nil, err
	}
	if writable != nil {
		options = append(options, WithWritablePaths(writable...))
	}
	options = append(options, WithLevel(level))
	runner, err := New(s.Root, options...)
	if err != nil {
		return nil, fmt.Errorf("windows: %w", err)
	}
	return sandbox.Runner(runner), nil
}

// Register adds the windows runner factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}
