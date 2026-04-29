package bindings

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// This file implements the LLM round logic that the script bridge
// exposes via llm.run / llm.stream. It is intentionally self-contained
// and unexported: the bridge is the sole consumer, so we don't owe any
// other package backwards compatibility.
//
// Design choices baked in here (locked in during the B-track design):
//   - One round = one LLM call. If the model emits tool calls, we
//     execute them once via tool.Registry.ExecuteAll and stop. Multi-
//     turn loops are the script's job, not the bridge's.
//   - Multimodal-native: roundStream.Current returns the most recent
//     model.Part (or zero Part when the chunk had nothing useful).
//     Callers project to script-facing maps via llm_marshal.go.
//   - No host-runtime dependency: we talk to the llm.LLM interface
//     directly and own our own option translation, so the bridge is
//     usable from any caller that can build an llm.LLMResolver.

// roundResult is the rich, strongly-typed output of one bridge-managed
// LLM round. It mirrors what the script ultimately sees, but stays in
// model.* types so internal callers (other bridge code, tests) can work
// without going through the map[string]any projection.
type roundResult struct {
	// Content is the raw assistant text as reported by the LLM
	// provider (StreamMessage.Message().Content() after accumulation).
	// Per the v1 design we expose the provider value verbatim instead
	// of synthesizing it from text parts; this keeps multimodal
	// payloads honest about what the model actually returned.
	Content string

	// Message is the assistant reply, including any tool_call parts.
	Message model.Message

	// Messages is the conversation tail returned to the caller:
	//   prior history + assistant reply + (optional) tool_result reply.
	// It is a fresh slice the caller can store on the board safely.
	Messages []model.Message

	ToolCalls   []model.ToolCall
	ToolResults []model.ToolResult

	// ToolPending signals "model wanted tools and we ran them" — the
	// script typically uses this as the cue to start another round
	// with the new Messages.
	ToolPending bool

	Usage model.TokenUsage
}

// roundOptions is the bridge-internal, pre-resolved configuration for
// one round. It is produced from LLMRunOptions (script overrides) +
// LLMBridgeOptions.Defaults at the bridge facade.
type roundOptions struct {
	Model       string
	Temperature *float64
	MaxTokens   int64
	JSONMode    bool
	Thinking    bool
	ToolNames   []string
}

// generateOptions translates roundOptions into the llm.GenerateOption
// list expected by llm.LLM.GenerateStream. Tool selection is gated on
// reg being non-nil so that callers running without a registry never
// accidentally advertise tools to the model.
func (ro roundOptions) generateOptions(reg *tool.Registry) []llm.GenerateOption {
	var opts []llm.GenerateOption
	if ro.Temperature != nil {
		opts = append(opts, llm.WithTemperature(*ro.Temperature))
	}
	if ro.MaxTokens > 0 {
		opts = append(opts, llm.WithMaxTokens(ro.MaxTokens))
	}
	if ro.JSONMode {
		opts = append(opts, llm.WithJSONMode(true))
	}
	if ro.Thinking {
		opts = append(opts, llm.WithThinking(true))
	}
	if defs := selectToolDefs(reg, ro.ToolNames); len(defs) > 0 {
		opts = append(opts, llm.WithTools(defs...))
	}
	return opts
}

// selectToolDefs filters reg.Definitions() down to the names allowed
// for this round. Returning the registry verbatim when names is empty
// would leak tools the script never asked for, so we treat empty as
// "no tools".
func selectToolDefs(reg *tool.Registry, names []string) []model.ToolDefinition {
	if reg == nil || len(names) == 0 {
		return nil
	}
	allow := make(map[string]bool, len(names))
	for _, n := range names {
		allow[n] = true
	}
	var defs []model.ToolDefinition
	for _, def := range reg.Definitions() {
		if allow[def.Name] {
			defs = append(defs, def)
		}
	}
	return defs
}

// roundStream is the iterator returned to the script through
// llm.stream. It owns the underlying llm.StreamMessage and the state
// needed to assemble a roundResult on Finish.
//
// The lifecycle is strictly:
//
//	for s.Next() { _ = s.Current() }    // pull chunks (multimodal)
//	r, err := s.Finish()                // assemble + run tools
//	defer s.Close()                     // safe to call multiple times
//
// Finish calls Close internally, so callers only need their own defer
// when they bail out before Finish (e.g. on an early error).
type roundStream struct {
	ctx     context.Context
	llm     llm.LLM // not used after start, kept for diagnostics
	inner   llm.StreamMessage
	reg     *tool.Registry
	source  string // labels diagnostics, mirrors the legacy eventID concept
	history []model.Message

	// current is the projection of the most recent chunk. We zero it
	// at the start of each Next() so a chunk with neither text nor
	// tool calls produces a zero Part rather than stale data.
	current model.Part

	// textAcc captures streamed text content for diagnostic / fallback
	// reconstruction; the canonical output remains inner.Message().
	textAcc strings.Builder
}

