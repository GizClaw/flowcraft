package claw

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const providerSafeEmptyUserText = "\u200b"

type providerSafeLLMResolver struct {
	inner llm.LLMResolver
}

func newProviderSafeLLMResolver(inner llm.LLMResolver) llm.LLMResolver {
	return providerSafeLLMResolver{inner: inner}
}

func (r providerSafeLLMResolver) Resolve(ctx context.Context, modelName string) (llm.LLM, error) {
	if r.inner == nil {
		return nil, errdefs.NotFoundf("claw: model resolver is not configured")
	}
	client, err := r.inner.Resolve(ctx, modelName)
	if err != nil {
		return nil, err
	}
	return providerSafeLLM{inner: client}, nil
}

func (r providerSafeLLMResolver) InvalidateCache(opts ...llm.InvalidateOption) {
	if r.inner == nil {
		return
	}
	r.inner.InvalidateCache(opts...)
}

type providerSafeLLM struct {
	inner llm.LLM
}

func (m providerSafeLLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return m.inner.Generate(ctx, providerSafeMessages(messages), opts...)
}

func (m providerSafeLLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	return m.inner.GenerateStream(ctx, providerSafeMessages(messages), opts...)
}

func providerSafeMessages(messages []llm.Message) []llm.Message {
	var out []llm.Message
	for i, msg := range messages {
		safe, changed := providerSafeMessage(msg)
		if !changed {
			continue
		}
		if out == nil {
			out = model.CloneMessages(messages)
		}
		out[i] = safe
	}
	if out != nil {
		return out
	}
	return messages
}

func providerSafeMessage(msg llm.Message) (llm.Message, bool) {
	var changed bool
	if parts, ok := stripClawHistoryXMLParts(msg.Parts); ok {
		msg.Parts = parts
		changed = true
	}
	if needsProviderSafeUserText(msg) {
		return llm.NewTextMessage(llm.RoleUser, providerSafeEmptyUserText), true
	}
	return msg, changed
}

func stripClawHistoryXMLParts(parts []llm.Part) ([]llm.Part, bool) {
	for i, part := range parts {
		if !isClawHistoryXMLPart(part) {
			continue
		}
		out := make([]llm.Part, 0, len(parts)-1)
		for _, part := range parts[:i] {
			out = append(out, part.Clone())
		}
		for _, part := range parts[i+1:] {
			if !isClawHistoryXMLPart(part) {
				out = append(out, part.Clone())
			}
		}
		return out, true
	}
	return parts, false
}

func needsProviderSafeUserText(msg llm.Message) bool {
	if msg.Role != llm.RoleUser || strings.TrimSpace(msg.Content()) != "" {
		return false
	}
	for _, part := range msg.Parts {
		if part.Type != llm.PartText {
			return false
		}
	}
	return true
}
