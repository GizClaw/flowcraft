package sqlite_test

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

type fakeLLM struct {
	resp string
	err  error
}

func (f *fakeLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if f.err != nil {
		return llm.Message{}, llm.TokenUsage{}, f.err
	}
	return llm.Message{
		Role:  model.RoleAssistant,
		Parts: []model.Part{{Type: model.PartText, Text: f.resp}},
	}, llm.TokenUsage{}, nil
}

func (f *fakeLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not used")
}
