package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// generateRaw is the transport stage's normalized completion shared by the
// chat and responses pipelines: everything the decoder needs, nothing it
// does not.
type generateRaw struct {
	id         string
	reasonings []rawReasoning
	texts      []string
	toolCalls  []rawToolCall
	finish     inference.FinishReason
	usage      rawUsage

	// webSearchCalls and citations surface DeepSeek's hosted web_search
	// output on the Responses surface.
	webSearchCalls []inference.WebSearchCall
	citations      []inference.Citation
}

type rawReasoning struct {
	id   string
	text string
}

type rawToolCall struct {
	id   string
	name string
	args []byte
}

type rawUsage struct {
	input     int64
	output    int64
	total     int64
	cached    int64
	reasoning int64
	present   bool
}

// streamRawKind enumerates the events the stateful stream transport hands
// to the pure stream decoder.
type streamRawKind int

const (
	streamRawText streamRawKind = iota
	streamRawReasoning
	streamRawToolFragment
	streamRawProviderOutput
	streamRawFinish
)

type streamRaw struct {
	kind            streamRawKind
	part            int // canonical part index, assigned by the transport
	text            string
	responseID      string
	tool            streamRawTool
	usage           *rawUsage
	finish          inference.FinishReason
	providerOutputs inference.ProviderOutputs
	reasoningID     string
}

type streamRawTool struct {
	id           string
	name         string
	argsFragment string
}

// ledger tracks the compiler's decision for every active request field so
// the report accounts for each one exactly once: rejected (compile fails),
// dropped (intentionally discarded with a reason), or native (consumed).
type ledger struct {
	operation inference.Operation
	active    []inference.FieldID
	rejected  map[inference.FieldID]string
	dropped   map[inference.FieldID]string
	order     []inference.FieldID
}

func newLedger(operation inference.Operation, active []inference.FieldID) *ledger {
	return &ledger{
		operation: operation,
		active:    append([]inference.FieldID(nil), active...),
		rejected:  make(map[inference.FieldID]string),
		dropped:   make(map[inference.FieldID]string),
	}
}

func (l *ledger) reject(field inference.FieldID, reason string) {
	if _, exists := l.rejected[field]; !exists {
		l.order = append(l.order, field)
	}
	l.rejected[field] = reason
}

func (l *ledger) drop(field inference.FieldID, reason string) {
	if _, rejected := l.rejected[field]; rejected {
		return
	}
	if _, exists := l.dropped[field]; !exists {
		l.dropped[field] = reason
	}
}

func (l *ledger) report() inference.CompileReport {
	report := inference.CompileReport{Operation: l.operation}
	for _, field := range l.active {
		decision := inference.Decision{Field: field, Disposition: inference.Native}
		if reason, rejected := l.rejected[field]; rejected {
			decision.Disposition = inference.Rejected
			decision.Reason = reason
		} else if reason, dropped := l.dropped[field]; dropped {
			decision.Disposition = inference.Dropped
			decision.Reason = reason
		}
		report.Decisions = append(report.Decisions, decision)
	}
	return report
}

func (l *ledger) err() error {
	if len(l.order) == 0 {
		return nil
	}
	field := l.order[0]
	reason := l.rejected[field]
	if strings.HasPrefix(string(field), "extension.") {
		return inference.NewError(
			inference.InvalidExtension,
			l.operation,
			field,
			fmt.Errorf("deepseek: %s", reason),
		)
	}
	return inference.NewError(
		inference.UnsupportedFeature,
		l.operation,
		field,
		fmt.Errorf("deepseek: %s", reason),
	)
}

var contextPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldGenerateContextText,
	message.PartImage:      inference.FieldGenerateContextImage,
	message.PartAudio:      inference.FieldGenerateContextAudio,
	message.PartVideo:      inference.FieldGenerateContextVideo,
	message.PartFile:       inference.FieldGenerateContextFile,
	message.PartData:       inference.FieldGenerateContextData,
	message.PartToolCall:   inference.FieldGenerateContextToolCall,
	message.PartToolResult: inference.FieldGenerateContextToolResult,
	message.PartReasoning:  inference.FieldGenerateContextReasoning,
}

var inputPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldGenerateInputText,
	message.PartImage:      inference.FieldGenerateInputImage,
	message.PartAudio:      inference.FieldGenerateInputAudio,
	message.PartVideo:      inference.FieldGenerateInputVideo,
	message.PartFile:       inference.FieldGenerateInputFile,
	message.PartData:       inference.FieldGenerateInputData,
	message.PartToolCall:   inference.FieldGenerateInputToolCall,
	message.PartToolResult: inference.FieldGenerateInputToolResult,
	message.PartReasoning:  inference.FieldGenerateInputReasoning,
}

