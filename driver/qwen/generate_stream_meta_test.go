package qwen

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

var _ inference.ProviderStreamMetadata = (*sseStream)(nil)

func TestSSEStreamMetadataRetainsEnvelopeRequestID(t *testing.T) {
	stream := &sseStream{
		queue: []streamFragment{
			{kind: fragmentText, text: "x", requestID: "req_qwen_1"},
		},
	}
	fragment, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if fragment.requestID != "req_qwen_1" {
		t.Fatalf("fragment request id = %q, want req_qwen_1", fragment.requestID)
	}
	if got := stream.RequestID(); got != "req_qwen_1" {
		t.Fatalf("stream RequestID = %q, want req_qwen_1", got)
	}
	if got := stream.ResponseID(); got != "" {
		t.Fatalf("ResponseID = %q, want empty", got)
	}
}
