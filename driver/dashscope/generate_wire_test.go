package dashscope

import (
	"context"
	stdjson "encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func canonG(t *testing.T, v any) string {
	t.Helper()
	raw, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic any
	if err := stdjson.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, _ := stdjson.Marshal(generic)
	return string(out)
}

func genFixtures() []struct {
	name string
	spec ModelSpec
	req  inference.GenerateRequest
	// after building, the test sets shape
} {
	mk := func(name, api, maxParam string) ModelSpec {
		spec := ModelSpec{Name: "m", Kind: "generate", Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText}, Outputs: []message.PartKind{message.PartText},
		}}
		spec.Generate.API = api
		spec.Generate.MaxTokensParam = maxParam
		return spec
	}
	base := func(content message.Content) inference.GenerateRequest {
		return inference.GenerateRequest{
			Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "m"}},
			Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: content},
			Output: inference.OutputText,
		}
	}
	temp := 0.3
	topP := 0.9
	seed := int64(11)
	topK := 5
	rep := 1.1
	pres := 0.2
	par := true
	search := false
	budget := 512
	vl := true
	maxTokens := 256
	strict := false

	withTools := base(message.NewTextContent("hi"))
	toolset, err := inference.NewToolset([]message.ToolDefinition{{
		Name: "weather", Description: "look up", InputSchema: []byte(`{"type":"object"}`),
	}})
	if err != nil {
		panic(err)
	}
	withTools.Tools, err = toolset.Choosing(inference.ToolChoice{
		Mode: inference.ToolChoiceFunction, Name: "weather",
	})
	if err != nil {
		panic(err)
	}

	full := base(message.NewTextContent("hi"))
	full.Extensions = inference.Extensions{&GenerateOptions{
		Temperature: &temp, TopP: &topP, Seed: &seed, TopK: &topK,
		RepetitionPenalty: &rep, PresencePenalty: &pres, Stop: []string{"x"},
		ParallelToolCalls: &par, EnableSearch: &search, ThinkingBudget: &budget,
		VlHighResolutionImages: &vl, MaxOutputTokens: &maxTokens,
		ResponseFormat: &ResponseFormat{Kind: "json_schema", Name: "out", Schema: []byte(`{"type":"object"}`), Strict: strict},
	}}
	return []struct {
		name string
		spec ModelSpec
		req  inference.GenerateRequest
	}{
		{"text-unary", mk("m", "", ""), base(message.NewTextContent("hi"))},
		{"text-tools", mk("m", "", ""), withTools},
		{"text-full", mk("m", "", ""), full},
		{"multimodal-completion", mk("m", generateAPIMultimodal, maxTokensParamCompletion), full},
		{"json-object", mk("m", generateAPIMultimodal, ""), func() inference.GenerateRequest {
			r := base(message.NewTextContent("hi"))
			r.Extensions = inference.Extensions{&GenerateOptions{
				ResponseFormat:  &ResponseFormat{Kind: "json_object"},
				MaxOutputTokens: &maxTokens,
			}}
			return r
		}()},
	}
}

func TestGenerateWireBodies(t *testing.T) {
	want := map[string]string{
		"text-unary/unary":             `{"input":{"messages":[{"content":"hi","role":"user"}]},"model":"m","parameters":{"result_format":"message"}}`,
		"text-unary/stream":            `{"input":{"messages":[{"content":"hi","role":"user"}]},"model":"m","parameters":{"incremental_output":true,"result_format":"message"}}`,
		"text-tools/unary":             `{"input":{"messages":[{"content":"hi","role":"user"}]},"model":"m","parameters":{"result_format":"message","tool_choice":{"function":{"name":"weather"},"type":"function"},"tools":[{"function":{"description":"look up","name":"weather","parameters":{"type":"object"}},"type":"function"}]}}`,
		"text-tools/stream":            `{"input":{"messages":[{"content":"hi","role":"user"}]},"model":"m","parameters":{"incremental_output":true,"result_format":"message","tool_choice":{"function":{"name":"weather"},"type":"function"},"tools":[{"function":{"description":"look up","name":"weather","parameters":{"type":"object"}},"type":"function"}]}}`,
		"text-full/unary":              `{"input":{"messages":[{"content":"hi","role":"user"}]},"model":"m","parameters":{"enable_search":false,"max_tokens":256,"parallel_tool_calls":true,"presence_penalty":0.2,"repetition_penalty":1.1,"response_format":{"json_schema":{"name":"out","schema":{"type":"object"},"strict":false},"type":"json_schema"},"result_format":"message","seed":11,"stop":["x"],"temperature":0.3,"thinking_budget":512,"top_k":5,"top_p":0.9,"vl_high_resolution_images":true}}`,
		"text-full/stream":             `{"input":{"messages":[{"content":"hi","role":"user"}]},"model":"m","parameters":{"enable_search":false,"incremental_output":true,"max_tokens":256,"parallel_tool_calls":true,"presence_penalty":0.2,"repetition_penalty":1.1,"response_format":{"json_schema":{"name":"out","schema":{"type":"object"},"strict":false},"type":"json_schema"},"result_format":"message","seed":11,"stop":["x"],"temperature":0.3,"thinking_budget":512,"top_k":5,"top_p":0.9,"vl_high_resolution_images":true}}`,
		"multimodal-completion/unary":  `{"input":{"messages":[{"content":[{"text":"hi"}],"role":"user"}]},"model":"m","parameters":{"enable_search":false,"max_completion_tokens":256,"parallel_tool_calls":true,"presence_penalty":0.2,"repetition_penalty":1.1,"response_format":{"json_schema":{"name":"out","schema":{"type":"object"},"strict":false},"type":"json_schema"},"seed":11,"stop":["x"],"temperature":0.3,"thinking_budget":512,"top_k":5,"top_p":0.9,"vl_high_resolution_images":true}}`,
		"multimodal-completion/stream": `{"input":{"messages":[{"content":[{"text":"hi"}],"role":"user"}]},"model":"m","parameters":{"enable_search":false,"incremental_output":true,"max_completion_tokens":256,"parallel_tool_calls":true,"presence_penalty":0.2,"repetition_penalty":1.1,"response_format":{"json_schema":{"name":"out","schema":{"type":"object"},"strict":false},"type":"json_schema"},"seed":11,"stop":["x"],"stream":true,"temperature":0.3,"thinking_budget":512,"top_k":5,"top_p":0.9,"vl_high_resolution_images":true}}`,
		"json-object/unary":            `{"input":{"messages":[{"content":[{"text":"hi"}],"role":"user"}]},"model":"m","parameters":{"max_tokens":256,"response_format":{"type":"json_object"}}}`,
		"json-object/stream":           `{"input":{"messages":[{"content":[{"text":"hi"}],"role":"user"}]},"model":"m","parameters":{"incremental_output":true,"max_tokens":256,"response_format":{"type":"json_object"},"stream":true}}`,
	}
	for _, tc := range genFixtures() {
		for _, shape := range []protocol.Shape{protocol.ShapeUnary, protocol.ShapeStream} {
			key := tc.name + "/" + string(shape)
			compiled, err := compileGenerate(tc.spec, "m")(context.Background(), tc.req, shape)
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			got := canonG(t, generateRequest(compiled.Wire))
			expected, ok := want[key]
			if !ok {
				t.Fatalf("%s: no expectation", key)
			}
			if got != expected {
				t.Fatalf("%s drifted:\n got: %s\nwant: %s", key, got, expected)
			}
		}
	}
}

