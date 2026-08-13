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

type sourceFactory struct {
	directory sdkdelegation.Directory
}

// NewSourceFactory returns a tool.Source factory whose tools are the
// delegate + delegation_status pair. directory is app-owned and may be
// bound to a deploy result after assembly; the tools discover targets
// from it at call time.
func NewSourceFactory(directory sdkdelegation.Directory) res.Factory {
	return &sourceFactory{directory: directory}
}

// Spec implements res.Factory.
func (sourceFactory) Spec() res.Spec {
	return res.Spec{
		Kind: "tool.Source",
		Impl: SourceImpl,
	}
}

// New implements res.Factory: the source takes no settings.
func (f *sourceFactory) New(_ context.Context, in res.Input) (any, error) {
	if _, err := res.DecodeTyped[struct{}](in.Settings); err != nil {
		return nil, errdefs.Validationf(
			"delegation tool resource: decode settings: %v", err)
	}
	if f == nil || f.directory == nil {
		return nil, errdefs.Validationf(
			"delegation tool resource: directory is required")
	}
	return &source{directory: f.directory}, nil
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
