package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"

	"github.com/openai/openai-go/v3/responses"
)

func TestGenerateOptionsWebSearchCompilesToHostedTool(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{
		GenerateOptions{
			WebSearch: &GenerateWebSearch{
				SearchContextSize: "high",
				AllowedDomains:    []string{"example.com"},
				UserLocation: GenerateWebSearchLocation{
					City:    "Hangzhou",
					Country: "CN",
				},
				ExternalWebAccess: boolPointer(false),
				ReturnTokenBudget: "unlimited",
				ToolChoice: &GenerateWebSearchToolChoice{
					Required: true,
				},
			},
		},
	}
	wire := compileTextWire(t, request)
	if wire.webSearch == nil {
		t.Fatal("web search extension did not reach the wire")
	}
	params := wireToParams(wire)
	if len(params.Tools) != 1 || params.Tools[0].OfWebSearch == nil {
		t.Fatalf("tools = %+v, want one hosted web_search tool", params.Tools)
	}
	if params.ToolChoice.OfToolChoiceMode.Value != responses.ToolChoiceOptionsRequired {
		t.Fatalf("tool choice = %+v, want required", params.ToolChoice)
	}
	found := false
	for _, include := range params.Include {
		if include == responses.ResponseIncludableWebSearchCallActionSources {
			found = true
		}
	}
	if !found {
		t.Fatalf("include = %v, want web_search_call.action.sources", params.Include)
	}

	raw, err := json.Marshal(params.Tools[0])
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		`"type":"web_search"`,
		`"search_context_size":"high"`,
		`"allowed_domains":["example.com"]`,
		`"external_web_access":false`,
		`"return_token_budget":"unlimited"`,
		`"city":"Hangzhou"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("tool JSON %s does not contain %s", body, want)
		}
	}
}

func TestOpenAIWebSearchCallDecoding(t *testing.T) {
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(`{
		"id":"ws_1",
		"type":"web_search_call",
		"status":"completed",
		"action":{
			"type":"search",
			"queries":["flowcraft"],
			"sources":[{"type":"url","url":"https://example.com"}]
		}
	}`), &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	call := openaiWebSearchCall(item)
	if call.ID != "ws_1" || call.Status != "completed" ||
		call.Action != "search" || len(call.Queries) != 1 ||
		call.Queries[0] != "flowcraft" || len(call.Sources) != 1 ||
		call.Sources[0] != "https://example.com" {
		t.Fatalf("call = %+v", call)
	}
}

func TestOpenAIAnnotationCitationDecoding(t *testing.T) {
	citation, ok := openaiAnnotationCitation(map[string]any{
		"type":        "url_citation",
		"url":         "https://example.com",
		"title":       "Example",
		"start_index": 12,
		"end_index":   34,
	})
	if !ok {
		t.Fatal("url citation not recognized")
	}
	if citation.URL != "https://example.com" ||
		citation.Title != "Example" ||
		citation.StartIndex == nil || *citation.StartIndex != 12 ||
		citation.EndIndex == nil || *citation.EndIndex != 34 {
		t.Fatalf("citation = %+v", citation)
	}
}

func TestDecodeGenerateCarriesWebSearchProviderOutput(t *testing.T) {
	response, err := decodeGenerate(context.Background(), generateRaw{
		webSearchCalls: []inference.WebSearchCall{{ID: "ws_1"}},
		citations:      []inference.Citation{{URL: "https://example.com"}},
		finish:         inference.FinishCompleted,
	})
	if err != nil {
		t.Fatalf("decodeGenerate: %v", err)
	}
	if len(response.ProviderOutputs) != 1 {
		t.Fatalf("provider outputs = %+v, want 1", response.ProviderOutputs)
	}
	output, ok := response.ProviderOutputs[0].(*WebSearchOutput)
	if !ok {
		t.Fatalf("provider output type = %T", response.ProviderOutputs[0])
	}
	if len(output.Calls) != 1 || len(output.Citations) != 1 {
		t.Fatalf("output = %+v", output)
	}
}

func boolPointer(value bool) *bool { return &value }
