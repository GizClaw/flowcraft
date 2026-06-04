package ingest

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

type fakeLLM struct {
	mu                sync.Mutex
	Responses         []string
	ResponsesBySystem map[string][]string
	Usages            []llm.TokenUsage
	UsagesBySystem    map[string][]llm.TokenUsage
	Err               error
	Messages          [][]llm.Message
	Options           [][]llm.GenerateOption
}

func (f *fakeLLM) Generate(_ context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Messages = append(f.Messages, msgs)
	f.Options = append(f.Options, opts)
	if len(msgs) > 0 && f.ResponsesBySystem != nil {
		system := msgs[0].Content()
		if responses := f.ResponsesBySystem[system]; len(responses) > 0 {
			body := responses[0]
			f.ResponsesBySystem[system] = responses[1:]
			return llm.NewTextMessage(llm.RoleAssistant, body), f.nextUsageForSystem(system), nil
		}
	}
	if len(f.Responses) == 0 {
		if f.Err != nil {
			return llm.Message{}, llm.TokenUsage{}, f.Err
		}
		return llm.NewTextMessage(llm.RoleAssistant, `{"proposals":[]}`), llm.TokenUsage{}, nil
	}
	body := f.Responses[0]
	f.Responses = f.Responses[1:]
	return llm.NewTextMessage(llm.RoleAssistant, body), f.nextUsage(), nil
}

func (f *fakeLLM) nextUsageForSystem(system string) llm.TokenUsage {
	if f.UsagesBySystem != nil {
		if usages := f.UsagesBySystem[system]; len(usages) > 0 {
			usage := usages[0]
			f.UsagesBySystem[system] = usages[1:]
			return usage
		}
	}
	return llm.TokenUsage{}
}

func (f *fakeLLM) nextUsage() llm.TokenUsage {
	if len(f.Usages) == 0 {
		return llm.TokenUsage{}
	}
	usage := f.Usages[0]
	f.Usages = f.Usages[1:]
	return usage
}

func (f *fakeLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("fakeLLM: streaming not implemented")
}
