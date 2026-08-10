package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"

	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

func TestArkWebSearchCallDecoding(t *testing.T) {
	call := arkWebSearchCall(&arkresponses.ItemFunctionWebSearch{
		Id:     "ws_1",
		Status: arkresponses.ItemStatus_completed,
		Action: &arkresponses.Action{
			Type:  arkresponses.ActionType_search,
			Query: "flowcraft",
		},
	})
	if call.ID != "ws_1" || call.Status != "completed" ||
		call.Action != "search" || len(call.Queries) != 1 ||
		call.Queries[0] != "flowcraft" {
		t.Fatalf("call = %+v", call)
	}
}

func TestArkCitationDecoding(t *testing.T) {
	citations := arkCitations([]*arkresponses.Annotation{
		{
			Url:         "https://example.com",
			Title:       "Example",
			SiteName:    stringPointer("example"),
			PublishTime: stringPointer("2026-08-10"),
		},
		{},
	})
	if len(citations) != 1 {
		t.Fatalf("citations = %+v, want 1", citations)
	}
	if citations[0].URL != "https://example.com" ||
		citations[0].Title != "Example" ||
		citations[0].SiteName != "example" ||
		citations[0].PublishTime != "2026-08-10" {
		t.Fatalf("citation = %+v", citations[0])
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

func stringPointer(value string) *string { return &value }