// mapFinishReason translates the provider's terminal states. DeepSeek adds
// insufficient_system_resource — the request was interrupted on their side,
// so it classifies as a retryable provider failure rather than a finish.
func mapFinishReason(reason string) (inference.FinishReason, error) {
	switch reason {
	case "", "stop":
		return inference.FinishCompleted, nil
	case "length":
		return inference.FinishMaxOutput, nil
	case "tool_calls":
		return inference.FinishToolCalls, nil
	case "content_filter":
		return inference.FinishContentFilter, nil
	case "insufficient_system_resource":
		return "", errdefs.NotAvailablef(
			"deepseek: request interrupted: insufficient system resource")
	default:
		return inference.FinishOther, nil
	}
}

// decodeGenerate assembles the canonical response: reasoning traces first
// (they are the model's process, ahead of its answer), then text, then
// tool calls.
func decodeGenerate(_ context.Context, raw generateRaw) (inference.GenerateResponse, error) {
	parts := make([]message.Part, 0,
		len(raw.reasonings)+len(raw.texts)+len(raw.toolCalls))
	for _, reasoning := range raw.reasonings {
		parts = append(parts, message.ReasoningPart{
			Text: reasoning.text,
			ID:   reasoning.id,
		})
	}
	for _, text := range raw.texts {
		parts = append(parts, message.TextPart{Text: text})
	}
	for _, call := range raw.toolCalls {
		arguments := json.RawMessage(call.args)
		if len(arguments) == 0 || !json.Valid(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		parts = append(parts, message.ToolCallPart{Call: message.ToolCall{
			ID:        call.id,
			Name:      call.name,
			Arguments: arguments,
		}})
	}

	response := inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: raw.finish,
		Metadata:     inference.Metadata{ResponseID: raw.id},
	}
	if raw.usage.present {
		response.Usage = rawUsageCanonical(raw.usage)
	}
	if output := webSearchProviderOutput(raw.webSearchCalls, raw.citations); output != nil {
		response.ProviderOutputs = append(response.ProviderOutputs, output)
	}
	return response, nil
}

func rawUsageCanonical(raw rawUsage) inference.Usage {
	usage := inference.Usage{
		InputTokens:  raw.input,
		OutputTokens: raw.output,
		TotalTokens:  raw.total,
	}
	if raw.cached > 0 {
		cached := raw.cached
		usage.Input.CacheReadTokens = &cached
	}
	if raw.reasoning > 0 {
		reasoning := raw.reasoning
		usage.Output.ReasoningTokens = &reasoning
		usage.Output.ReasoningAccounting = inference.ReasoningIncludedInOutput
	}
	return usage
}

// decodeGenerateStream is the pure stage: streamRaw already carries
// canonical part indices assigned by the stateful transport.
func decodeGenerateStream(
	_ context.Context,
	raw streamRaw,
) (inference.GenerateStreamEvent, error) {
	switch raw.kind {
	case streamRawText:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta:     inference.TextPartDelta{Text: raw.text},
		}, nil
	case streamRawReasoning:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta: inference.ReasoningDelta{
				Text: raw.text,
				ID:   raw.reasoningID,
			},
		}, nil
	case streamRawToolFragment:
		return inference.GenerateStreamEvent{
			PartIndex: raw.part,
			Delta: inference.ToolCallDelta{
				ID:                raw.tool.id,
				Name:              raw.tool.name,
				ArgumentsFragment: raw.tool.argsFragment,
			},
		}, nil
	case streamRawProviderOutput:
		return inference.GenerateStreamEvent{
			ProviderOutputs: raw.providerOutputs.Clone(),
		}, nil
	case streamRawFinish:
		event := inference.GenerateStreamEvent{
			FinishReason:    raw.finish,
			ResponseID:      raw.responseID,
			ProviderOutputs: raw.providerOutputs.Clone(),
		}
		if raw.usage != nil {
			usage := rawUsageCanonical(*raw.usage)
			event.Usage = &usage
		}
		return event, nil
	}
	return inference.GenerateStreamEvent{}, fmt.Errorf(
		"deepseek: unknown stream raw kind %d",
		raw.kind,
	)
}

func bytesClone(raw []byte) []byte {
	return append([]byte(nil), raw...)
}
