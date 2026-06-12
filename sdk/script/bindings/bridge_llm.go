package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// LLMBridgeOptions configures NewLLMBridge.
//
// The bridge is the only consumer of these options at the moment, so
// the surface stays minimal: a resolver to materialize the LLM, an
// optional tool registry for function-calling, the generation defaults
// the script can override per call, and a source label for diagnostics.
//
// Notably absent — these were intentional decisions during the
// "B-track" refactor:
//
//   - No llm.RoundConfig: scripts and the bridge interior speak the
//     bridge's own call-options / roundOptions types end-to-end.
//   - No Go-side OnEvent hook: scripts pull chunks via the iterator
//     returned by stream(); a host-side event subscription will be
//     added only when a real consumer needs it.
type LLMBridgeOptions struct {
	Resolver llm.LLMResolver
	Registry *tool.Registry

	// Defaults are merged with the script-supplied generation overrides
	// on every llm.run() / llm.stream() call. Any field the script
	// omits is inherited from here; explicit script values win.
	// Messages are never defaulted and must be supplied per call.
	Defaults LLMRunOptions

	// Source labels diagnostics (errors, future tracing). It is the
	// bridge equivalent of "node id" / "event id" in legacy code.
	Source string
}

// LLMRunOptions is the defaultable generation-options subset shared by
// LLMBridgeOptions.Defaults and per-call script overrides.
//
// Scripts pass a call object containing messages plus these optional
// fields. Messages are parsed separately into an internal call-options
// value so LLMBridgeOptions.Defaults cannot imply a default conversation.
// The messages value is required on every call and must use the
// canonical model.Message shape: role plus parts. Any unset generation
// field inherits the bridge's LLMBridgeOptions.Defaults value.
// Pointer fields (Temperature, JSONMode, Thinking) distinguish "script
// omitted" from "script explicitly set to zero/false". Unknown JSON keys
// are rejected at parse time so script typos surface immediately instead
// of silently falling back to defaults.
//
// Script-side schema (JS / Lua):
//
//	llm.run({
//	    messages: [{
//	        role: "user",
//	        parts: [{ type: "text", text: "What changed?" }],
//	    }],
//	    model:        "openai/gpt-4o-mini",   // optional, string
//	    temperature:  0.2,                    // optional, number
//	    max_tokens:   1024,                   // optional, integer
//	    json_mode:    true,                   // optional, bool
//	    thinking:     false,                  // optional, bool
//	    tools:        ["web_search"],         // optional, []string
//	})
type LLMRunOptions struct {
	Model       string   `json:"model,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty"`
	JSONMode    *bool    `json:"json_mode,omitempty"`
	Thinking    *bool    `json:"thinking,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

type llmCallOptions struct {
	messages   []model.Message
	runOptions LLMRunOptions
}

// NewLLMBridge exposes LLM calls to scripts as the global "llm":
//
//	llm.run({ messages })                         // blocking, returns full result map
//	llm.run({ messages, model, temperature, ... }) // with per-call overrides
//	llm.stream({ messages })                      // returns iterator { next, part, text, finish, close }
//	llm.stream({ messages, model, ... })          // streamed, with per-call overrides
//
// Iterator usage (multimodal-friendly):
//
//	var s = llm.stream({
//	    messages: [{ role: "user", parts: [{ type: "text", text: "go on" }] }],
//	    model: "...",
//	});
//	while (s.next()) {
//	    var p = s.part();        // map projection of model.Part
//	    if (p.type === "text")   write(p.text);
//	    if (p.type === "image")  show(p.image.url);
//	}
//	var r = s.finish();          // round result map
//	board.setVar("answer", r.content);
//	board.setChannel(board.MAIN_CHANNEL, r.messages);
//
// Neither mode writes to the board; the script controls what to do
// with results (typically via the board bridge: board.setVar /
// board.setChannel). The per-call schema is parsed by parseRunOptions.
func NewLLMBridge(opts LLMBridgeOptions) BindingFunc {
	return func(callCtx context.Context) (string, any) {
		resolveOpts := func(rawOpts any) (roundOptions, []model.Message, error) {
			callOpts, err := parseRunOptions(rawOpts)
			if err != nil {
				return roundOptions{}, nil, err
			}
			return toRoundOptions(opts.Defaults, callOpts.runOptions), callOpts.messages, nil
		}

		return "llm", map[string]any{
			"run": func(rawOpts any) (map[string]any, error) {
				ro, messages, err := resolveOpts(rawOpts)
				if err != nil {
					return nil, err
				}
				r, err := runRound(callCtx, opts.Resolver, opts.Registry, opts.Source, messages, ro)
				if err != nil {
					return nil, err
				}
				return roundResultToMap(r), nil
			},

			"stream": func(rawOpts any) (map[string]any, error) {
				ro, messages, err := resolveOpts(rawOpts)
				if err != nil {
					return nil, err
				}
				s, err := startRound(callCtx, opts.Resolver, opts.Registry, opts.Source, messages, ro)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"next":  s.Next,
					"text":  s.Text,
					"part":  func() map[string]any { return partToMap(s.Current()) },
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

// parseRunOptions decodes the script-supplied call object into internal
// call options. messages is mandatory and must be supplied explicitly
// for each llm.run() / llm.stream() call; only generation fields flow
// into LLMRunOptions for default merging. Any non-map value, missing or
// null messages value, or unknown JSON key returns an error so that the
// script sees a real exception instead of silently inheriting a default
// conversation or an implicit history source.
func parseRunOptions(v any) (llmCallOptions, error) {
	if v == nil {
		return llmCallOptions{}, errdefs.Validationf("llm: missing required field %q", "messages")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return llmCallOptions{}, errdefs.Validationf("llm: options must be an object, got %T", v)
	}
	rawMessages, ok := m["messages"]
	if !ok {
		return llmCallOptions{}, errdefs.Validationf("llm: missing required field %q", "messages")
	}
	messages, err := parseLLMMessages(rawMessages, "llm.messages")
	if err != nil {
		return llmCallOptions{}, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return llmCallOptions{}, fmt.Errorf("llm: marshal options: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var opts struct {
		Messages    json.RawMessage `json:"messages"`
		Model       string          `json:"model,omitempty"`
		Temperature *float64        `json:"temperature,omitempty"`
		MaxTokens   int64           `json:"max_tokens,omitempty"`
		JSONMode    *bool           `json:"json_mode,omitempty"`
		Thinking    *bool           `json:"thinking,omitempty"`
		Tools       []string        `json:"tools,omitempty"`
	}
	if err := dec.Decode(&opts); err != nil {
		return llmCallOptions{}, fmt.Errorf("llm: invalid options: %w", err)
	}
	return llmCallOptions{
		messages: messages,
		runOptions: LLMRunOptions{
			Model:       opts.Model,
			Temperature: opts.Temperature,
			MaxTokens:   opts.MaxTokens,
			JSONMode:    opts.JSONMode,
			Thinking:    opts.Thinking,
			Tools:       opts.Tools,
		},
	}, nil
}

// toRoundOptions folds defaults with the script-supplied override into
// the bridge-internal roundOptions value the round logic consumes. It
// only handles generation options; messages are resolved by parseRunOptions
// and passed to the round separately.
//
// Merge rules:
//
//   - Scalars (Model, MaxTokens): override wins when non-zero.
//   - Pointers (Temperature, JSONMode, Thinking): override wins when
//     non-nil. Explicit `false` from a script therefore disables a
//     default `true` — the whole reason these fields are *bool.
//   - Slices (Tools): override REPLACES (does not append). A script
//     wanting to extend defaults must build the union itself; the
//     bridge intentionally avoids the magic of additive merges so the
//     script's intent stays explicit.
//
// Defaults is never mutated; callers can reuse the same bridge across
// many concurrent script invocations safely.
func toRoundOptions(defaults, override LLMRunOptions) roundOptions {
	out := roundOptions{
		Model:       defaults.Model,
		Temperature: defaults.Temperature,
		MaxTokens:   defaults.MaxTokens,
		ToolNames:   defaults.Tools,
	}
	if defaults.JSONMode != nil {
		out.JSONMode = *defaults.JSONMode
	}
	if defaults.Thinking != nil {
		out.Thinking = *defaults.Thinking
	}

	if override.Model != "" {
		out.Model = override.Model
	}
	if override.Temperature != nil {
		out.Temperature = override.Temperature
	}
	if override.MaxTokens != 0 {
		out.MaxTokens = override.MaxTokens
	}
	if override.JSONMode != nil {
		out.JSONMode = *override.JSONMode
	}
	if override.Thinking != nil {
		out.Thinking = *override.Thinking
	}
	if len(override.Tools) > 0 {
		out.ToolNames = override.Tools
	}
	return out
}
