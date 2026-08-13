// Package sqlite adapts the SQLite checkpoint store to deployment
// resources. Register [Register] on a resource.Registry to make
// checkpoints configurable from a document:
//
//	resources:
//	  cps:
//	    kind: agent.CheckpointStore
//	    impl: sqlite
//	    settings:
//	      path: ./data/checkpoints.db
package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind implemented by
// checkpoint stores.
const ResourceKind = "agent.CheckpointStore"

// Settings is the strict settings subtree for the sqlite resource.
type Settings struct {
	// Path is the SQLite database file. ":memory:" is allowed for
	// tests and ephemeral runs.
	Path string `json:"path"`
}

type factory struct {
	opts []Option
}

// NewFactory returns a deployment resource factory for [Store]. Options
// are applied to every store the factory opens.
func NewFactory(opts ...Option) resource.Factory {
	return factory{opts: append([]Option(nil), opts...)}
}

// Spec implements resource.Factory.
func (factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "sqlite"}
}

// New implements resource.Factory.
func (f factory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](
		in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"sqlite checkpoint config: decode settings: %w", err))
	}
	if strings.TrimSpace(settings.Path) == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"sqlite checkpoint config: settings.path is required"))
	}
	store, err := OpenContext(ctx, settings.Path, f.opts...)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint config: open: %w", err)
	}
	return store, nil
}

// Register adds the sqlite checkpoint store factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}
