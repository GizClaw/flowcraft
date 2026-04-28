package llmnode

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// This file implements the LLM round logic used by Node.ExecuteBoard.
// It is deliberately self-contained: llmnode is the sole consumer, so we
// do not owe any other package backwards compatibility.
//
// Design choices baked in here:
//   - One round = one LLM call. If the model emits tool calls, we execute
//     them once via tool.Registry.ExecuteAll and stop. Multi-turn loops
//     are the graph's job (loopguard + condition edges), not the round's.
//   - Streaming events flow into graph.StreamPublisher as token / tool_call
//     / tool_result envelopes; the executor fans them onto the event bus
//     and the deprecated StreamCallback shim if a legacy caller registered
//     one.
//   - Cooperative interrupts (engine.Host.Interrupts()) are observed
//     between every chunk in the streaming loop. On interrupt, runRound
//     returns a roundResult with Interrupted set; ExecuteBoard commits
//     the partial assistant message + synthetic [cancelled] tool_results
//     to the board before propagating engine.Interrupted to the executor.
//   - No host-runtime dependency for the LLM call itself: we talk to
//     llm.LLM directly and own our own option translation.

// CancelledToolResultContent is the canonical body the round driver
// stamps onto synthetic tool_results when a tool_call could not be
// dispatched because the round was interrupted. Downstream consumers
// (memory writers, agent layer) MAY pattern-match on this exact string
// to recognise a cancelled call without losing the call→result pairing
// that LLM providers require.
const CancelledToolResultContent = "[cancelled by interrupt]"

// roundResult is the structured output of one LLM round. It mirrors the
// fields Node.writeResults consumes — keeping the contract here means
// the node never sees an llm.* round type.
type roundResult struct {
	Content     string
	Message     model.Message
	Messages    []model.Message
	ToolCalls   []model.ToolCall
	ToolResults []model.ToolResult
	ToolPending bool
	Usage       model.TokenUsage

	// Interrupted reports whether the round terminated because a
	// cooperative interrupt was observed (host.Interrupts() fired or
	// ctx was cancelled). When true the node MUST still commit the
	// result (so partial transcript + cancelled tool_results land on
	// the board) and then propagate engine.Interrupted upstream.
	Interrupted bool

	// InterruptCause carries the host's stated reason when the round
	// observed a host interrupt. Zero value means "not interrupted by
	// host" — interruption may still have happened via ctx cancel, in
	// which case Interrupted is true and InterruptCause is "".
	InterruptCause engine.Cause

	// InterruptDetail mirrors engine.Interrupt.Detail for traceability.
	InterruptDetail string
}

// generateOptions translates Config (the round-relevant subset) into
// the llm.GenerateOption list expected by llm.LLM.GenerateStream. Tool
// selection is gated on reg being non-nil so that callers running
// without a registry never accidentally advertise tools to the model.
func (c Config) generateOptions(reg *tool.Registry) []llm.GenerateOption {
	var opts []llm.GenerateOption
	if c.Temperature != nil {
		opts = append(opts, llm.WithTemperature(*c.Temperature))
	}
	if c.MaxTokens > 0 {
		opts = append(opts, llm.WithMaxTokens(c.MaxTokens))
	}
	if c.JSONMode {
		opts = append(opts, llm.WithJSONMode(true))
	}
	if c.Thinking {
		opts = append(opts, llm.WithThinking(true))
	}
	if defs := selectToolDefs(reg, c.ToolNames); len(defs) > 0 {
		opts = append(opts, llm.WithTools(defs...))
	}
	return opts
}

// selectToolDefs filters reg.Definitions() down to the names allowed
// for this round. Returning the registry verbatim when names is empty
// would leak tools the node never asked for, so we treat empty as
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

