package memory

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Assembly is the memory capability contract a deployment binds to:
// agent lifecycle hooks consume it as a context provider and turn
// sink, and applications can push documents. Implementations (such as
// the flowcraft memory module) remain entirely behind the Factory and
// may expose implementation-specific services (like background
// workers) through their own types; this package knows nothing about
// them.
type Assembly interface {
	ContextProvider
	TurnSink
	DocumentSink
}

// Input carries the deployment-provided dependencies and the
// implementation-owned settings document to a memory Factory. Settings
// is the raw settings sub-document (typically the memory.yaml bytes);
// its schema belongs to the implementation, not to this package.
type Input struct {
	Workspace workspace.Workspace
	Inference *inference.Runtime
	Settings  []byte
}

// Factory builds one Assembly from deployment inputs. Implementations
// live in their own modules (e.g. github.com/GizClaw/flowcraft/memory)
// and are registered by the application, mirroring how inference
// provider factories are registered.
type Factory interface {
	New(context.Context, Input) (Assembly, error)
}

// FactoryFunc adapts a plain function to Factory.
type FactoryFunc func(context.Context, Input) (Assembly, error)

// New calls f.
func (f FactoryFunc) New(ctx context.Context, input Input) (Assembly, error) {
	return f(ctx, input)
}