// TestMessageWireBodies pins the message shape per role. Each role has its own type,
// so this is where the presence rules live: an assistant turn with only tool calls
// sends no content key, one with neither sends an explicit empty string, and the two
// surfaces disagree on whether content is a string or an item array.
func TestMessageWireBodies(t *testing.T) {
	textOnly := ModelSpec{Name: "m", Kind: "generate"}
	textOnly.Capabilities.Inputs = []message.PartKind{message.PartText}

	withImage := ModelSpec{Name: "m", Kind: "generate"}
	withImage.Capabilities.Inputs = []message.PartKind{message.PartText, message.PartImage}

	source, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatalf("image source: %v", err)
	}

	cases := []struct {
		name       string
		turn       message.Message
		spec       ModelSpec
		multimodal bool
		want       string
	}{
		{
			name: "system",
			turn: message.NewTextMessage(message.RoleSystem, "be brief"),
			spec: textOnly,
			want: `[{"content":"be brief","role":"system"}]`,
		},
		{
			name: "user text surface keeps a bare string",
			turn: message.NewTextMessage(message.RoleUser, "hi"),
			spec: textOnly,
			want: `[{"content":"hi","role":"user"}]`,
		},
		{
			name: "user multimodal becomes an item array",
			turn: message.Message{Role: message.RoleUser, Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "what is this"},
				message.ImagePart{Source: source},
			}}},
			spec:       withImage,
			multimodal: true,
			want:       `[{"content":[{"text":"what is this"},{"image":"https://example.com/a.png"}],"role":"user"}]`,
		},
		{
			name: "assistant with text",
			turn: message.NewTextMessage(message.RoleAssistant, "answer"),
			spec: textOnly,
			want: `[{"content":"answer","role":"assistant"}]`,
		},
		{
			name: "assistant with only tool calls sends no content",
			turn: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
				message.ToolCallPart{Call: message.ToolCall{
					ID: "c1", Name: "weather", Arguments: message.NewArguments([]byte(`{"city":"x"}`)),
				}},
			}}},
			spec: textOnly,
			want: `[{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\":\"x\"}","name":"weather"},"id":"c1","type":"function"}]}]`,
		},
		{
			name: "assistant with neither content nor calls sends an empty string",
			turn: message.Message{Role: message.RoleAssistant},
			spec: textOnly,
			want: `[{"content":"","role":"assistant"}]`,
		},
		{
			name: "tool result flattens to text with its call id",
			turn: message.Message{Role: message.RoleTool, Content: message.Content{Parts: []message.Part{
				message.ToolResultPart{Result: message.ToolResult{
					CallID: "c1", Content: message.NewTextContent("sunny"),
				}},
			}}},
			spec: textOnly,
			want: `[{"content":"sunny","role":"tool","tool_call_id":"c1"}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warnings []inference.Warning
			turns, err := lowerTurn(tc.turn, tc.spec, tc.multimodal, &warnings)
			if err != nil {
				t.Fatalf("lowerTurn: %v", err)
			}
			if got := canonicalBody(t, turns); got != tc.want {
				t.Fatalf("message drifted:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
