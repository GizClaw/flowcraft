package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// The benchmarks here measure what happens once per tool call, not once per process:
// narrowing a binding, reading a slot out of it, and looking a tool up. They are the
// numbers that decide whether a capability carrier is affordable on every call, so a
// regression in allocation shows up as a failing allocation assertion in the tests and as
// a number here.

type payload struct{ name string }

var payloadSlot = tool.NewSlot[*payload]("bench.payload")

func benchSet() *tool.CapSet {
	set := tool.NewCapSet()
	payloadSlot.Provide(set, &payload{name: "value"})
	return set
}

func benchBinding() tool.Binding {
	return tool.Bind(tooltest.Session("bench"), policy.Grant{}).WithCaps(benchSet())
}

// BenchmarkBindingNarrow is the per-call path dispatch runs before every execution.
func BenchmarkBindingNarrow(b *testing.B) {
	binding := benchBinding()
	allow := tool.AllowOf(payloadSlot)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := payloadSlot.Get(binding.Narrow(allow).Caps); !ok {
			b.Fatal("the declared slot is unreadable")
		}
	}
}

// BenchmarkSlotGet is one typed read out of the pocket.
func BenchmarkSlotGet(b *testing.B) {
	view := benchBinding().Caps
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := payloadSlot.Get(view); !ok {
			b.Fatal("the slot is unreadable")
		}
	}
}

// BenchmarkAllowOf is the registration-time cost of a declaration list, not a per-call
// one: dispatch caches the result per tool.
func BenchmarkAllowOf(b *testing.B) {
	needs := []tool.Need{payloadSlot}
	b.ReportAllocs()
	for b.Loop() {
		_ = tool.AllowOf(needs...)
	}
}

// BenchmarkBindingValidation is the first thing dispatch does with every call.
func BenchmarkBindingValid(b *testing.B) {
	binding := benchBinding()
	b.ReportAllocs()
	for b.Loop() {
		if !binding.Valid() {
			b.Fatal("the fixture binding is invalid")
		}
	}
}

// BenchmarkRegistryGet is the name lookup every dispatched call performs.
func BenchmarkRegistryGet(b *testing.B) {
	registry, err := tool.NewRegistry([]tool.Source{sourceOf(
		namedTool("alpha", "a"), namedTool("beta", "b"), namedTool("gamma", "c"))})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := registry.Get("beta"); !ok {
			b.Fatal("the tool is missing")
		}
	}
}

// BenchmarkRegistryDefinitions measures the cached read a projection performs each round.
func BenchmarkRegistryDefinitions(b *testing.B) {
	registry, err := tool.NewRegistry([]tool.Source{sourceOf(
		namedTool("alpha", "a"), namedTool("beta", "b"), namedTool("gamma", "c"))})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if len(registry.Definitions()) != 3 {
			b.Fatal("definitions changed")
		}
	}
}

// BenchmarkDecodeArgs measures the fast path: the diagnostic machinery only runs when a
// decode fails, which is what makes a write_file body affordable.
func BenchmarkDecodeArgs(b *testing.B) {
	const payload = `{"path":"/srv/a.txt","limit":10}`
	b.ReportAllocs()
	for b.Loop() {
		var args readArgs
		if err := tool.DecodeArgs("read_file", payload, nil, &args); err != nil {
			b.Fatalf("DecodeArgs: %v", err)
		}
	}
}

// BenchmarkDecodeArgsUnknownKey measures the error path, which is the expensive half and
// therefore the one worth knowing about.
func BenchmarkDecodeArgsUnknownKey(b *testing.B) {
	const payload = `{"pth":"/srv/a.txt"}`
	b.ReportAllocs()
	for b.Loop() {
		var args readArgs
		if err := tool.DecodeArgs("read_file", payload, nil, &args); err == nil {
			b.Fatal("an unknown key was accepted")
		}
	}
}

// BenchmarkFuncToolExecute is the contract's floor: the wrapper a plain function costs.
func BenchmarkFuncToolExecute(b *testing.B) {
	impl := tool.FuncTool(message.ToolDefinition{
		Name:        "echo",
		InputSchema: []byte(`{"type":"object"}`),
	}, func(_ context.Context, args string) (message.Content, error) {
		return message.NewTextContent(args), nil
	})
	binding := benchBinding()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := impl.Execute(context.Background(), binding, "x"); err != nil {
			b.Fatal(err)
		}
	}
}
