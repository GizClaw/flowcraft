package memory

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	derivecontextpack "github.com/GizClaw/flowcraft/memory/derive/context"
)

func TestWithDefaultDepsInstallsRRFContextPacker(t *testing.T) {
	deps := withDefaultDeps(Deps{})

	if _, ok := deps.ContextPacker.(derivecontextpack.RRFPacker); !ok {
		t.Fatalf("ContextPacker = %T, want default RRFPacker", deps.ContextPacker)
	}
}

func TestWithDefaultDepsPreservesCustomContextPacker(t *testing.T) {
	custom := customDefaultContextPacker{}
	deps := withDefaultDeps(Deps{ContextPacker: custom})

	if deps.ContextPacker != custom {
		t.Fatalf("ContextPacker = %T, want custom packer preserved", deps.ContextPacker)
	}
}

type customDefaultContextPacker struct{}

func (customDefaultContextPacker) PackContext(context.Context, derive.ContextPackInput) (derive.ContextPackOutput, error) {
	return derive.ContextPackOutput{}, nil
}
