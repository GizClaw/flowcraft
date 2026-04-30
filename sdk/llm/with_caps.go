package llm

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// WithCaps wraps inner so that GenerateOptions fields the model does
// not support are dropped or downgraded, and Messages containing
// unsupported modality parts cause an errdefs.Validation error
// before any provider call. See doc/sdk-llm-redesign.md §3.5 for
// the full per-cap behavior table.
//
// If caps is the zero value (all supported), inner is returned
// unwrapped — the resolver relies on this to skip the wrap entirely.
func WithCaps(inner LLM, caps ModelCaps) LLM {
	if caps.IsZero() {
		return inner
	}
	return &capsLLM{inner: inner, caps: caps}
}

type capsLLM struct {
	inner LLM
	caps  ModelCaps
}

func (c *capsLLM) Generate(ctx context.Context, msgs []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	msgs, err := c.preprocessMessages(ctx, msgs)
	if err != nil {
		return Message{}, TokenUsage{}, err
	}
	return c.inner.Generate(ctx, msgs, c.filtered(ctx, opts)...)
}

func (c *capsLLM) GenerateStream(ctx context.Context, msgs []Message, opts ...GenerateOption) (StreamMessage, error) {
	msgs, err := c.preprocessMessages(ctx, msgs)
	if err != nil {
		return nil, err
	}
	// CapStreaming downgrade: if the model does not support streaming,
	// fall through to Generate and wrap the single result as a
	// one-chunk stream. This preserves the caller's iterator-style
	// loop while honouring the model constraint.
	if !c.caps.Supports(CapStreaming) {
		msg, usage, err := c.inner.Generate(ctx, msgs, c.filtered(ctx, opts)...)
		if err != nil {
			return nil, err
		}
		return newOneChunkStream(msg, usage), nil
	}
	return c.inner.GenerateStream(ctx, msgs, c.filtered(ctx, opts)...)
}

// preprocessMessages applies the message-level cap rules:
//
//  1. Modality caps (Vision / Audio / File): scan parts; if any
//     unsupported modality part is present, return errdefs.Validation
//     so the caller knows their input was rejected (not silently
//     stripped — see RFC §10.2).
//  2. SystemPrompt cap: fold all RoleSystem messages into the first
//     RoleUser message as a "[System: ...]\n\n" prefix.
//
// Returned slice may share backing array with input only when no
// transformation was applied; otherwise a fresh slice is returned.
func (c *capsLLM) preprocessMessages(_ context.Context, msgs []Message) ([]Message, error) {
	if err := c.validateModalities(msgs); err != nil {
		return nil, err
	}
	if !c.caps.Supports(CapSystemPrompt) {
		msgs = foldSystemMessages(msgs)
	}
	return msgs, nil
}

func (c *capsLLM) validateModalities(msgs []Message) error {
	checkVision := !c.caps.Supports(CapVision)
	checkAudio := !c.caps.Supports(CapAudio)
	checkFile := !c.caps.Supports(CapFile)
	if !checkVision && !checkAudio && !checkFile {
		return nil
	}
	for _, m := range msgs {
		for _, p := range m.Parts {
			switch {
			case checkVision && p.Type == model.PartImage:
				return errdefs.Validationf("llm: model does not support vision input (image part in %s message)", m.Role)
			case checkAudio && p.Type == model.PartAudio:
				return errdefs.Validationf("llm: model does not support audio input (audio part in %s message)", m.Role)
			case checkFile && p.Type == model.PartFile:
				return errdefs.Validationf("llm: model does not support file input (file part in %s message)", m.Role)
			}
		}
	}
	return nil
}

