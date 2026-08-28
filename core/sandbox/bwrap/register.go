package bwrap

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

// BackendName is the sandbox impl name for bubblewrap.
const BackendName = "bwrap"

type settings struct {
	Root          string   `json:"root"`
	Binary        string   `json:"binary,omitempty"`
	WritablePaths []string `json:"writable_paths,omitempty"`
	ExtraFlags    []string `json:"extra_flags,omitempty"`
	ReadOnlyRoot  bool     `json:"readonly_root,omitempty"`
}

type factory struct{}

// NewFactory returns a deployment resource factory for bubblewrap.
func NewFactory() resource.Factory { return factory{} }

// Spec implements resource.Factory.
func (factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: BackendName}
}

// New implements resource.Factory.
func (factory) New(ctx context.Context, in resource.Input) (any, error) {
	s, err := resource.DecodeTyped[settings](ctx, in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, errdefs.Validationf("decode bwrap settings: %v", err)
	}
	if s.Root == "" {
		return nil, errdefs.Validationf("bwrap settings.root is required")
	}
	var options []RunnerOption
	if s.Binary != "" {
		binary, err := sandbox.Resolve(s.Root, s.Binary)
		if err != nil {
			return nil, fmt.Errorf("bwrap settings.binary: %w", err)
		}
		options = append(options, WithBinary(binary))
	}
	writable, err := sandbox.ResolveMany(s.Root, s.WritablePaths)
	if err != nil {
		return nil, fmt.Errorf("bwrap settings.writable_paths: %w", err)
	}
	if writable != nil {
		options = append(options, WithWritablePaths(writable...))
	}
	if s.ExtraFlags != nil {
		options = append(options, WithExtraFlags(s.ExtraFlags...))
	}
	if s.ReadOnlyRoot {
		options = append(options, WithReadOnlyRoot())
	}
	runner, err := New(s.Root, options...)
	if err != nil {
		return nil, err
	}
	return sandbox.Runner(runner), nil
}

// Register adds the bubblewrap factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}
