// Package config adapts the SQLite checkpoint store to deployment
// resources. Register [NewFactory] on a deploy.Builder to make
// checkpoints configurable from a document:
//
//	resources:
//	  cps:
//	    kind: agent.CheckpointStore
//	    impl: sqlite
//	    settings:
//	      path: ./data/checkpoints.db
package config

import (
	"context"
	"fmt"
	"strings"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sqlitecheckpoint "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite"
)

// ResourceKind is the deployment resource kind implemented by
// checkpoint stores. Runtime checkpoint wiring validates resources
// against this kind.
const ResourceKind = "agent.CheckpointStore"

// Settings is the strict settings subtree for the sqlite resource.
type Settings struct {
	// Path is the SQLite database file. ":memory:" is allowed for
	// tests and ephemeral runs.
	Path string `json:"path"`
}

type factory struct {
	opts []sqlitecheckpoint.Option
}

// NewFactory returns a deployment resource factory for
// [sqlitecheckpoint.Store]. Options are applied to every store the
// factory opens.
func NewFactory(opts ...sqlitecheckpoint.Option) sdkconfig.Factory {
	return factory{opts: append([]sqlitecheckpoint.Option(nil), opts...)}
}

// Spec implements sdkconfig.Factory.
func (factory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: ResourceKind, Impl: "sqlite"}
}

// New implements sdkconfig.Factory.
func (f factory) New(ctx context.Context, in sdkconfig.Input) (any, error) {
	settings, err := sdkconfig.DecodeSettings[Settings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"sqlite checkpoint config: decode settings: %w", err))
	}
	if strings.TrimSpace(settings.Path) == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"sqlite checkpoint config: settings.path is required"))
	}
	store, err := sqlitecheckpoint.OpenContext(ctx, settings.Path, f.opts...)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint config: open: %w", err)
	}
	return store, nil
}
