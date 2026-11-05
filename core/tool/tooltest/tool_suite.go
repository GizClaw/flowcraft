package tooltest

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/tool"
)

// ToolFactory builds a fresh tool.Tool for each subtest.
type ToolFactory func() tool.Tool

// ToolSuite runs the contract every tool.Tool implementation should pass.
func ToolSuite(t *testing.T, f ToolFactory) {
	t.Helper()
	t.Run("DefinitionStableAndValid", func(t *testing.T) { toolDefinitionStableAndValid(t, f) })
	t.Run("EmptyArgumentsNoPanic", func(t *testing.T) { toolEmptyArgumentsNoPanic(t, f) })
	t.Run("CancelledContextPrompt", func(t *testing.T) { toolCancelledContextPrompt(t, f) })
	t.Run("MetadataZeroSafe", func(t *testing.T) { toolMetadataZeroSafe(t, f) })
	t.Run("ConcurrentSafe", func(t *testing.T) { toolConcurrentSafe(t, f) })
}

func toolDefinitionStableAndValid(t *testing.T, f ToolFactory) {
	t.Helper()
	impl := f()
	if impl == nil {
		t.Fatal("factory returned nil tool")
	}
	first := impl.Definition()
	if err := first.Validate(); err != nil {
		t.Fatalf("Definition() invalid: %v", err)
	}
	second := impl.Definition()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Definition() not stable:\nfirst: %+v\nsecond: %+v", first, second)
	}
}

func toolEmptyArgumentsNoPanic(t *testing.T, f ToolFactory) {
	t.Helper()
	impl := f()
	defer recoverPanicAs(t, "Execute(context.Background(), \"\")")
	_, _ = impl.Execute(context.Background(), "")
}

func toolCancelledContextPrompt(t *testing.T, f ToolFactory) {
	t.Helper()
	impl := f()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverPanicAs(t, "Execute(cancelled context)")
		_, _ = impl.Execute(ctx, "")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled context did not stop tool execution promptly")
	}
}

func toolMetadataZeroSafe(t *testing.T, f ToolFactory) {
	t.Helper()
	impl := f()
	defer recoverPanicAs(t, "MetadataOf")
	_ = tool.MetadataOf(impl)
}

func toolConcurrentSafe(t *testing.T, f ToolFactory) {
	t.Helper()
	impl := f()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverPanicAs(t, "concurrent Execute")
			_, _ = impl.Execute(context.Background(), `{"x":1}`)
			_ = impl.Definition()
			_ = tool.MetadataOf(impl)
		}()
	}
	wg.Wait()
}

func recoverPanicAs(t *testing.T, label string) {
	t.Helper()
	if recovered := recover(); recovered != nil {
		t.Fatalf("%s panicked: %v", label, recovered)
	}
}
