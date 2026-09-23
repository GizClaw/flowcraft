package local

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// ResourceKind is the deployment resource kind implemented by this
// package.
const ResourceKind = "sandbox.Runner"

// BackendName is the sandbox impl name for the local process backend.
const BackendName = "local"

// Settings is the strict settings subtree of the local runner factory.
type Settings struct {
	Root string `json:"root"`
	// Journal attaches a file journal when present. The subtree being
	// there at all is the opt-in: an absent key costs nothing, and a
	// key with a typo'd field fails the build (the decoder rejects
	// unknown fields).
	Journal *journal.Settings `json:"journal,omitempty"`
}

// Factory builds a local process runner as the sandbox.Runner resource.
type Factory struct{}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: BackendName}
}

// New implements resource.Factory.
func (Factory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, err
	}
	if settings.Root == "" {
		return nil, errdefs.Validationf(
			"sandbox/local: settings.root is required")
	}
	var opts []Option
	if settings.Journal != nil {
		// Resolve and validate at build time: a deployment that asked
		// for a journal on a platform without a watch source, or with
		// settings the engine cannot honour, fails here rather than
		// producing a runner that quietly reports nothing.
		cfg, err := settings.Journal.Build(settings.Root, nil)
		if err != nil {
			return nil, err
		}
		opts = append(opts, WithFileJournalConfig(cfg))
	}
	return New(settings.Root, opts...), nil
}

// Register adds the local runner factory to r.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}

var _ sandbox.Runner = (*Runner)(nil)
