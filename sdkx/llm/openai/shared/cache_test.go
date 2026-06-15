package shared

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestComputePromptCacheKey_DeterministicAcrossRuns verifies the hash
// stays byte-stable for the same inputs. This is the contract that
// makes OpenAI's implicit prompt cache useful - drift here means
// every call lands on a fresh backend node.
func TestComputePromptCacheKey_DeterministicAcrossRuns(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "persona"),
		llm.NewTextMessage(llm.RoleSystem, "rules"),
		llm.NewTextMessage(llm.RoleUser, "this varies"),
	}
	tools := []llm.ToolDefinition{
		{Name: "search", Description: "look stuff up", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"q": map[string]any{"type": "string"}},
		}},
	}

	first := ComputePromptCacheKey(msgs, tools)
	if first == "" {
		t.Fatal("expected non-empty key")
	}
	if got := len(first); got != 16 {
		t.Fatalf("expected 16-hex-char key, got %d chars: %q", got, first)
	}

	// Re-running the same inputs produces the same key. Re-build the
	// inputs from scratch (different slice identity) to make sure
	// the hash is value-based, not pointer-based.
	for i := 0; i < 5; i++ {
		got := ComputePromptCacheKey(
			[]llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "persona"),
				llm.NewTextMessage(llm.RoleSystem, "rules"),
				llm.NewTextMessage(llm.RoleUser, "this varies"),
			},
			[]llm.ToolDefinition{
				{Name: "search", Description: "look stuff up", InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"type": "string"}},
				}},
			},
		)
		if got != first {
			t.Fatalf("iteration %d drift: got %q, want %q", i, got, first)
		}
	}
}

// TestComputePromptCacheKey_VolatileUserDoesNotChangeKey is the
// rationale behind excluding non-system messages from the hash: a
// turn-varying user prompt must NOT shift the routing key, otherwise
// every multi-turn call lands on a fresh backend node.
func TestComputePromptCacheKey_VolatileUserDoesNotChangeKey(t *testing.T) {
	stable := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "persona"),
	}
	a := ComputePromptCacheKey(
		append([]llm.Message{}, append(stable, llm.NewTextMessage(llm.RoleUser, "hi"))...),
		nil,
	)
	b := ComputePromptCacheKey(
		append([]llm.Message{}, append(stable, llm.NewTextMessage(llm.RoleUser, "completely different query"))...),
		nil,
	)
	if a != b {
		t.Fatalf("user message changed routing key: a=%q b=%q", a, b)
	}
}

// TestComputePromptCacheKey_DifferentSystemChangesKey is the dual: a
// genuinely different stable prefix must produce a different key, so
// two different agents don't pollute each other's cache slot on the
// backend.
func TestComputePromptCacheKey_DifferentSystemChangesKey(t *testing.T) {
	a := ComputePromptCacheKey([]llm.Message{llm.NewTextMessage(llm.RoleSystem, "agent-A")}, nil)
	b := ComputePromptCacheKey([]llm.Message{llm.NewTextMessage(llm.RoleSystem, "agent-B")}, nil)
	if a == b {
		t.Fatalf("expected different keys for different system prompts, both = %q", a)
	}
}

// TestComputePromptCacheKey_ToolOrderAffectsKey reflects reality:
// OpenAI's prefix cache keys off the byte sequence of the request, and
// reordering tools changes that sequence. We don't try to be clever
// here - caller is expected to keep tool order stable.
func TestComputePromptCacheKey_ToolOrderAffectsKey(t *testing.T) {
	a := ComputePromptCacheKey(nil, []llm.ToolDefinition{
		{Name: "search"}, {Name: "calc"},
	})
	b := ComputePromptCacheKey(nil, []llm.ToolDefinition{
		{Name: "calc"}, {Name: "search"},
	})
	if a == b {
		t.Fatalf("expected different keys for reordered tools, both = %q", a)
	}
}

// TestComputePromptCacheKey_NoStableContentEmpty: if there are no
// system messages and no tools, there's nothing stable to anchor the
// cache to - return empty so buildParams omits the field.
func TestComputePromptCacheKey_NoStableContentEmpty(t *testing.T) {
	got := ComputePromptCacheKey(
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
		nil,
	)
	if got != "" {
		t.Fatalf("expected empty key, got %q", got)
	}
}

// TestComputePromptCacheKey_SchemaKeyOrderStable: Go map iteration is
// non-deterministic, but the cache key must be - canonicalJSON sorts
// keys so two equivalent schemas hash identically.
func TestComputePromptCacheKey_SchemaKeyOrderStable(t *testing.T) {
	// Build schemas with the same logical content but different
	// in-memory key insertion order. Both should hash identically.
	schemaA := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"z": map[string]any{"type": "number"},
		},
		"required": []string{"a"},
	}
	schemaB := map[string]any{
		"required": []string{"a"},
		"properties": map[string]any{
			"z": map[string]any{"type": "number"},
			"a": map[string]any{"type": "string"},
		},
		"type": "object",
	}
	a := ComputePromptCacheKey(nil, []llm.ToolDefinition{{Name: "t", InputSchema: schemaA}})
	b := ComputePromptCacheKey(nil, []llm.ToolDefinition{{Name: "t", InputSchema: schemaB}})
	if a != b {
		t.Fatalf("equivalent schemas hashed to different keys: a=%q b=%q", a, b)
	}
}
