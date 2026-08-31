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

// BackendName is the sandbox impl name for the windows backend.
const BackendName = "windows"

type settings struct {
	Root          string   `json:"root"`
	WriteConfine  bool     `json:"write_confine,omitempty"`
	WritablePaths []string `json:"writable_paths,omitempty"`
}

type factory struct{}

// NewFactory returns a deployment resource factory for the windows
// backend.
func NewFactory() resource.Factory { return factory{} }

// Spec implements resource.Factory.
func (factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: BackendName}
}

// New implements resource.Factory.
func (factory) New(ctx context.Context, in resource.Input) (any, error) {
	s, err := resource.DecodeTyped[settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("decode windows settings: %v", err)
	}
	if s.Root == "" {
		return nil, errdefs.Validationf("windows settings.root is required")
	}
	var options []Option
	if s.WriteConfine {
		options = append(options, WithWriteConfinement())
	}
	writable, err := sandbox.ResolveMany(s.Root, s.WritablePaths)
	if err != nil {
		return nil, fmt.Errorf("windows settings.writable_paths: %w", err)
	}
	if writable != nil {
		options = append(options, WithWritablePaths(writable...))
	}
	runner, err := New(s.Root, options...)
	if err != nil {
		return nil, err
	}
	return sandbox.Runner(runner), nil
}

// Register adds the windows factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}
