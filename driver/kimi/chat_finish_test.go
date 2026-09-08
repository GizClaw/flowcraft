package kimi

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestChatStreamEndReportsSynthesizedFinishOnlyWithoutExplicitReason(t *testing.T) {
	truncated := &chatStream{sawTools: true}
	if got := truncated.end(); got != string(inference.FinishToolCalls) {
		t.Fatalf("end() = %q, want synthesized tool_calls", got)
	}
	if len(truncated.pending) != 1 ||
		!truncated.pending[0].synthesized ||
		truncated.pending[0].finish != inference.FinishToolCalls {
		t.Fatalf("synthesized finish payload = %+v, want synthesized tool_calls",
			truncated.pending)
	}
	if got := truncated.end(); got != "" {
		t.Fatalf("second end() = %q, want empty", got)
	}

	explicit := &chatStream{finish: inference.FinishCompleted}
	if got := explicit.end(); got != "" {
		t.Fatalf("end() with explicit finish = %q, want empty", got)
	}
	if len(explicit.pending) != 1 ||
		explicit.pending[0].synthesized ||
		explicit.pending[0].finish != inference.FinishCompleted {
		t.Fatalf("explicit finish payload = %+v, want non-synthesized completed",
			explicit.pending)
	}

	textOnly := &chatStream{}
	if got := textOnly.end(); got != string(inference.FinishCompleted) {
		t.Fatalf("end() without tools/finish = %q, want synthesized completed", got)
	}
	if len(textOnly.pending) != 1 ||
		!textOnly.pending[0].synthesized {
		t.Fatalf("text-only synthesized payload = %+v, want synthesized", textOnly.pending)
	}
}

func TestDecodeGenerateStreamPropagatesSynthesizedFinish(t *testing.T) {
	synthesized := streamRaw{
		kind:        streamRawFinish,
		finish:      inference.FinishCompleted,
		synthesized: true,
	}
	event, err := decodeGenerateStream(context.Background(), synthesized)
	if err != nil {
		t.Fatalf("decode synthesized finish: %v", err)
	}
	if event.FinishReason != inference.FinishCompleted || !event.FinishSynthesized {
		t.Fatalf("decoded synthesized finish = %+v", event)
	}

	explicit := streamRaw{
		kind:   streamRawFinish,
		finish: inference.FinishCompleted,
	}
	event, err = decodeGenerateStream(context.Background(), explicit)
	if err != nil {
		t.Fatalf("decode explicit finish: %v", err)
	}
	if event.FinishSynthesized {
		t.Fatalf("decoded explicit finish must not be synthesized: %+v", event)
	}
}
