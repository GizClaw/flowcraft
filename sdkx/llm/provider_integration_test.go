//go:build integration

package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdkx/internal/testenv"

	// trigger init() self-registration for all providers
	_ "github.com/GizClaw/flowcraft/sdkx/llm/anthropic"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

// providerSpec describes a provider under test.
// Env is the single env var containing a JSON config:
//
//	{"provider":"azure","api_key":"...","model":"gpt-5","base_url":"...","caps":{"no_temperature":true}}
type providerSpec struct {
	Provider string // llm.RegisterProvider name
	Env      string // env var holding the JSON config
}

var providers = []providerSpec{
	{Provider: "minimax", Env: "FLOWCRAFT_TEST_MINIMAX"},
	{Provider: "qwen", Env: "FLOWCRAFT_TEST_QWEN"},
	{Provider: "bytedance", Env: "FLOWCRAFT_TEST_BYTEDANCE"},
	{Provider: "azure", Env: "FLOWCRAFT_TEST_AZURE"},
	{Provider: "deepseek", Env: "FLOWCRAFT_TEST_DEEPSEEK"},
}

func init() {
	testenv.Load()
}

func defaultOpts(maxTokens int64) []llm.GenerateOption {
	return []llm.GenerateOption{llm.WithMaxTokens(maxTokens), llm.WithTemperature(0.7)}
}

// parseSpecConfig parses the JSON config from the env var.
// Returns nil if the env var is not set or empty.
func parseSpecConfig(spec providerSpec) map[string]any {
	raw := os.Getenv(spec.Env)
	if raw == "" {
		return nil
	}
	var config map[string]any
	if json.Unmarshal([]byte(raw), &config) != nil {
		return nil
	}
	return config
}

// capsHas checks whether a provider's config contains a truthy caps value
// for the given key (e.g. "no_temperature").
func capsHas(spec providerSpec, key string) bool {
	config := parseSpecConfig(spec)
	if config == nil {
		return false
	}
	caps, _ := config["caps"].(map[string]any)
	v, _ := caps[key].(bool)
	return v
}

func createProvider(t *testing.T, spec providerSpec) llm.LLM {
	t.Helper()
	config := parseSpecConfig(spec)
	if config == nil {
		t.Skipf("skip %s: %s not set", spec.Provider, spec.Env)
	}
	model, _ := config["model"].(string)
	provider, err := llm.NewFromConfig(spec.Provider, model, config)
	if err != nil {
		t.Fatalf("create %s provider: %v", spec.Provider, err)
	}
	return provider
}

// ---------------------------------------------------------------------------
// Scenario 1: basic single-turn – simplest "hi" message
// ---------------------------------------------------------------------------

func TestProviders_BasicGenerate(t *testing.T) {
	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "Reply in one short sentence."),
				llm.NewTextMessage(llm.RoleUser, "hi"),
			}
			resp, usage, err := provider.Generate(ctx, msgs, llm.WithMaxTokens(100))
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			t.Logf("response=%q tokens=%+v", resp.Content(), usage)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: streaming single-turn
// ---------------------------------------------------------------------------

func TestProviders_BasicStream(t *testing.T) {
	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "Reply in one short sentence."),
				llm.NewTextMessage(llm.RoleUser, "hi"),
			}
			stream, err := provider.GenerateStream(ctx, msgs, llm.WithMaxTokens(100))
			if err != nil {
				t.Fatalf("GenerateStream error: %v", err)
			}
			defer stream.Close()
			var content strings.Builder
			for stream.Next() {
				content.WriteString(stream.Current().Content)
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream error: %v", err)
			}
			t.Logf("streamed=%q tokens=%+v", content.String(), stream.Usage())
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: kanban callback message (the exact format used in production)
// ---------------------------------------------------------------------------

func TestProviders_KanbanCallbackMessage(t *testing.T) {
	callbackQuery := `[Task Callback] card_id=abc123

Target Agent: my-worker-agent
Status: completed
Summary: The task has been completed successfully.

Use task_context(card_id="abc123") to recall the original request and your dispatch note.`

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "You are a project coordinator. When you receive task callbacks, summarize the result for the user."),
				llm.NewTextMessage(llm.RoleUser, callbackQuery),
			}
			resp, usage, err := provider.Generate(ctx, msgs, defaultOpts(200)...)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			t.Logf("response=%q tokens=%+v", resp.Content(), usage)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: kanban callback via streaming (matches production path)
// ---------------------------------------------------------------------------

