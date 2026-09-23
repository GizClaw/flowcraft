package summary

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// GenerateRuntime is the narrow inference surface the LLM summarizer needs.
type GenerateRuntime interface {
	Generate(context.Context, model.ModelRef, inference.GenerateRequest) (inference.GenerateResponse, error)
}

// LLMSummarizer compresses memory inputs with a generation model, replacing
// the extractive fallback when a generate model is configured. Failures
// degrade to the extractive summary in the compactor, so a flaky model never
// blocks derivation.
type LLMSummarizer struct {
	runtime  GenerateRuntime
	ref      model.ModelRef
	maxRunes int
}

// NewLLMSummarizer builds a model-backed summarizer.
func NewLLMSummarizer(runtime GenerateRuntime, ref model.ModelRef) (*LLMSummarizer, error) {
	if runtime == nil {
		return nil, errors.New("summary: generate runtime is required")
	}
	if strings.TrimSpace(ref.ID.Provider) == "" || strings.TrimSpace(ref.ID.Name) == "" {
		return nil, errors.New("summary: model provider and name are required")
	}
	return &LLMSummarizer{runtime: runtime, ref: ref, maxRunes: 1200}, nil
}

// Summarize asks the model for one durable summary of the given texts.
func (summarizer *LLMSummarizer) Summarize(ctx context.Context, request SummarizeRequest) (string, error) {
	if summarizer == nil || summarizer.runtime == nil {
		return "", errors.New("summary: summarizer is incomplete")
	}
	if ctx == nil {
		return "", errors.New("summary: context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	texts := make([]string, 0, len(request.Texts))
	for _, text := range request.Texts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			texts = append(texts, trimmed)
		}
	}
	if len(texts) == 0 {
		return "", errors.New("summary: no texts to summarize")
	}
	response, err := summarizer.runtime.Generate(ctx, summarizer.ref, generateRequest(request, texts))
	if err != nil {
		return "", fmt.Errorf("summary: generate: %w", err)
	}
	text := strings.TrimSpace(response.Message.Content.Text())
	if text == "" {
		return "", errors.New("summary: model returned an empty summary")
	}
	return limitRunes(text, summarizer.maxRunes), nil
}

func generateRequest(request SummarizeRequest, texts []string) inference.GenerateRequest {
	intent := inference.Intent{Text: &inference.TextIntent{}}
	return inference.GenerateRequest{
		Context: []coremessage.Message{{
			Role:    coremessage.RoleSystem,
			Content: coremessage.NewTextContent(summarySystem),
		}},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: coremessage.NewTextContent(summaryUser(request, texts)),
				Intent:  intent,
			},
		},
	}
}

func summaryUser(request SummarizeRequest, texts []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Level: %s\n", request.Level)
	if len(request.Topics) > 0 {
		fmt.Fprintf(&builder, "Topics: %s\n", strings.Join(uniqueStrings(request.Topics), ", "))
	}
	builder.WriteString("Memories:\n")
	for index, text := range texts {
		fmt.Fprintf(&builder, "%d. %s\n", index+1, text)
	}
	builder.WriteString("\nWrite the summary.")
	return builder.String()
}

func limitRunes(value string, max int) string {
	runes := []rune(value)
	if max <= 0 || len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

const summarySystem = `You compress long-term memory into durable summaries that are retrieved months later.

Rules:
- Preserve every concrete name, place, date, number, and relationship exactly as stated.
- Keep causal and purposeful chains together ("because", "so that", "after").
- Do not invent or infer facts that the memories do not state.
- Prefer specific wording over generalities (say "Sweden", not "her home country").
- Write 2-4 sentences, or one dense paragraph for larger inputs.`
