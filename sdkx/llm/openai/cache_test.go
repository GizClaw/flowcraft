package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// TestComputePromptCacheKey_DeterministicAcrossRuns verifies the hash
// stays byte-stable for the same inputs. This is the contract that
// makes OpenAI's implicit prompt cache useful — drift here means
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

	first := computePromptCacheKey(msgs, tools)
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
		got := computePromptCacheKey(
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
	a := computePromptCacheKey(
		append([]llm.Message{}, append(stable, llm.NewTextMessage(llm.RoleUser, "hi"))...),
		nil,
	)
	b := computePromptCacheKey(
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
	a := computePromptCacheKey([]llm.Message{llm.NewTextMessage(llm.RoleSystem, "agent-A")}, nil)
	b := computePromptCacheKey([]llm.Message{llm.NewTextMessage(llm.RoleSystem, "agent-B")}, nil)
	if a == b {
		t.Fatalf("expected different keys for different system prompts, both = %q", a)
	}
}

// TestComputePromptCacheKey_ToolOrderAffectsKey reflects reality:
// OpenAI's prefix cache keys off the byte sequence of the request, and
// reordering tools changes that sequence. We don't try to be clever
// here — caller is expected to keep tool order stable.
func TestComputePromptCacheKey_ToolOrderAffectsKey(t *testing.T) {
	a := computePromptCacheKey(nil, []llm.ToolDefinition{
		{Name: "search"}, {Name: "calc"},
	})
	b := computePromptCacheKey(nil, []llm.ToolDefinition{
		{Name: "calc"}, {Name: "search"},
	})
	if a == b {
		t.Fatalf("expected different keys for reordered tools, both = %q", a)
	}
}

// TestComputePromptCacheKey_NoStableContentEmpty: if there are no
// system messages and no tools, there's nothing stable to anchor the
// cache to — return empty so buildParams omits the field.
func TestComputePromptCacheKey_NoStableContentEmpty(t *testing.T) {
	got := computePromptCacheKey(
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
		nil,
	)
	if got != "" {
		t.Fatalf("expected empty key, got %q", got)
	}
}

// TestComputePromptCacheKey_SchemaKeyOrderStable: Go map iteration is
// non-deterministic, but the cache key must be — canonicalJSON sorts
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
	a := computePromptCacheKey(nil, []llm.ToolDefinition{{Name: "t", InputSchema: schemaA}})
	b := computePromptCacheKey(nil, []llm.ToolDefinition{{Name: "t", InputSchema: schemaB}})
	if a != b {
		t.Fatalf("equivalent schemas hashed to different keys: a=%q b=%q", a, b)
	}
}

// TestBuildParams_InjectsPromptCacheKey is the end-to-end wire-level
// check: when a request has a stable system prompt, the rendered
// chat-completions request body contains a non-empty
// `prompt_cache_key` field. Without this the routing hint is lost
// even if computePromptCacheKey works.
func TestBuildParams_InjectsPromptCacheKey(t *testing.T) {
	var captured []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{
			"id": "x",
			"model": "gpt-test",
			"object": "chat.completion",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`)
	}))
	defer ts.Close()

	c, _ := New("gpt-test", "k", ts.URL)
	_, _, err := c.Generate(t.Context(),
		[]llm.Message{
			llm.NewTextMessage(llm.RoleSystem, "stable persona"),
			llm.NewTextMessage(llm.RoleUser, "first call"),
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var body struct {
		PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode body: %v\nbody=%s", err, captured)
	}
	if body.PromptCacheKey == "" {
		t.Fatalf("expected prompt_cache_key in request body, got empty\nbody=%s", captured)
	}
	if len(body.PromptCacheKey) != 16 {
		t.Errorf("expected 16-hex-char key, got %d chars: %q", len(body.PromptCacheKey), body.PromptCacheKey)
	}
}

// TestBuildParams_NoCacheKeyWhenNothingStable: a call with no system
// messages and no tools should NOT emit a prompt_cache_key — sending
// an empty / pointless value would just pollute the backend's
// routing table.
func TestBuildParams_NoCacheKeyWhenNothingStable(t *testing.T) {
	var captured []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{
			"id":"x","model":"gpt-test","object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer ts.Close()

	c, _ := New("gpt-test", "k", ts.URL)
	_, _, err := c.Generate(t.Context(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "just a user msg")},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Decode the body and check field is absent / empty.
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if v, ok := body["prompt_cache_key"]; ok && v != "" {
		t.Fatalf("expected no prompt_cache_key, got %v", v)
	}
}
