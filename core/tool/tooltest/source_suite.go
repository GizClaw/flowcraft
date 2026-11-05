package tooltest

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

// SourceFactory builds a fresh tool.Source for each subtest.
type SourceFactory func() tool.Source

// SourceSuite runs the contract every tool.Source implementation should
// pass.
func SourceSuite(t *testing.T, f SourceFactory) {
	t.Helper()
	t.Run("ToolsNoPanic", func(t *testing.T) { sourceToolsNoPanic(t, f) })
	t.Run("LazyToolsValid", func(t *testing.T) { sourceLazyToolsValid(t, f) })
	t.Run("EagerDefinitionsValid", func(t *testing.T) { sourceEagerDefinitionsValid(t, f) })
}

func sourceToolsNoPanic(t *testing.T, f SourceFactory) {
	t.Helper()
	src := f()
	if src == nil {
		t.Fatal("factory returned nil source")
	}
	defer recoverPanicAs(t, "Tools()")
	_ = src.Tools()
}

func sourceLazyToolsValid(t *testing.T, f SourceFactory) {
	t.Helper()
	src := f()
	defer recoverPanicAs(t, "LazyTools()")
	for _, lazy := range src.LazyTools() {
		if err := lazy.Validate(); err != nil {
			t.Fatalf("LazyTool invalid: %v", err)
		}
	}
}

func sourceEagerDefinitionsValid(t *testing.T, f SourceFactory) {
	t.Helper()
	src := f()
	for _, impl := range src.Tools() {
		if impl == nil {
			t.Fatal("source returned a nil tool")
		}
		if err := impl.Definition().Validate(); err != nil {
			t.Fatalf("tool definition invalid: %v", err)
		}
	}
}
