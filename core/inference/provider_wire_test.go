package inference

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// sdkShapedWire mirrors what a provider SDK hands back for a request: a tool
// schema is an open map, and some fields are plain interface values.
type sdkShapedWire struct {
	Model string
	Tools []sdkShapedTool
	Extra map[string]any
}

type sdkShapedTool struct {
	Name       string
	Parameters map[string]any
}

type openValueWire struct {
	Value any
}

type openSliceWire struct {
	Items []any
}

type canonicalPartWire struct {
	Part message.Part
}

type canonicalContentWire struct {
	Messages []message.Content
}

type canonicalRequestWire struct {
	Request GenerateRequest
}

func bindWireType[Wire any](t *testing.T) error {
	t.Helper()
	compile := func(
		context.Context,
		ModelRef,
		GenerateRequest,
		GenerateExecutionShape,
	) (Compiled[Wire], error) {
		var zero Wire
		return Compiled[Wire]{Wire: zero}, nil
	}
	transport := func(context.Context, Wire) (string, error) { return "", nil }
	decode := func(context.Context, string) (GenerateResponse, error) {
		return GenerateResponse{}, nil
	}
	_, err := BindGenerate[Wire, string](compile, transport, decode)
	return err
}

// TestProviderWireAcceptsOpenInterfaceValues pins the contract drivers rely on
// when they hand their SDK's own request type to Transport: interface values
// inside the wire are allowed.
func TestProviderWireAcceptsOpenInterfaceValues(t *testing.T) {
	for _, test := range []struct {
		name string
		bind func(*testing.T) error
	}{
		{"sdk params", func(t *testing.T) error { return bindWireType[sdkShapedWire](t) }},
		{"any field", func(t *testing.T) error { return bindWireType[openValueWire](t) }},
		{"any slice", func(t *testing.T) error { return bindWireType[openSliceWire](t) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.bind(t); err != nil {
				t.Fatalf("bind rejected an SDK-shaped wire: %v", err)
			}
		})
	}
}

// TestProviderWireRejectsCanonicalTypes pins the half of the contract that
// survives: a wire may not embed canonical state, because Transport reading a
// canonical field would bypass the compiler's ledger.
func TestProviderWireRejectsCanonicalTypes(t *testing.T) {
	for _, test := range []struct {
		name string
		bind func(*testing.T) error
	}{
		{"canonical part", func(t *testing.T) error {
			return bindWireType[canonicalPartWire](t)
		}},
		{"canonical content", func(t *testing.T) error {
			return bindWireType[canonicalContentWire](t)
		}},
		{"canonical request", func(t *testing.T) error {
			return bindWireType[canonicalRequestWire](t)
		}},
		{"canonical request itself", func(t *testing.T) error {
			return bindWireType[GenerateRequest](t)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.bind(t)
			if err == nil {
				t.Fatal("bind accepted a wire that embeds canonical state")
			}
			if !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("bind error = %v, want the canonical wire contract", err)
			}
		})
	}
}