// foldSystemMessages collapses every RoleSystem message into a
// "[System: ...]\n\n" prefix on the first RoleUser message. Multiple
// system messages are joined by "\n". If no user message exists, a
// new one is synthesised carrying just the system prefix. Other roles
// (assistant / tool) are preserved untouched.
//
// Format: "[System: <sys1>\n<sys2>\n...]\n\n<user-text>" — see RFC
// §10.5 for the format decision.
func foldSystemMessages(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	var sysParts []string
	hasSys := false
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			hasSys = true
			text := m.Content()
			if text != "" {
				sysParts = append(sysParts, text)
			}
		}
	}
	if !hasSys {
		return msgs
	}

	out := make([]Message, 0, len(msgs))
	prefix := ""
	if len(sysParts) > 0 {
		prefix = fmt.Sprintf("[System: %s]\n\n", strings.Join(sysParts, "\n"))
	}
	prefixUsed := prefix == "" // nothing to prepend → already "used"
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			continue
		}
		if !prefixUsed && m.Role == model.RoleUser {
			cloned := m.Clone()
			prependText(&cloned, prefix)
			out = append(out, cloned)
			prefixUsed = true
			continue
		}
		out = append(out, m)
	}
	if !prefixUsed {
		// No user message existed — synthesise one carrying the prefix.
		out = append(out, NewTextMessage(model.RoleUser, prefix))
	}
	return out
}

// prependText injects text at the front of m's first text part. If
// no text part exists, one is inserted at index 0.
func prependText(m *Message, text string) {
	for i, p := range m.Parts {
		if p.Type == model.PartText {
			m.Parts[i].Text = text + p.Text
			return
		}
	}
	prefixed := []model.Part{{Type: model.PartText, Text: text}}
	m.Parts = append(prefixed, m.Parts...)
}

// filtered returns the per-call options trail with cap-driven
// drops / downgrades appended. Each helper is conditional on the
// corresponding Supports check so the chain only does work that is
// necessary.
//
// Order of filter ops within the call: caller opts first (so they
// have a chance to set values) → drops / downgrades override last
// (so a disabled cap always wins regardless of caller intent).
func (c *capsLLM) filtered(ctx context.Context, opts []GenerateOption) []GenerateOption {
	out := make([]GenerateOption, 0, len(opts)+1)
	out = append(out, opts...)
	out = append(out, func(o *GenerateOptions) {
		// Generation params: silent drop (telemetry-noisy behavior would
		// flood logs since callers routinely set defaults that some
		// models don't take).
		if !c.caps.Supports(CapTemperature) {
			o.Temperature = nil
		}
		if !c.caps.Supports(CapTopP) {
			o.TopP = nil
		}
		if !c.caps.Supports(CapTopK) {
			o.TopK = nil
		}
		if !c.caps.Supports(CapMaxTokens) {
			o.MaxTokens = nil
		}
		if !c.caps.Supports(CapStopWords) {
			o.StopWords = nil
		}
		if !c.caps.Supports(CapFrequencyPenalty) {
			o.FrequencyPenalty = nil
		}
		if !c.caps.Supports(CapPresencePenalty) {
			o.PresencePenalty = nil
		}
		if !c.caps.Supports(CapThinking) {
			o.Thinking = nil
		}

		// JSON: schema downgrades to mode (preserved from original behavior),
		// then mode honors its own cap (overrides the downgrade if both
		// caps are disabled).
		if !c.caps.Supports(CapJSONSchema) && o.JSONSchema != nil {
			o.JSONSchema = nil
			t := true
			o.JSONMode = &t
		}
		if !c.caps.Supports(CapJSONMode) {
			o.JSONMode = nil
		}

		// Tools: warn if caller actually supplied tools that get dropped,
		// because that materially changes execution semantics (caller
		// expected the model to call something).
		if !c.caps.Supports(CapTools) {
			if len(o.Tools) > 0 || o.ToolChoice != nil {
				telemetry.Warn(ctx, "llm: dropping tools/tool_choice — model does not support tools",
					otellog.Int("dropped_tool_count", len(o.Tools)))
			}
			o.Tools = nil
			o.ToolChoice = nil
		} else if !c.caps.Supports(CapToolChoice) {
			if o.ToolChoice != nil {
				telemetry.Warn(ctx, "llm: dropping tool_choice — model does not support explicit selection")
			}
			o.ToolChoice = nil
		}
		// CapParallelTools enforcement: parallel-tool toggles are
		// carried per-provider as Extra map entries (no top-level
		// GenerateOptions field), so we strip the known protocol-
		// reserved keys when the cap is disabled. The list is kept
		// narrow to exactly the keys defined by OpenAI / Anthropic
		// chat APIs — these are reserved namespaces, not free-form
		// labels that another adapter could plausibly repurpose.
		//
		// Adapters built on those protocols (incl. OpenAI-compatible
		// wrappers like Qwen, ByteDance, MiniMax, DeepSeek) all
		// honour these key names, so the strip works uniformly.
		// New adapters introducing a different parallel-tool key
		// must register it via RegisterParallelToolExtraKey at init
		// time so the middleware can tear it down here too.
		if !c.caps.Supports(CapParallelTools) && len(o.Extra) > 0 {
			stripParallelToolExtras(ctx, o)
		}
	})
	return out
}

