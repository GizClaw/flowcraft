package llm

// This file groups symbols that are scheduled for removal in v0.3.0,
// once the agent + engine runtime supersedes the workflow-based
// execution path. New code MUST NOT rely on anything declared here.
// The canonical migration target is documented per symbol; until then
// the helpers stay buildable so existing callers (graph/node,
// script/bindings, …) keep compiling.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ============================================================================
// Spec-redesign aliases (added 2026-04-30, scheduled for removal in v0.3.0).
//
// These symbols are kept as thin shims so that callers using the old
// caps-only / extra-caps / model-caps API surface keep compiling
// during the migration window. Internal sdk/llm code MUST use the
// new names directly (WithCaps / WithPolicyCaps / LookupModelSpec).
// See doc/sdk-llm-redesign.md §7 "Deprecation housekeeping" for the
// placement convention.
// ============================================================================

// CapsMiddleware wraps an LLM with the caps filter.
//
// Deprecated: use [WithCaps] directly. Scheduled for removal in v0.3.0;
// kept only so existing call sites compile during the migration.
func CapsMiddleware(inner LLM, caps ModelCaps) LLM {
	return WithCaps(inner, caps)
}

// WithExtraCaps merges resolver-wide caps into every produced LLM.
//
// Deprecated: renamed to [WithPolicyCaps] to make the resolver-wide
// policy intent explicit (vs per-model caps). Scheduled for removal
// in v0.3.0.
func WithExtraCaps(caps ModelCaps) ResolverOption {
	return WithPolicyCaps(caps)
}

// LookupModelCaps returns the catalog ModelCaps for a registered
// model.
//
// Deprecated: use (*ProviderRegistry).LookupModelSpec(provider,
// model).Caps. Scheduled for removal in v0.3.0.
func (r *ProviderRegistry) LookupModelCaps(provider, model string) ModelCaps {
	return r.LookupModelSpec(provider, model).Caps
}

// ============================================================================
// Workflow-era round helpers (RunRound / RoundConfig family). Scheduled
// for removal in v0.3.0 alongside the rest of the workflow package
// surface, once the agent + engine runtime owns per-call streaming
// and tool execution end-to-end.
// ============================================================================

// RoundResult is the structured output of one LLM round.
//
// Deprecated: RoundResult is part of the workflow-based round helper
// surface and is scheduled for removal in v0.3.0, once the
// agent + engine runtime owns per-call streaming and tool execution.
// Prefer building rounds on top of [LLM.GenerateStream] directly
// until the replacement lands.
type RoundResult struct {
	Content     string
	Message     Message
	Messages    []Message
	ToolCalls   []model.ToolCall
	ToolResults []model.ToolResult
	ToolPending bool
	Usage       TokenUsage
}

// RunRound executes one LLM round: resolve model → stream generation → tool
// follow-up → return result. It is a pure function: callers supply messages
// explicitly and handle board I/O themselves.
//
// stream may be nil; when non-nil, token / tool_call / tool_result events are
// emitted as they arrive (eventID labels the source in each event).
//
// Deprecated: RunRound mixes resolver, streaming, and tool follow-up
// in one helper and depends on workflow.StreamCallback. It is
// scheduled for removal in v0.3.0; the agent + engine layering will
// replace it. New code should drive LLM rounds directly via
// [LLM.GenerateStream] and surface events through the engine host.
func RunRound(
	ctx context.Context,
	stream workflow.StreamCallback,
	resolver LLMResolver,
	reg *tool.Registry,
	eventID string,
	messages []Message,
	cfg RoundConfig,
) (*RoundResult, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "llm.round",
		trace.WithAttributes(attribute.String("event.id", eventID)))
	defer span.End()

	s, err := StreamRound(ctx, stream, resolver, reg, eventID, messages, cfg)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	defer s.Close()

	for s.Next() {
	}
	result, err := s.Finish()
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int64("llm.output_tokens", result.Usage.OutputTokens),
		attribute.Bool("llm.tool_pending", result.ToolPending),
		attribute.Int("llm.tool_calls", len(result.ToolCalls)),
	)
	return result, nil
}

// RoundStream is a token-by-token iterator over an in-progress LLM round.
// Callers loop with Next() / Token(), then call Finish() for the complete result.
//
// Deprecated: RoundStream is the iterator counterpart of [RunRound]
// and is scheduled for removal in v0.3.0 alongside it. Use
// [LLM.GenerateStream] directly.
type RoundStream struct {
	ctx     context.Context
	inner   StreamMessage
	reg     *tool.Registry
	stream  workflow.StreamCallback
	eventID string

	messages []Message
	current  string
	acc      strings.Builder
}