// startRound resolves the model and opens the underlying stream.
// Failure to resolve or to open the stream surfaces as a rich error
// containing the source label so scripts can locate the failure.
func startRound(
	ctx context.Context,
	resolver llm.LLMResolver,
	reg *tool.Registry,
	source string,
	history []model.Message,
	ro roundOptions,
) (*roundStream, error) {
	if resolver == nil {
		return nil, errdefs.Validationf("llm round %q: resolver is nil", source)
	}

	l, err := resolver.Resolve(ctx, ro.Model)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: resolve %q: %w", source, ro.Model, err)
	}

	inner, err := l.GenerateStream(ctx, history, ro.generateOptions(reg)...)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: open stream: %w", source, err)
	}

	// Defensive copy so the caller can safely keep mutating the slice
	// they handed us while the round is in flight.
	hist := make([]model.Message, len(history))
	copy(hist, history)

	return &roundStream{
		ctx:     ctx,
		llm:     l,
		inner:   inner,
		reg:     reg,
		source:  source,
		history: hist,
	}, nil
}

// Next advances to the next chunk. It returns false when the stream
// has been fully consumed; callers then call Finish to retrieve the
// assembled result.
//
// Each call repopulates Current with one model.Part. We project text
// chunks to PartText so script callers see uniform shape regardless
// of whether the provider emits text or richer parts. Tool call
// fragments inside StreamChunk surface as PartToolCall. Other
// non-textual chunks fall back to a zero Part — Current() == Part{}
// — to keep the iterator deterministic.
func (s *roundStream) Next() bool {
	s.current = model.Part{}
	if !s.inner.Next() {
		return false
	}

	chunk := s.inner.Current()
	if chunk.Content != "" {
		s.textAcc.WriteString(chunk.Content)
		s.current = model.Part{Type: model.PartText, Text: chunk.Content}
		return true
	}
	if len(chunk.ToolCalls) > 0 {
		// Surface only the first tool call as the chunk's part. The
		// full assistant message (with all tool calls) is still
		// available via Finish; this projection keeps Next() / Current()
		// one-part-per-tick.
		tc := chunk.ToolCalls[0]
		s.current = model.Part{Type: model.PartToolCall, ToolCall: &tc}
	}
	return true
}

// Current returns the projection of the most recent chunk. After
// Next() returns false (or before the first Next() call) the result
// is the zero Part.
func (s *roundStream) Current() model.Part { return s.current }

// Text is a convenience for the common "text only" iteration pattern.
// Returns the empty string when the current chunk has no text.
func (s *roundStream) Text() string {
	if s.current.Type == model.PartText {
		return s.current.Text
	}
	return ""
}

// Close releases the underlying stream. It is safe to call multiple
// times: subsequent calls are no-ops.
func (s *roundStream) Close() error {
	inner := s.inner
	s.inner = nil
	if inner != nil {
		return inner.Close()
	}
	return nil
}

// Finish assembles the round result. It is mandatory to call Finish
// (or Close) before the round goes out of scope, otherwise the
// underlying stream may leak provider resources.
//
// Finish:
//  1. Surfaces any stream error.
//  2. Reads the accumulated assistant message from the provider.
//  3. If the message contains tool calls, executes them once via the
//     registry and appends the tool_result message to the conversation.
//  4. Always calls Close on success or failure.
func (s *roundStream) Finish() (*roundResult, error) {
	defer s.Close()

	if s.inner == nil {
		return nil, errdefs.NotAvailablef("llm round %q: stream already closed", s.source)
	}

	if err := s.inner.Err(); err != nil {
		return nil, fmt.Errorf("llm round %q: stream error: %w", s.source, err)
	}

	rawUsage := s.inner.Usage()
	usage := model.TokenUsage{
		InputTokens:  rawUsage.InputTokens,
		OutputTokens: rawUsage.OutputTokens,
		TotalTokens:  rawUsage.InputTokens + rawUsage.OutputTokens,
	}

	assistant := s.inner.Message()
	// Provider didn't bother building a Message but we did see text
	// chunks: synthesize a text-only assistant message so downstream
	// consumers always get a usable Message.
	if len(assistant.Parts) == 0 && s.textAcc.Len() > 0 {
		assistant = model.NewTextMessage(model.RoleAssistant, s.textAcc.String())
	}

	// Build the conversation tail: prior history + assistant reply.
	// We start fresh so the caller is never aliased into our buffer.
	out := make([]model.Message, 0, len(s.history)+2)
	out = append(out, s.history...)
	if assistant.Role != "" || len(assistant.Parts) > 0 {
		out = append(out, assistant)
	}

	calls := assistant.ToolCalls()
	var results []model.ToolResult
	toolPending := false

	if len(calls) > 0 && s.reg != nil {
		toolPending = true
		results = s.reg.ExecuteAll(s.ctx, calls)
		out = append(out, model.NewToolResultMessage(results))
	}

	return &roundResult{
		Content:     assistant.Content(),
		Message:     assistant,
		Messages:    out,
		ToolCalls:   calls,
		ToolResults: results,
		ToolPending: toolPending,
		Usage:       usage,
	}, nil
}

// runRound is the synchronous shortcut used by llm.run: drain every
// chunk and Finish in one call. The script never sees the iterator.
func runRound(
	ctx context.Context,
	resolver llm.LLMResolver,
	reg *tool.Registry,
	source string,
	history []model.Message,
	ro roundOptions,
) (*roundResult, error) {
	s, err := startRound(ctx, resolver, reg, source, history, ro)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	for s.Next() {
		// drain
	}
	return s.Finish()
}
