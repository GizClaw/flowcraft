package yaml

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/memory/config"
)

// ResourceKind is the deploy resource kind this package builds.
// The returned value is a *config.Assembly.
const ResourceKind = "memory.Assembly"

// ResourceSettings is the impl-owned settings subtree of a
// memory resource. The documented shape is:
//
//	deps:
//	  inference: infer/runtime
//	settings: { file: ./memory.yaml }
//
// Deps is declared by the deploy framework, not by this
// package. Settings carries the memory.yaml path.
type ResourceSettings struct {
	File string `yaml:"file"`
}

// deployFactory is the [deploy.ResourceFactory] implementation.
type deployFactory struct {
	builder *config.Builder
}

// NewDeployFactory returns the deploy factory for memory
// assemblies. The host supplies the [config.Builder] that owns
// the StoreFactory catalog: deployments reference WHICH stores
// they want, the host decides which driver code exists.
//
//	b.RegisterResource(yaml.NewDeployFactory(builder))
func NewDeployFactory(builder *config.Builder) deploy.ResourceFactory {
	return &deployFactory{builder: builder}
}

// Spec declares the resource kind + impl + deps the deploy
// framework needs to validate the document and the host
// needs to build a working memory.Assembly.
//
//   - Kind / Impl pair identifies the resource kind this
//     package claims.
//   - Deps["inference"] is optional for memory documents without embedding
//     and must resolve to an inference runtime when embedding is enabled.
func (*deployFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{
		Kind:     ResourceKind,
		Impl:     "yaml",
		ItemType: "memory.Runtime",
		Deps: []deploy.ResourceDepSpec{
			{
				Name:     "inference",
				Type:     "inference.Runtime",
				Required: false,
			},
		},
	}
}

// New decodes the settings (a single `file:` path), reads the
// memory.yaml at that path, decodes it into a Document, and
// hands the Document + the resolved deps to the Builder. The
// returned value is a *config.Assembly; the deploy framework
// closes it in reverse construction order.
func (f *deployFactory) New(ctx context.Context, in deploy.ResourceInput) (any, error) {
	settings, err := deploy.DecodeSettings[ResourceSettings](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory config: decode resource settings: %w", err))
	}
	if settings.File == "" {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory config: resource settings.file is required"))
	}
	doc, err := config.DecodeYAMLFile(settings.File)
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	assembly, err := f.builder.NewAssembly(ctx, doc, in.Deps)
	if err != nil {
		return nil, err
	}
	return assembly, nil
}