// StreamRound starts an LLM round and returns a RoundStream for
// token-by-token iteration. Call Finish() after Next() returns false.
//
// Deprecated: scheduled for removal in v0.3.0; see [RunRound].
func StreamRound(
	ctx context.Context,
	stream workflow.StreamCallback,
	resolver LLMResolver,
	reg *tool.Registry,
	eventID string,
	messages []Message,
	cfg RoundConfig,
) (*RoundStream, error) {
	l, err := resolver.Resolve(ctx, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: cannot resolve model %q: %w", eventID, cfg.Model, err)
	}

	opts := buildRoundGenerateOptions(cfg, reg)

	inner, err := l.GenerateStream(ctx, messages, opts...)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: generate failed: %w", eventID, err)
	}

	msgsCopy := make([]Message, len(messages))
	copy(msgsCopy, messages)

	return &RoundStream{
		ctx:      ctx,
		inner:    inner,
		reg:      reg,
		stream:   stream,
		eventID:  eventID,
		messages: msgsCopy,
	}, nil
}

// Next advances to the next token chunk. Returns false when the stream is done.
func (s *RoundStream) Next() bool {
	if !s.inner.Next() {
		return false
	}
	chunk := s.inner.Current()
	s.current = chunk.Content
	if s.current != "" {
		s.acc.WriteString(s.current)
		if s.stream != nil {
			s.stream(workflow.StreamEvent{
				Type:    "token",
				NodeID:  s.eventID,
				Payload: map[string]any{"content": s.current},
			})
		}
	}
	return true
}

// Token returns the content string of the current chunk.
func (s *RoundStream) Token() string { return s.current }

// Close releases the underlying stream resources. It is safe to call
// multiple times. Finish calls Close automatically on success; callers
// should defer Close when there is any chance of early return.
func (s *RoundStream) Close() error {
	inner := s.inner
	s.inner = nil
	if inner != nil {
		return inner.Close()
	}
	return nil
}

// Finish completes the round: checks stream error, executes tool calls (if any),
// and returns the full RoundResult. It closes the underlying stream on success.
func (s *RoundStream) Finish() (*RoundResult, error) {
	defer s.Close()

	if err := s.inner.Err(); err != nil {
		return nil, fmt.Errorf("llm round %q: stream error: %w", s.eventID, err)
	}

	rawUsage := s.inner.Usage()
	usage := TokenUsage{
		InputTokens:  rawUsage.InputTokens,
		OutputTokens: rawUsage.OutputTokens,
		TotalTokens:  rawUsage.InputTokens + rawUsage.OutputTokens,
	}

	accMsg := s.inner.Message()
	if s.acc.Len() > 0 && len(accMsg.Parts) == 0 {
		accMsg = NewTextMessage(RoleAssistant, s.acc.String())
	}

	messages := s.messages
	if accMsg.Role != "" || len(accMsg.Parts) > 0 {
		messages = append(messages, accMsg)
	}

	toolCalls := accMsg.ToolCalls()
	toolPending := false
	var toolResults []model.ToolResult

	if len(toolCalls) > 0 && s.reg != nil {
		toolPending = true

		if s.stream != nil {
			for _, tc := range toolCalls {
				s.stream(workflow.StreamEvent{
					Type:    "tool_call",
					NodeID:  s.eventID,
					Payload: map[string]any{"id": tc.ID, "name": tc.Name, "arguments": tc.Arguments},
				})
			}
		}

		toolResults = s.reg.ExecuteAll(s.ctx, toolCalls)

		if s.stream != nil {
			tcNames := make(map[string]string, len(toolCalls))
			for _, tc := range toolCalls {
				tcNames[tc.ID] = tc.Name
			}
			for _, r := range toolResults {
				s.stream(workflow.StreamEvent{
					Type:   "tool_result",
					NodeID: s.eventID,
					Payload: map[string]any{
						"tool_call_id": r.ToolCallID,
						"name":         tcNames[r.ToolCallID],
						"content":      r.Content,
						"is_error":     r.IsError,
					},
				})
			}
		}

		messages = append(messages, NewToolResultMessage(toolResults))
	}

	return &RoundResult{
		Content:     s.acc.String(),
		Message:     accMsg,
		Messages:    messages,
		ToolCalls:   toolCalls,
		ToolResults: toolResults,
		ToolPending: toolPending,
		Usage:       usage,
	}, nil
}

