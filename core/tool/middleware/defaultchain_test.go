package middleware

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool"
)

// dispatchThrough composes a chain around terminal the way the executor does:
// the first entry is outermost.
func dispatchThrough(chain []tool.Middleware, terminal tool.Dispatch) tool.Dispatch {
	next := terminal
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i] == nil {
			continue
		}
		next = chain[i](next)
	}
	return next
}

// TestDefaultChainShape pins the default chain by what it does, not by which
// closure each slot holds: a middleware is a function value, and Go is free to
// inline and duplicate one, so comparing code pointers passes on one
// architecture and fails on the next.
func TestDefaultChainShape(t *testing.T) {
	chain, err := FromSettings(Settings{})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	// recover, telemetry, and the non-text part limiter.
	if len(chain) != 3 {
		t.Fatalf("default chain = %d entries, want 3", len(chain))
	}
	ctx := context.Background()

	// Recover is outermost: a panicking tool becomes an error result instead of
	// taking the process with it.
	dispatch := dispatchThrough(chain, func(context.Context, message.ToolCall) message.ToolResult {
		panic("boom")
	})
	res := dispatch(ctx, message.ToolCall{ID: "call", Name: "boom"})
	if !res.IsError {
		t.Fatalf("a panic escaped the default chain: %#v", res)
	}

	// The part limiter is last, which is why the default chain carries a limiter
	// at all: an oversized non-text part does not reach the caller.
	source, err := media.NewImageBytes(make([]byte, DefaultResultPartBudget+1), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	dispatch = dispatchThrough(chain, func(_ context.Context, call message.ToolCall) message.ToolResult {
		return message.ToolResult{CallID: call.ID, Content: message.Content{
			Parts: []message.Part{message.ImagePart{Source: source}},
		}}
	})
	res = dispatch(ctx, message.ToolCall{ID: "call", Name: "shot"})
	for _, part := range res.Content.Parts {
		if _, isImage := part.(message.ImagePart); isImage {
			t.Fatal("an oversized image part survived the default chain")
		}
	}
}
