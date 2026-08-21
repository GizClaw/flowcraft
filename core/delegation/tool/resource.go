package delegation

import (
	"context"
	"reflect"

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
func NewSourceFactory() res.Factory {
	return &sourceFactory{}
}

// Spec implements res.Factory.
func (sourceFactory) Spec() res.Spec {
	return res.Spec{
		Kind: "tool.Source",
		Impl: SourceImpl,
		Deps: []res.DepSpec{{
			Name:     sdkdelegation.DirectoryDep,
			Type:     "delegation.Directory",
			Required: true,
		}},
	}
}

// New implements res.Factory: the source takes no settings.
func (sourceFactory) New(_ context.Context, in res.Input) (any, error) {
	if _, err := res.DecodeTyped[struct{}](in.Settings); err != nil {
		return nil, errdefs.Validationf(
			"delegation tool resource: decode settings: %v", err)
	}
	value, ok := in.Dep(sdkdelegation.DirectoryDep)
	if !ok {
		return nil, errdefs.Validationf(
			"delegation tool resource: dep %q is required", sdkdelegation.DirectoryDep)
	}
	directory, ok := value.(sdkdelegation.Directory)
	if !ok || isNilInterface(directory) {
		return nil, errdefs.Validationf(
			"delegation tool resource: dep %q is %T, want delegation.Directory",
			sdkdelegation.DirectoryDep, value)
	}
	return &source{directory: directory}, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
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