func buildRoundGenerateOptions(cfg RoundConfig, reg *tool.Registry) []GenerateOption {
	var opts []GenerateOption
	if cfg.Temperature != nil {
		opts = append(opts, WithTemperature(*cfg.Temperature))
	}
	if cfg.MaxTokens > 0 {
		opts = append(opts, WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Thinking {
		opts = append(opts, WithThinking(true))
	}
	if cfg.JSONMode {
		opts = append(opts, WithJSONMode(true))
	}

	var toolDefs []ToolDefinition
	if reg != nil && len(cfg.ToolNames) > 0 {
		allowed := make(map[string]bool, len(cfg.ToolNames))
		for _, name := range cfg.ToolNames {
			allowed[name] = true
		}
		for _, def := range reg.Definitions() {
			if allowed[def.Name] {
				toolDefs = append(toolDefs, def)
			}
		}
	}
	if len(toolDefs) > 0 {
		opts = append(opts, WithTools(toolDefs...))
	}
	return opts
}

// RoundConfig configures the LLM call parameters for one round.
// Board I/O (system prompt injection, output routing, etc.) is the caller's responsibility.
//
// Deprecated: RoundConfig is the input shape consumed by [RunRound]
// and is scheduled for removal in v0.3.0 alongside it. Configure
// individual [GenerateOption]s on the agent / engine side once the
// replacement lands.
type RoundConfig struct {
	Model       string   `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	JSONMode    bool     `json:"json_mode,omitempty" yaml:"json_mode,omitempty"`
	Thinking    bool     `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty" yaml:"tool_names,omitempty"`
}

// CoerceMapForStruct uses reflection on T's json tags to coerce string values
// in m to the numeric/bool types expected by the target struct fields. This
// allows JSON round-trip (Marshal → Unmarshal) to succeed when map values
// arrive as strings (e.g. from ${board.temperature} template resolution).
//
// isDeferred, when non-nil, reports whether a string value is a deferred
// reference (e.g. a template variable) that will be resolved later. Such
// values are removed from non-string fields so that json.Unmarshal sees the
// zero value instead of an invalid string. The caller should supply the
// resolver's own ContainsRef function to keep detection in sync.
//
// The input map is not modified; a shallow clone is returned.
//
// Deprecated: CoerceMapForStruct only exists to support
// [RoundConfigFromMap]. It is scheduled for removal in v0.3.0
// alongside [RoundConfig].
func CoerceMapForStruct[T any](m map[string]any, isDeferred func(string) bool) map[string]any {
	if m == nil {
		return nil
	}
	var zero T
	t := reflect.TypeOf(zero)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return m
	}

	result := maps.Clone(m)
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		key, _, _ := strings.Cut(tag, ",")
		if key == "" {
			continue
		}
		val, ok := result[key]
		if !ok {
			continue
		}
		str, ok := val.(string)
		if !ok {
			continue
		}

		target := field.Type
		if target.Kind() == reflect.Ptr {
			target = target.Elem()
		}
		if target.Kind() == reflect.String {
			continue
		}
		if coerced, ok := coerceString(str, target.Kind()); ok {
			result[key] = coerced
		} else if isDeferred != nil && isDeferred(str) {
			delete(result, key)
		}
	}
	return result
}

func coerceString(s string, kind reflect.Kind) (any, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	switch kind {
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return i, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return u, true
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, false
		}
		return b, true
	default:
		return nil, false
	}
}

// RoundConfigFromMap parses RoundConfig from a generic map via JSON round-trip.
// isDeferred is passed through to CoerceMapForStruct; see its documentation.
//
// Deprecated: scheduled for removal in v0.3.0; see [RoundConfig].
func RoundConfigFromMap(m map[string]any, isDeferred func(string) bool) (RoundConfig, error) {
	var cfg RoundConfig
	if m == nil {
		return cfg, nil
	}
	m = CoerceMapForStruct[RoundConfig](m, isDeferred)
	data, err := json.Marshal(m)
	if err != nil {
		return cfg, fmt.Errorf("llm: marshal config map: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("llm: unmarshal config: %w", err)
	}
	return cfg, nil
}