func TestProviders_KanbanCallbackStream(t *testing.T) {
	callbackQuery := `[Task Callback] card_id=abc123

Target Agent: my-worker-agent
Status: completed
Summary: The task has been completed successfully.

Use task_context(card_id="abc123") to recall the original request and your dispatch note.`

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "You are a project coordinator. When you receive task callbacks, summarize the result for the user."),
				llm.NewTextMessage(llm.RoleUser, callbackQuery),
			}
			stream, err := provider.GenerateStream(ctx, msgs, defaultOpts(200)...)
			if err != nil {
				t.Fatalf("GenerateStream error: %v", err)
			}
			defer stream.Close()
			var content strings.Builder
			for stream.Next() {
				content.WriteString(stream.Current().Content)
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream error: %v", err)
			}
			t.Logf("streamed=%q tokens=%+v", content.String(), stream.Usage())
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: multi-turn with consecutive assistant messages
// (the graph bug that caused duplicate "general_query" outputs)
// ---------------------------------------------------------------------------

func TestProviders_ConsecutiveAssistantMessages(t *testing.T) {
	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "Reply in one short sentence."),
				llm.NewTextMessage(llm.RoleUser, "hi"),
				llm.NewTextMessage(llm.RoleAssistant, "general_query"),
				llm.NewTextMessage(llm.RoleAssistant, "general_query"),
				llm.NewTextMessage(llm.RoleUser, "hello again"),
			}
			stream, err := provider.GenerateStream(ctx, msgs, defaultOpts(100)...)
			if err != nil {
				t.Fatalf("GenerateStream error: %v", err)
			}
			defer stream.Close()
			var content strings.Builder
			for stream.Next() {
				content.WriteString(stream.Current().Content)
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream error: %v", err)
			}
			t.Logf("streamed=%q tokens=%+v", content.String(), stream.Usage())
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: multi-turn with tool calls (copilot dispatcher pattern)
// ---------------------------------------------------------------------------

func TestProviders_WithToolCalls(t *testing.T) {
	tools := []llm.ToolDefinition{
		{
			Name:        "kanban_submit",
			Description: "Submit a task to a target agent",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_agent_id": map[string]any{"type": "string", "description": "Target agent ID"},
					"query":           map[string]any{"type": "string", "description": "Task description"},
				},
				"required": []string{"target_agent_id", "query"},
			},
		},
		{
			Name:        "task_context",
			Description: "Retrieve the full context of a dispatched task",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card_id": map[string]any{"type": "string", "description": "Card ID"},
				},
			},
		},
	}

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "You are a dispatcher. Use kanban_submit to delegate tasks."),
				llm.NewTextMessage(llm.RoleUser, "Please create a project plan for a mobile app"),
			}
			opts := append(defaultOpts(500), llm.WithTools(tools...), llm.WithToolChoiceAuto())
			resp, usage, err := provider.Generate(ctx, msgs, opts...)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			t.Logf("has_tool_calls=%v content=%q tokens=%+v", resp.HasToolCalls(), resp.Content(), usage)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 7: temperature edge cases for minimaxi
// minimaxi requires temperature in (0.0, 1.0], 0.0 is NOT allowed.
// ---------------------------------------------------------------------------