// parallelToolExtraKeys lists Extra map keys that adapters use to
// toggle parallel tool calling on their backend. WithCaps deletes
// these keys from GenerateOptions.Extra when the model's
// CapParallelTools is disabled, so a catalog declaration of "this
// model cannot run tools in parallel" actually takes effect at the
// request boundary instead of being a silent no-op.
//
// Default contents cover the only two protocols the SDK ships
// adapters for today:
//   - "parallel_tool_calls"       — OpenAI Chat Completions / Responses
//   - "disable_parallel_tool_use" — Anthropic Messages API (also via
//     the MiniMax /anthropic-compat endpoint)
//
// Guarded by parallelToolExtraKeysMu for the rare init-time mutation.
var (
	parallelToolExtraKeysMu sync.RWMutex
	parallelToolExtraKeys   = map[string]struct{}{
		"parallel_tool_calls":       {},
		"disable_parallel_tool_use": {},
	}
)

// RegisterParallelToolExtraKey adds an Extra-map key name to the set
// of keys WithCaps strips when CapParallelTools is disabled. New
// provider adapters introducing a non-standard parallel-tool toggle
// MUST call this from init() so the catalog cap actually enforces
// the constraint at request time.
func RegisterParallelToolExtraKey(key string) {
	if key == "" {
		return
	}
	parallelToolExtraKeysMu.Lock()
	defer parallelToolExtraKeysMu.Unlock()
	parallelToolExtraKeys[key] = struct{}{}
}

func stripParallelToolExtras(ctx context.Context, o *GenerateOptions) {
	parallelToolExtraKeysMu.RLock()
	keys := make([]string, 0, len(parallelToolExtraKeys))
	for k := range parallelToolExtraKeys {
		if _, ok := o.Extra[k]; ok {
			keys = append(keys, k)
		}
	}
	parallelToolExtraKeysMu.RUnlock()
	if len(keys) == 0 {
		return
	}
	for _, k := range keys {
		delete(o.Extra, k)
	}
	telemetry.Warn(ctx,
		"llm: dropping parallel-tool extras — model does not support parallel tool calls",
		otellog.String("dropped_keys", strings.Join(keys, ",")))
}

// ---------------------------------------------------------------------------
// oneChunkStream — streaming downgrade target used by WithCaps when
// CapStreaming is disabled. Wraps a single completed Generate result
// as a StreamMessage that yields exactly one chunk and then ends, so
// the caller's GenerateStream loop still gets a uniform iterator
// interface.
//
// Semantics:
//   - First Next() returns true; Current() yields a StreamChunk
//     synthesised from the message (Role + concatenated text +
//     ToolCalls + FinishReason="stop").
//   - Second Next() returns false. Err() returns nil.
//   - Message() returns the original message; Usage() returns the
//     captured usage.
//   - Close() is a no-op and idempotent.
// ---------------------------------------------------------------------------

func newOneChunkStream(msg Message, usage TokenUsage) *oneChunkStream {
	return &oneChunkStream{
		msg:   msg,
		usage: model.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}
}

type oneChunkStream struct {
	msg     Message
	usage   model.Usage
	emitted bool
	cur     model.StreamChunk
}

func (s *oneChunkStream) Next() bool {
	if s.emitted {
		return false
	}
	s.cur = model.StreamChunk{
		Role:         s.msg.Role,
		Content:      s.msg.Content(),
		ToolCalls:    s.msg.ToolCalls(),
		FinishReason: "stop",
	}
	s.emitted = true
	return true
}

func (s *oneChunkStream) Current() model.StreamChunk { return s.cur }
func (s *oneChunkStream) Err() error                 { return nil }
func (s *oneChunkStream) Close() error               { return nil }
func (s *oneChunkStream) Message() Message           { return s.msg }
func (s *oneChunkStream) Usage() model.Usage         { return s.usage }
