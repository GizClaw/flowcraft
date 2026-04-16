package bindings

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// LLMBridgeOptions configures NewLLMBridge.
type LLMBridgeOptions struct {
	Stream  workflow.StreamCallback
	EventID string

	Resolver llm.LLMResolver
	Registry *tool.Registry

	// BaseConfig is merged with overrides passed from the script.
	BaseConfig llm.RoundConfig

	// ReadMessages returns the messages to send to the LLM.
	// The caller controls where messages come from (board, parameters, etc.).
	// When nil, an empty slice is used.
	ReadMessages func(ctx context.Context) []model.Message
}

// NewLLMBridge exposes LLM calls to scripts as global "llm":
//
//	llm.run()                  — blocking, returns full result
//	llm.run({ temperature: 0.2 }) — with config overrides
//	llm.stream()               — returns iterator { next, token, finish }
//
// Neither mode writes to the board; the script controls what to do with results.
func NewLLMBridge(opts LLMBridgeOptions) BindingFunc {
	return func(callCtx context.Context) (string, any) {
		readMsgs := func() []model.Message {
			if opts.ReadMessages != nil {
				return opts.ReadMessages(callCtx)
			}
			return nil
		}

		return "llm", map[string]any{
			"run": func(overrides any) (map[string]any, error) {
				cfg := mergeRoundConfig(opts.BaseConfig, normalizeLLMOverrides(overrides))
				msgs := readMsgs()
				result, err := llm.RunRound(
					callCtx, opts.Stream, opts.Resolver, opts.Registry,
					opts.EventID, msgs, cfg,
				)
				if err != nil {
					return nil, err
				}
				return roundResultToMap(result), nil
			},

			"stream": func(overrides any) (map[string]any, error) {
				cfg := mergeRoundConfig(opts.BaseConfig, normalizeLLMOverrides(overrides))
				msgs := readMsgs()
				s, err := llm.StreamRound(
					callCtx, opts.Stream, opts.Resolver, opts.Registry,
					opts.EventID, msgs, cfg,
				)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"next":  s.Next,
					"token": s.Token,
					"close": func() error { return s.Close() },
					"finish": func() (map[string]any, error) {
						r, err := s.Finish()
						if err != nil {
							return nil, err
						}
						return roundResultToMap(r), nil
					},
				}, nil
			},
		}
	}
}

func roundResultToMap(r *llm.RoundResult) map[string]any {
	m := map[string]any{
		"content":      r.Content,
		"tool_pending": r.ToolPending,
		"usage": map[string]any{
			"input_tokens":  r.Usage.InputTokens,
			"output_tokens": r.Usage.OutputTokens,
			"total_tokens":  r.Usage.TotalTokens,
		},
	}
	if len(r.ToolCalls) > 0 {
		tcs := make([]map[string]any, len(r.ToolCalls))
		for i, tc := range r.ToolCalls {
			tcs[i] = map[string]any{
				"id":        tc.ID,
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
		}
		m["tool_calls"] = tcs
	}
	return m
}

func normalizeLLMOverrides(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func mergeRoundConfig(base llm.RoundConfig, overrides map[string]any) llm.RoundConfig {
	if len(overrides) == 0 {
		return base
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return base
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return base
	}
	for k, v := range overrides {
		m[k] = v
	}
	merged, err := json.Marshal(m)
	if err != nil {
		return base
	}
	var out llm.RoundConfig
	if err := json.Unmarshal(merged, &out); err != nil {
		return base
	}
	return out
}