func TestProviders_TemperatureEdgeCases(t *testing.T) {
	temps := []struct {
		name string
		temp float64
	}{
		{"temp_0.01", 0.01},
		{"temp_0.5", 0.5},
		{"temp_1.0", 1.0},
	}

	for _, spec := range providers {
		if capsHas(spec, "no_temperature") {
			t.Run(spec.Provider, func(t *testing.T) {
				t.Skipf("skip %s: model does not support temperature", spec.Provider)
			})
			continue
		}
		for _, tt := range temps {
			name := fmt.Sprintf("%s/%s", spec.Provider, tt.name)
			t.Run(name, func(t *testing.T) {
				provider := createProvider(t, spec)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				msgs := []llm.Message{
					llm.NewTextMessage(llm.RoleSystem, "Reply with OK."),
					llm.NewTextMessage(llm.RoleUser, "test"),
				}
				resp, _, err := provider.Generate(ctx, msgs, llm.WithTemperature(tt.temp), llm.WithMaxTokens(10))
				if err != nil {
					t.Fatalf("Generate error at temperature=%v: %v", tt.temp, err)
				}
				t.Logf("temp=%v response=%q", tt.temp, resp.Content())
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 8: tool_result + text in same user message (kanban callback merge)
// ---------------------------------------------------------------------------

func TestProviders_MixedToolResultAndText(t *testing.T) {
	tools := []llm.ToolDefinition{
		{
			Name:        "task_context",
			Description: "Retrieve the full context of a dispatched task",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"card_id": map[string]any{"type": "string"}},
			},
		},
	}

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "You are a dispatcher. Summarize results."),
				llm.NewTextMessage(llm.RoleUser, "Check task abc123"),
				llm.NewToolCallMessage([]llm.ToolCall{
					{ID: "call_001", Name: "task_context", Arguments: `{"card_id":"abc123"}`},
				}),
				llm.NewToolResultMessage([]llm.ToolResult{
					{ToolCallID: "call_001", Content: `{"status":"claimed"}`},
				}),
				llm.NewTextMessage(llm.RoleUser, `[Task Callback] card_id=abc123

Target Agent: worker
Status: completed
Summary: Done.`),
			}
			opts := append(defaultOpts(200), llm.WithTools(tools...))
			resp, usage, err := provider.Generate(ctx, msgs, opts...)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			t.Logf("response=%q tokens=%+v", resp.Content(), usage)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 9: large number of messages (simulates long copilot session)
// ---------------------------------------------------------------------------

func TestProviders_LargeMessageCount(t *testing.T) {
	tools := []llm.ToolDefinition{
		{
			Name:        "kanban_submit",
			Description: "Submit a task to a target agent",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_agent_id": map[string]any{"type": "string"},
					"query":           map[string]any{"type": "string"},
				},
				"required": []string{"target_agent_id", "query"},
			},
		},
	}

	counts := []int{50, 100, 200}
	for _, spec := range providers {
		for _, n := range counts {
			name := fmt.Sprintf("%s/%d_messages", spec.Provider, n)
			t.Run(name, func(t *testing.T) {
				provider := createProvider(t, spec)
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				msgs := []llm.Message{
					llm.NewTextMessage(llm.RoleSystem, "You are a dispatcher. Reply briefly."),
				}
				for i := 0; i < n; i++ {
					msgs = append(msgs,
						llm.NewTextMessage(llm.RoleUser, fmt.Sprintf("Check task %d", i)),
						llm.NewToolCallMessage([]llm.ToolCall{
							{ID: fmt.Sprintf("call_%d", i), Name: "kanban_submit", Arguments: fmt.Sprintf(`{"target_agent_id":"agent-%d","query":"task %d"}`, i, i)},
						}),
						llm.NewToolResultMessage([]llm.ToolResult{
							{ToolCallID: fmt.Sprintf("call_%d", i), Content: fmt.Sprintf(`{"card_id":"card_%d","status":"submitted"}`, i)},
						}),
					)
				}
				msgs = append(msgs, llm.NewTextMessage(llm.RoleUser, "Summarize all tasks in one sentence."))

				opts := append(defaultOpts(100), llm.WithTools(tools...))
				resp, usage, err := provider.Generate(ctx, msgs, opts...)
				if err != nil {
					t.Fatalf("Generate error with %d message rounds: %v", n, err)
				}
				t.Logf("rounds=%d response=%q tokens=%+v", n, resp.Content()[:min(80, len(resp.Content()))], usage)
			})
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Scenario 11: Qwen thinking mode (enable_thinking)
// ---------------------------------------------------------------------------

func TestProviders_QwenThinking(t *testing.T) {
	spec := providerSpec{Provider: "qwen", Env: "FLOWCRAFT_TEST_QWEN"}
	provider := createProvider(t, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a helpful assistant."),
		llm.NewTextMessage(llm.RoleUser, "What is 15 * 37? Show your reasoning briefly."),
	}
	resp, usage, err := provider.Generate(ctx, msgs, llm.WithMaxTokens(300))
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	t.Logf("qwen-thinking response=%q tokens=%+v", resp.Content(), usage)
}

// ---------------------------------------------------------------------------
// Scenario 12: DeepSeek thinking mode
// ---------------------------------------------------------------------------

func TestProviders_DeepSeekThinking(t *testing.T) {
	spec := providerSpec{Provider: "deepseek", Env: "FLOWCRAFT_TEST_DEEPSEEK"}
	provider := createProvider(t, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a helpful assistant."),
		llm.NewTextMessage(llm.RoleUser, "What is 15 * 37? Show your reasoning briefly."),
	}
	resp, usage, err := provider.Generate(ctx, msgs, llm.WithMaxTokens(300))
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	t.Logf("deepseek response=%q tokens=%+v", resp.Content(), usage)
}

// ---------------------------------------------------------------------------
// Scenario 13: Anthropic JSON mode (Beta API) – Generate
// ---------------------------------------------------------------------------

func TestProviders_AnthropicJSONMode(t *testing.T) {
	candidates := []providerSpec{
		{Provider: "anthropic", Env: "FLOWCRAFT_TEST_ANTHROPIC"},
		{Provider: "minimax", Env: "FLOWCRAFT_TEST_MINIMAX"},
	}
	var spec providerSpec
	found := false
	for _, c := range candidates {
		if parseSpecConfig(c) != nil {
			spec = c
			found = true
			break
		}
	}
	if !found {
		t.Skipf("skip: neither FLOWCRAFT_TEST_ANTHROPIC nor FLOWCRAFT_TEST_MINIMAX set")
	}
	provider := createProvider(t, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, `You are a JSON formatter. Return a JSON object with fields: "name" (string), "items" (array of strings). Output ONLY valid JSON, no markdown.`),
		llm.NewTextMessage(llm.RoleUser, "List 3 fruits."),
	}
	resp, usage, err := provider.Generate(ctx, msgs, llm.WithMaxTokens(200), llm.WithJSONMode(true))
	if err != nil {
		t.Fatalf("[%s] Generate error: %v", spec.Provider, err)
	}
	content := strings.TrimSpace(resp.Content())
	t.Logf("[%s] json-mode response=%q tokens=%+v", spec.Provider, content, usage)
	if !strings.HasPrefix(content, "{") {
		t.Errorf("[%s] expected JSON object, got %q", spec.Provider, content)
	}
}

// ---------------------------------------------------------------------------
// Scenario 14: Anthropic JSON mode (Beta API) – Stream
// ---------------------------------------------------------------------------

func TestProviders_AnthropicJSONModeStream(t *testing.T) {
	candidates := []providerSpec{
		{Provider: "anthropic", Env: "FLOWCRAFT_TEST_ANTHROPIC"},
		{Provider: "minimax", Env: "FLOWCRAFT_TEST_MINIMAX"},
	}
	var spec providerSpec
	found := false
	for _, c := range candidates {
		if parseSpecConfig(c) != nil {
			spec = c
			found = true
			break
		}
	}
	if !found {
		t.Skipf("skip: neither FLOWCRAFT_TEST_ANTHROPIC nor FLOWCRAFT_TEST_MINIMAX set")
	}
	provider := createProvider(t, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, `Return a JSON object with fields: "count" (number), "items" (array of strings). Output ONLY valid JSON, no markdown.`),
		llm.NewTextMessage(llm.RoleUser, "List 2 colors."),
	}
	stream, err := provider.GenerateStream(ctx, msgs, llm.WithMaxTokens(200), llm.WithJSONMode(true))
	if err != nil {
		t.Fatalf("[%s] GenerateStream error: %v", spec.Provider, err)
	}
	defer stream.Close()
	var content strings.Builder
	for stream.Next() {
		content.WriteString(stream.Current().Content)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("[%s] stream error: %v", spec.Provider, err)
	}
	result := strings.TrimSpace(content.String())
	t.Logf("[%s] json-mode-stream result=%q tokens=%+v", spec.Provider, result, stream.Usage())
	if !strings.HasPrefix(result, "{") {
		t.Errorf("[%s] expected JSON object from stream, got %q", spec.Provider, result)
	}
}

// ---------------------------------------------------------------------------
// Scenario 15: OpenAI JSON mode – Generate (compare with Anthropic Beta)
// ---------------------------------------------------------------------------

func TestProviders_OpenAIJSONMode(t *testing.T) {
	jsonProviders := []providerSpec{
		{Provider: "qwen", Env: "FLOWCRAFT_TEST_QWEN"},
		{Provider: "azure", Env: "FLOWCRAFT_TEST_AZURE"},
		{Provider: "deepseek", Env: "FLOWCRAFT_TEST_DEEPSEEK"},
	}
	for _, spec := range jsonProviders {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, `Return a JSON object with fields: "answer" (string), "confidence" (number 0-1).`),
				llm.NewTextMessage(llm.RoleUser, "What is the capital of France?"),
			}
			opts := append(defaultOpts(200), llm.WithJSONMode(true))
			resp, usage, err := provider.Generate(ctx, msgs, opts...)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			content := strings.TrimSpace(resp.Content())
			t.Logf("json-mode response=%q tokens=%+v", content, usage)
			if !strings.HasPrefix(content, "{") {
				t.Errorf("expected JSON object, got %q", content)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 10: full copilot dispatcher flow with tool round-trip
// ---------------------------------------------------------------------------

func TestProviders_ToolRoundTrip(t *testing.T) {
	tools := []llm.ToolDefinition{
		{
			Name:        "task_context",
			Description: "Retrieve the full context of a dispatched task",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card_id": map[string]any{"type": "string", "description": "Card ID"},
				},
			},
		},
	}

	for _, spec := range providers {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleSystem, "You are a helpful assistant. Check the task status and report back."),
				llm.NewTextMessage(llm.RoleUser, "What is the status of card abc123?"),
				llm.NewToolCallMessage([]llm.ToolCall{
					{ID: "call_001", Name: "task_context", Arguments: `{"card_id":"abc123"}`},
				}),
				llm.NewToolResultMessage([]llm.ToolResult{
					{ToolCallID: "call_001", Content: `{"card_id":"abc123","status":"done","result":{"output":"Task completed successfully"}}`},
				}),
			}
			opts := append(defaultOpts(200), llm.WithTools(tools...))
			resp, usage, err := provider.Generate(ctx, msgs, opts...)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			t.Logf("response=%q tokens=%+v", resp.Content(), usage)
		})
	}
}