// runRound resolves the model, streams the generation while observing
// host.Interrupts(), and (when the stream finished cleanly) executes
// any tool calls once via reg.ExecuteAll.
//
// Parameters:
//   - ctx is the per-round context; cancellation triggers interrupt path.
//   - host supplies the cooperative interrupt channel; nil → no host
//     interrupts (still respects ctx).
//   - pub may be nil; when non-nil it receives token / tool_call /
//     tool_result events.
//   - eventID is used only for diagnostic strings (errors); event
//     routing is owned by the executor's publisher binding.
func runRound(
	ctx context.Context,
	host engine.Host,
	pub graph.StreamPublisher,
	resolver llm.LLMResolver,
	reg *tool.Registry,
	eventID string,
	messages []model.Message,
	cfg Config,
) (*roundResult, error) {
	if resolver == nil {
		return nil, fmt.Errorf("llm round %q: resolver is nil", eventID)
	}

	l, err := resolver.Resolve(ctx, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: resolve %q: %w", eventID, cfg.Model, err)
	}

	inner, err := l.GenerateStream(ctx, messages, cfg.generateOptions(reg)...)
	if err != nil {
		return nil, fmt.Errorf("llm round %q: open stream: %w", eventID, err)
	}
	defer inner.Close()

	// Defensive copy so the caller can keep mutating the slice they
	// handed us while the round is in flight.
	hist := make([]model.Message, len(messages))
	copy(hist, messages)

	// Snapshot the interrupt channel once — Host implementations are
	// allowed to return the same channel on repeat calls but we don't
	// rely on that.
	var intrCh <-chan engine.Interrupt
	if host != nil {
		intrCh = host.Interrupts()
	}

	var (
		textAcc         strings.Builder
		hostInterrupted bool
		hostIntr        engine.Interrupt
	)

streamLoop:
	for inner.Next() {
		select {
		case intr, ok := <-intrCh:
			if ok {
				hostInterrupted = true
				hostIntr = intr
				break streamLoop
			}
			// Closed channel: treat as no-host-interrupt and keep going.
			intrCh = nil
		case <-ctx.Done():
			break streamLoop
		default:
		}

		chunk := inner.Current()
		if chunk.Content == "" {
			continue
		}
		textAcc.WriteString(chunk.Content)
		emit(pub, "token", map[string]any{"content": chunk.Content})
	}

	streamErr := inner.Err()
	ctxInterrupted := ctx.Err() != nil

	// Drain the interrupt channel one more time even if the stream
	// loop never iterated (provider returned 0 chunks before the host
	// signal raced to land). Without this drain a host interrupt that
	// arrived during stream open would be silently lost.
	if !hostInterrupted && intrCh != nil {
		select {
		case intr, ok := <-intrCh:
			if ok {
				hostInterrupted = true
				hostIntr = intr
			}
		default:
		}
	}

	// Treat any of host.Interrupts(), ctx cancel, or a stream error
	// driven by ctx cancel as a cooperative interrupt — never a hard
	// failure. Only true provider-side stream errors fall through to
	// the error return below.
	interrupted := hostInterrupted || ctxInterrupted

	rawUsage := inner.Usage()
	usage := model.TokenUsage{
		InputTokens:  rawUsage.InputTokens,
		OutputTokens: rawUsage.OutputTokens,
		TotalTokens:  rawUsage.InputTokens + rawUsage.OutputTokens,
	}

	assistant := inner.Message()
	// Provider didn't bother building a Message but we did see text
	// chunks: synthesise a text-only assistant message so downstream
	// consumers always get a usable Message — even when interrupted
	// half-way through.
	if len(assistant.Parts) == 0 && textAcc.Len() > 0 {
		assistant = model.NewTextMessage(model.RoleAssistant, textAcc.String())
	}

	out := make([]model.Message, 0, len(hist)+2)
	out = append(out, hist...)
	if assistant.Role != "" || len(assistant.Parts) > 0 {
		out = append(out, assistant)
	}

	calls := assistant.ToolCalls()
	var results []model.ToolResult
	toolPending := false

	switch {
	case len(calls) == 0 || reg == nil:
		// Nothing to dispatch.

	case interrupted:
		// Stream was interrupted; we never got to ExecuteAll. Fabricate
		// cancelled tool_results so the LLM history stays well-formed
		// (every tool_call paired with a tool_result) and downstream
		// consumers can spot the cancel via CancelledToolResultContent.
		results = make([]model.ToolResult, 0, len(calls))
		for _, c := range calls {
			r := model.ToolResult{
				ToolCallID: c.ID,
				Content:    CancelledToolResultContent,
				IsError:    true,
			}
			results = append(results, r)
			emit(pub, "tool_result", map[string]any{
				"tool_call_id": r.ToolCallID,
				"name":         c.Name,
				"content":      r.Content,
				"is_error":     r.IsError,
				"cancelled":    true,
			})
		}
		out = append(out, model.NewToolResultMessage(results))

	default:
		toolPending = true
		for _, c := range calls {
			emit(pub, "tool_call", map[string]any{
				"id":        c.ID,
				"name":      c.Name,
				"arguments": c.Arguments,
			})
		}

		results = reg.ExecuteAll(ctx, calls)

		nameByID := make(map[string]string, len(calls))
		for _, c := range calls {
			nameByID[c.ID] = c.Name
		}
		for _, r := range results {
			emit(pub, "tool_result", map[string]any{
				"tool_call_id": r.ToolCallID,
				"name":         nameByID[r.ToolCallID],
				"content":      r.Content,
				"is_error":     r.IsError,
			})
		}

		out = append(out, model.NewToolResultMessage(results))
	}

	// Real provider errors that aren't a ctx-cancel side effect bubble
	// up so the executor classifies them as failures, not interrupts.
	if streamErr != nil && !interrupted {
		return nil, fmt.Errorf("llm round %q: stream error: %w", eventID, streamErr)
	}

	r := &roundResult{
		Content:     textAcc.String(),
		Message:     assistant,
		Messages:    out,
		ToolCalls:   calls,
		ToolResults: results,
		ToolPending: toolPending,
		Usage:       usage,
		Interrupted: interrupted,
	}
	if hostInterrupted {
		r.InterruptCause = hostIntr.Cause
		r.InterruptDetail = hostIntr.Detail
	}
	return r, nil
}

// emit is a tiny helper so the round body stays readable. nil pub is the
// common "no subscriber" case (e.g. round driven from a unit test) and
// must be cheap.
func emit(pub graph.StreamPublisher, evType string, payload map[string]any) {
	if pub == nil {
		return
	}
	pub.Emit(evType, payload)
}
