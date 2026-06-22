package tasks

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func generateTextMessages(ctx context.Context, modelLLM llm.LLM, messages []llm.Message, timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	msg, _, err := modelLLM.Generate(ctx, messages)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(msg.Content()), nil
}
