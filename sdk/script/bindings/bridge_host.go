package bindings

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// NewHostBridge exposes the engine.Host control plane to scripts as the
// global "host". It is the script-side mirror of the small interfaces
// composed in engine.Host (Publisher, Interrupter, UserPrompter,
// Checkpointer, UsageReporter).
//
// Script-facing API (every method returns nil / "" on a NoopHost so
// scripts can call it unconditionally):
//
//	host.publish(subject, payload)         -> nil | error
//	host.checkInterrupt()                  -> {cause, detail} | null
//	host.askUser({ parts, schema, source, metadata })
//	                                       -> { parts, metadata }
//	host.reportUsage({ input, output, total })
//	                                       -> nil
//
// Checkpointing is intentionally NOT exposed: scripts have no access to
// the executing engine's ExecID / Board snapshot / Step marker, so a
// "host.checkpoint(payload)" call would be ambiguous at best and
// accidentally destructive at worst. If a future use case needs script
// initiated checkpoints, it should land as its own typed API rather
// than a generic bag.
//
// Source labels diagnostic strings (errors carry it, future tracing may
// too); it mirrors the bridge_llm.LLMBridgeOptions.Source convention.
//
// Interrupt latching: the first interrupt the bridge observes is cached
// inside the closure so subsequent host.checkInterrupt() calls keep
// returning the same {cause, detail} for the lifetime of the bridge
// instance. This matches how scripts naturally consume the signal —
// "have I been told to stop?" — instead of forcing them to either save
// the value at first sight or risk losing it on a re-poll.
//
// The bridge does NOT own the engine.Host; the caller (typically
// scriptnode.ScriptNode) feeds it whatever ctx.Host the executor
// installed and reuses the same Host instance for all bindings.
func NewHostBridge(host engine.Host, source string) BindingFunc {
	return func(callCtx context.Context) (string, any) {
		if host == nil {
			host = engine.NoopHost{}
		}

		var (
			latchMu sync.Mutex
			latched *engine.Interrupt
		)

		// pollInterrupt does the latch-or-fetch dance once per call.
		// It deliberately uses a non-blocking select so scripts that
		// poll in a loop never freeze the goja VM.
		pollInterrupt := func() *engine.Interrupt {
			latchMu.Lock()
			defer latchMu.Unlock()
			if latched != nil {
				return latched
			}
			ch := host.Interrupts()
			if ch == nil {
				return nil
			}
			select {
			case intr, ok := <-ch:
				if !ok {
					return nil
				}
				latched = &intr
				return latched
			default:
				return nil
			}
		}

		return "host", map[string]any{
			"publish": func(subject string, payload any) error {
				env, err := event.NewEnvelope(callCtx, event.Subject(subject), payload)
				if err != nil {
					return fmt.Errorf("host.publish: %w", err)
				}
				return host.Publish(callCtx, env)
			},

			"checkInterrupt": func() any {
				intr := pollInterrupt()
				if intr == nil {
					return nil
				}
				return map[string]any{
					"cause":  string(intr.Cause),
					"detail": intr.Detail,
				}
			},

			"askUser": func(raw any) (map[string]any, error) {
				prompt, err := parseUserPrompt(raw, source)
				if err != nil {
					return nil, err
				}
				reply, err := host.AskUser(callCtx, prompt)
				if err != nil {
					return nil, err
				}
				return userReplyToMap(reply), nil
			},

			"reportUsage": func(raw any) error {
				usage, err := parseUsage(raw)
				if err != nil {
					return fmt.Errorf("host.reportUsage: %w", err)
				}
				host.ReportUsage(callCtx, usage)
				return nil
			},
		}
	}
}

// parseUserPrompt projects a script-supplied object onto engine.UserPrompt.
// Accepted shapes (any subset, all fields optional):
//
//	{ parts: [...], schema: "..." | bytes, source: "...", metadata: {...} }
//
// Parts are projected via parsePart (the same helper llm_marshal uses)
// so multimodal payloads survive the round-trip without a custom parser
// here.
func parseUserPrompt(raw any, source string) (engine.UserPrompt, error) {
	prompt := engine.UserPrompt{Source: source}
	if raw == nil {
		return prompt, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return prompt, errdefs.Validationf("askUser: prompt must be an object, got %T", raw)
	}

	if rawParts, ok := m["parts"]; ok && rawParts != nil {
		list, err := asAnyList(rawParts, "askUser.parts")
		if err != nil {
			return prompt, err
		}
		parts := make([]model.Part, len(list))
		for i, raw := range list {
			p, err := parsePart(raw, fmt.Sprintf("askUser.parts[%d]", i))
			if err != nil {
				return prompt, err
			}
			parts[i] = p
		}
		prompt.Parts = parts
	}

	if rawSchema, ok := m["schema"]; ok && rawSchema != nil {
		switch v := rawSchema.(type) {
		case string:
			prompt.Schema = []byte(v)
		case []byte:
			prompt.Schema = v
		default:
			return prompt, errdefs.Validationf("askUser.schema: expected string or bytes, got %T", v)
		}
	}

	if rawSrc, ok := m["source"]; ok && rawSrc != nil {
		s, ok := rawSrc.(string)
		if !ok {
			return prompt, errdefs.Validationf("askUser.source: expected string, got %T", rawSrc)
		}
		// Caller-supplied source overrides the bridge default so a
		// script can attribute the prompt to a sub-step of itself.
		prompt.Source = s
	}

	if rawMeta, ok := m["metadata"]; ok && rawMeta != nil {
		meta, err := parseStringMap(rawMeta, "askUser.metadata")
		if err != nil {
			return prompt, err
		}
		prompt.Metadata = meta
	}

	return prompt, nil
}

func userReplyToMap(reply engine.UserReply) map[string]any {
	out := map[string]any{
		"parts": partsToList(reply.Parts),
	}
	if len(reply.Metadata) > 0 {
		meta := make(map[string]any, len(reply.Metadata))
		for k, v := range reply.Metadata {
			meta[k] = v
		}
		out["metadata"] = meta
	}
	return out
}

// parseUsage maps {input, output, total} (any of which may be missing)
// onto a model.TokenUsage. Numbers may arrive as float64 (the JSON
// default in goja) or int64; both are folded down to int64 here.
func parseUsage(raw any) (model.TokenUsage, error) {
	var usage model.TokenUsage
	if raw == nil {
		return usage, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return usage, errdefs.Validationf("usage: expected object, got %T", raw)
	}
	if v, ok := m["input"]; ok {
		n, err := asInt64(v, "input")
		if err != nil {
			return usage, err
		}
		usage.InputTokens = n
	}
	if v, ok := m["output"]; ok {
		n, err := asInt64(v, "output")
		if err != nil {
			return usage, err
		}
		usage.OutputTokens = n
	}
	if v, ok := m["total"]; ok {
		n, err := asInt64(v, "total")
		if err != nil {
			return usage, err
		}
		usage.TotalTokens = n
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage, nil
}

func asInt64(v any, field string) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case uint:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case float32:
		return int64(n), nil
	case float64:
		return int64(n), nil
	default:
		return 0, errdefs.Validationf("usage.%s: expected number, got %T", field, v)
	}
}

func parseStringMap(raw any, field string) (map[string]string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errdefs.Validationf("%s: expected object, got %T", field, raw)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, errdefs.Validationf("%s.%s: expected string, got %T", field, k, v)
		}
		out[k] = s
	}
	return out, nil
}
