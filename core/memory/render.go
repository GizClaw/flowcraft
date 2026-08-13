package memory

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// ContextRenderer projects a structured context result into content suitable
// for an agent prompt. It is a consumer-side SPI: memory providers remain
// responsible only for selecting, hydrating, and packing ContextItems.
type ContextRenderer interface {
	Render(context.Context, ContextResult) (message.Content, error)
}

// ContextRendererFunc adapts a function to ContextRenderer.
type ContextRendererFunc func(context.Context, ContextResult) (message.Content, error)

func (function ContextRendererFunc) Render(ctx context.Context, result ContextResult) (message.Content, error) {
	return function(ctx, result)
}
