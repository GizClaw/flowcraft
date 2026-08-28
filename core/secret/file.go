package secret

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// FileSettings is the settings subtree of the "file" secret store:
// one file per secret under a base directory. This mirrors deployment
// conventions (docker secrets, mounted k8s secrets).
type FileSettings struct {
	// ID is the short name used in ${secret:ID.NAME} references; empty
	// falls back to the deployment resource name.
	ID string `json:"id,omitempty"`
	// Default marks this store as the target of NAME-only
	// ${secret:NAME} references. At most one store in a deployment may
	// be default.
	Default bool `json:"default,omitempty"`
	// Base is the directory secret files live in. Required. Lookups
	// cannot escape it (lexically or through symlinks) and are capped
	// at the loader's size limit.
	Base string `json:"base"`
}

// fileFactory builds a file-backed secret store.
type fileFactory struct{}

func (fileFactory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "file"}
}

// New implements resource.Factory.
func (fileFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[FileSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("secret file store: decode settings: %w", err)
	}
	if strings.TrimSpace(settings.Base) == "" {
		return nil, errdefs.Validationf("secret file store: settings.base is required")
	}
	loader := resource.NewLoader(resource.WithBaseDir(settings.Base))
	return Store{
		id:  settings.ID,
		def: settings.Default,
		lookup: func(ctx context.Context, name string) (string, bool, error) {
			raw, err := loader.Load(ctx, resource.Source{File: name})
			if err != nil {
				if errdefs.IsNotFound(err) {
					return "", false, nil
				}
				return "", false, err
			}
			// Mirror docker/k8s conventions: strip one trailing
			// newline so API keys stored with echo or printf work.
			return strings.TrimRight(string(raw), "\r\n"), true, nil
		},
	}, nil
}
