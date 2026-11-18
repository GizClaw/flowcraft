package tool_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func describe(size int) string { return strings.Repeat("a", size) }

func descriptionRegistry(t *testing.T, description string) (*tool.Registry, tool.Tool) {
	t.Helper()
	impl := tool.TextTool(
		message.ToolDefinition{
			Name:        "big",
			Description: description,
			InputSchema: []byte(`{"type":"object"}`),
		},
		func(context.Context, string) (string, error) { return "ok", nil },
	)
	registry, err := tool.NewRegistry([]tool.Source{source{tools: []tool.Tool{impl}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry, impl
}

// TestDefinitionsCapThirdPartyDescription pins the projection cap: a description
// is host- or server-supplied text, every reader of Definitions pays for it, and
// the cap belongs where they meet.
func TestDefinitionsCapThirdPartyDescription(t *testing.T) {
	original := describe(3 * tool.MaxDescriptionBytes)
	registry, impl := descriptionRegistry(t, original)

	defs := registry.Definitions()
	if len(defs) != 1 {
		t.Fatalf("definitions = %d, want 1", len(defs))
	}
	capped := defs[0].Description
	if len(capped) > tool.MaxDescriptionBytes {
		t.Fatalf("description = %d bytes, want <= %d", len(capped), tool.MaxDescriptionBytes)
	}
	if !strings.HasSuffix(capped, tool.DescriptionEllipsis) {
		t.Fatalf("description does not end with the marker: %q", capped[max(0, len(capped)-8):])
	}
	// Only the projection is capped: the tool keeps saying what its author wrote,
	// so a host that reads Tool.Definition directly is not lied to.
	if got := impl.Definition().Description; got != original {
		t.Fatalf("Tool.Definition was modified: %d bytes, want %d", len(got), len(original))
	}
	if got := string(defs[0].InputSchema); got != `{"type":"object"}` {
		t.Fatalf("schema = %q, want it untouched", got)
	}
	// The cap is part of the cached list, so a rebuild reports the same text.
	if again := registry.Definitions()[0].Description; again != capped {
		t.Fatalf("second Definitions() = %d bytes, want the same %d", len(again), len(capped))
	}
}

// TestDefinitionsCapKeepsRunesWhole: the cut is in bytes, so a multibyte
// description must not end in half a rune.
func TestDefinitionsCapKeepsRunesWhole(t *testing.T) {
	registry, _ := descriptionRegistry(t, strings.Repeat("世", 2*tool.MaxDescriptionBytes))
	capped := registry.Definitions()[0].Description
	if len(capped) > tool.MaxDescriptionBytes {
		t.Fatalf("description = %d bytes, want <= %d", len(capped), tool.MaxDescriptionBytes)
	}
	if !utf8.ValidString(capped) {
		t.Fatal("the capped description is not valid UTF-8")
	}
	if !strings.HasSuffix(capped, tool.DescriptionEllipsis) {
		t.Fatal("the capped description does not end with the marker")
	}
}

// TestDefinitionsLeaveAShortDescriptionAlone: the cap must be invisible below it.
func TestDefinitionsLeaveAShortDescriptionAlone(t *testing.T) {
	for _, size := range []int{0, 1, tool.MaxDescriptionBytes - len(tool.DescriptionEllipsis), tool.MaxDescriptionBytes} {
		original := describe(size)
		registry, _ := descriptionRegistry(t, original)
		if got := registry.Definitions()[0].Description; got != original {
			t.Errorf("description of %d bytes became %d; the cap is not a rewrite", size, len(got))
		}
	}
}
