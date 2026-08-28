package secret

import (
	"context"
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/core/resource"
)

// EnvSettings is the settings subtree of the "env" secret store.
// Secrets are environment variables named by the secret reference.
type EnvSettings struct {
	// ID is the short name used in ${secret:ID.NAME} references; empty
	// falls back to the resource name (e.g. "secret.env" -> "secret.env").
	ID string `json:"id,omitempty"`
	// Default marks this store as the target of NAME-only
	// ${secret:NAME} references. At most one store in a deployment may
	// be default.
	Default bool `json:"default,omitempty"`
}

// envFactory builds an environment-backed secret store.
type envFactory struct{}

func (envFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "env"}
}

// New implements resource.Factory.
func (envFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[EnvSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("secret env store: decode settings: %w", err)
	}
	return Store{
		id:  settings.ID,
		def: settings.Default,
		lookup: func(_ context.Context, name string) (string, bool, error) {
			value, ok := os.LookupEnv(name)
			return value, ok, nil
		},
	}, nil
}

// Register adds the built-in secret store impls to r.
func Register(r *resource.Registry) error {
	if err := r.Register(envFactory{}); err != nil {
		return err
	}
	return nil
}
