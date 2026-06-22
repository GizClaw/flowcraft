package context

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
)

func TestRRFPackerFiltersEmptyItemsAndPreservesStableOrder(t *testing.T) {
	packer := RRFPacker{}
	out, err := packer.PackContext(context.Background(), derive.ContextPackInput{
		Items: []derive.ContextItem{
			{Kind: derive.ContextItemRecentMessage, Text: "recent"},
			{Kind: derive.ContextItemSummaryNode, Text: ""},
			{Kind: derive.ContextItemDocumentChunk, Text: "document"},
		},
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("Items len = %d, want 2", len(out.Items))
	}
	if out.Items[0].Text != "recent" || out.Items[1].Text != "document" {
		t.Fatalf("Items order = %+v, want stable non-empty order", out.Items)
	}
}

func TestRRFPackerDedupesRetrievalAndRecentSourceMessage(t *testing.T) {
	packer := RRFPacker{}
	msg := sourcemessage.Message{ID: "m1", ConversationID: "conv"}
	hit := retrieval.Hit{Score: 0.8}
	out, err := packer.PackContext(context.Background(), derive.ContextPackInput{
		Items: []derive.ContextItem{
			{Kind: derive.ContextItemRecentMessage, Text: "recent duplicate", Message: &msg},
			{Kind: derive.ContextItemRecentMessage, Text: "retrieved", Message: &msg, Retrieval: &hit},
		},
	})
	if err != nil {
		t.Fatalf("PackContext() error = %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Text != "retrieved" || out.Items[0].Retrieval == nil {
		t.Fatalf("Items = %+v, want retrieval-backed source message kept", out.Items)
	}
}
