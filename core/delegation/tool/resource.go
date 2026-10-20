package delegation

import (
	"context"

	sdkdelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	res "github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
)

// SourceImpl is the tool.Source impl name for the delegation tools.
const SourceImpl = "delegation"

type sourceFactory struct{}

// NewSourceFactory returns a tool.Source factory whose tools are the
// delegate + delegation_status pair. The directory is resolved from the
// deploy dependency, so every deployment generation's tools see that
// generation's targets at call time.
// depDirectory is the delegation directory the tools read at call time.
var depDirectory = res.Require[sdkdelegation.Directory](sdkdelegation.DirectoryDep)

func NewSourceFactory() res.Factory[*source] {
	return &sourceFactory{}
}

// Spec implements res.Factory.
func (sourceFactory) Spec() res.Spec {
	return res.Spec{
		Kind: "tool.Source",
		Impl: SourceImpl,
		Deps: []res.Requirement{depDirectory.Spec()},
	}
}

// New implements res.Factory: the source takes no settings.
func (sourceFactory) New(ctx context.Context, in res.Input) (*source, error) {
	if _, err := res.DecodeTyped[struct{}](ctx, in.Settings); err != nil {
		return nil, errdefs.Validationf(
			"delegation tool resource: decode settings: %w", err)
	}
	directory, err := depDirectory.Get(in)
	if err != nil {
		return nil, err
	}
	return &source{directory: directory}, nil
}

// source is the tool.Source value contributing the delegation tools.
type source struct {
	directory sdkdelegation.Directory
}

// Tools implements tool.Source.
func (s *source) Tools() []tool.Tool {
	return New(s.directory)
}

// LazyTools implements tool.Source.
func (s *source) LazyTools() []tool.LazyTool { return nil }

var _ tool.Source = (*source)(nil)
