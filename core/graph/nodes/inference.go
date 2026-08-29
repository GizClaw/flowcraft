package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"github.com/GizClaw/flowcraft/core/message"
	otellog "go.opentelemetry.io/otel/log"
)

// InferenceConfig is the config of the "inference" node type. Board
// references (${board:*}) are resolved per invocation before decode,
// so fields like system_prompt may interpolate upstream output.
type InferenceConfig struct {
	// Model targets a specific model through the wired Runtime. When
	// absent the node defers target selection to the wired Router.
	Model *inference.ModelRef `json:"model,omitempty"`

	// ModelHint is an optional per-call model preference passed to the
	// wired Router's generate selection: "provider/name" or a bare model
	// name (e.g. "${board:model}"). The router honors it only when it
	// names a configured target that can serve the request; otherwise it
	// falls back to the default routing policy. Ignored when Model is
	// set, since a static model bypasses the router entirely.
	ModelHint string `json:"model_hint,omitempty"`

	// MessagesChannel names the board channel holding the
	// conversation; empty means the main channel. The channel's tail
	// message is the current turn's input and must have role user or
	// tool — everything before it becomes the request context.
	MessagesChannel string `json:"messages_channel,omitempty"`

	// SystemPrompt is prepended as a system message when the context
	// does not already start with one.
	SystemPrompt string `json:"system_prompt,omitempty"`

	// OutputKey, when set, receives the full assistant Message.
	OutputKey string `json:"output_key,omitempty"`
	// UsageKey, when set, receives the call's inference.Usage.
	UsageKey string `json:"usage_key,omitempty"`
	// ToolPendingKey, when set, receives whether the finish reason is
	// tool_calls — the boolean condition edges branch on to route
	// through a tool node and loop back.
	ToolPendingKey string `json:"tool_pending_key,omitempty"`
	// UndefinedToolRecovery converts a generate rejection for a tool
	// call absent from the exposed definitions into recoverable
	// feedback instead of failing the node. The rejected call is not
	// replayed: the node stores a user-role feedback text under
	// RecoverFeedbackKey (or the reserved defaultRecoverFeedbackKey var
	// when unset) and the graph routes on RecoverPendingKey back to
	// inference, where the stored feedback becomes the next round's
	// input without ever being written to the messages channel. Strict
	// deployments leave this disabled and keep failing hard.
	UndefinedToolRecovery *UndefinedToolRecoveryConfig `json:"undefined_tool_recovery,omitempty"`
	// RecoverPendingKey, when set, receives whether the current round
	// was recovered from an undefined-tool response. Graph conditions
	// route on it to loop back to inference.
	RecoverPendingKey string `json:"recover_pending_key,omitempty"`
	// RecoverCountKey, when set, receives the per-run recovery counter.
	// The node hard-fails once it reaches MaxPerRun.
	RecoverCountKey string `json:"recover_count_key,omitempty"`
	// RecoverFeedbackKey, when set, receives the user-role feedback
	// text describing the rejected tool call. The stored text becomes
	// the recovered round's current input while the messages channel
	// stays free of engine-generated turns, so UIs render a pure user
	// transcript. Empty defaults to a reserved per-node var named
	// "__recover_feedback.<node id>", so concurrent recovery on
	// different inference nodes never shares a slot.
	RecoverFeedbackKey string `json:"recover_feedback_key,omitempty"`

	// Stream opens a GenerateStream and publishes text and reasoning
	// deltas as token stream events. Reasoning fragments arrive as
	// incremental message.ReasoningPart deltas: consumers concatenate
	// Text and take Signature/ID from the terminal fragment. The board
	// still receives exactly one assembled message (tool_call parts
	// included).
	Stream bool `json:"stream,omitempty"`

	// Tools names the catalog tools the model may call this turn.
	Tools []string `json:"tools,omitempty"`
	// AllTools sends the catalog's entire visible set instead of only
	// the named Tools. The node stays catalog-agnostic: whatever
	// Definitions returns (a dynamic injection view, a filtered view,
	// the plain registry) is what the model sees. When Tools is also
	// set, the names are declared to the catalog as RequiredByName via
	// an optional interface and must still exist.
	AllTools bool `json:"all_tools,omitempty"`
	// ToolChoice constrains when/which tools are called and rides the
	// text intent next to Tools.
	ToolChoice *inference.ToolChoice `json:"tool_choice,omitempty"`

	// Intent is the canonical execution envelope: one or more of the
	// text, image, audio, and video modalities with their controls
	// (see inference.Intent). It is authoritative — when set, the
	// node builds the request from it directly. Tools / AllTools /
	// ToolChoice stay node-level sugar that resolves the wired catalog
	// into intent.text.tools / intent.text.tool_choice. When Intent is
	// absent the node defaults to plain text generation.
	Intent *inference.Intent `json:"intent,omitempty"`

	// Extensions names provider knobs in the shared {provider, id,
	// fields} wire form (see inference.DecodeExtensions). Decoders are
	// provider-carried: the deploy path aggregates them from the wired
	// inference assembly, so only configured providers are available.
	Extensions []inference.ExtensionEntry `json:"extensions,omitempty"`
}

// UndefinedToolRecoveryConfig configures the inference node's
// recoverable handling of undefined-tool responses.
type UndefinedToolRecoveryConfig struct {
	// Enabled turns recovery on. Defaults to false: undefined tool
	// calls fail the node exactly as before.
	Enabled bool `json:"enabled,omitempty"`
	// MaxPerRun caps how many undefined-tool responses one graph run
	// converts into feedback before failing hard. Zero falls back to 2.
	MaxPerRun int `json:"max_per_run,omitempty"`
}

// defaultUndefinedToolMaxRecoveries bounds how many undefined-tool
// responses one graph run converts into feedback before failing hard.
const defaultUndefinedToolMaxRecoveries = 2

// defaultRecoverFeedbackPrefix is the reserved board-var prefix the
// recovery flow uses when RecoverFeedbackKey is not configured; the
// recovering node's id is appended (e.g. "__recover_feedback.llm"), so
// the slot is scoped per node. It lives in the engine's "__" namespace
// (see agent.MainChannel); user-domain code must not introduce keys
// with that prefix.
const defaultRecoverFeedbackPrefix = "__recover_feedback."

// undefinedToolFeedback is the model-readable rejection appended as a
// user feedback message when a response names a tool the model was
// never shown.
const undefinedToolFeedback = "tool %q is not exposed in this round's tool set; " +
	"call tool_search to find and select it before calling it again"

// InferenceNodeDeps wires the inference node's collaborators. Runtime
// serves configs carrying an explicit model; Router serves configs
// without one. Either may be nil if no graph needs it — the error
// surfaces at invocation, classified NotAvailable.
type InferenceNodeDeps struct {
	Assembly *inference.Assembly
	Router   *route.Router
	// Catalog resolves config tool names into definitions; required
	// only when a graph configures tools.
	Catalog tool.Catalog
	// Extensions maps "provider/id" to decoders, the same registry
	// shape the script bridge wires with bindings.WithExtensionDecoder.
	// The deploy path populates it from the inference assembly's
	// provider-carried decoders.
	Extensions map[string]inference.ExtensionDecoder
}

// Inference returns the "inference" node type: one Generate call per
// invocation, channel tail in, one assistant message appended. The
// node never executes tool calls — finish_reason == tool_calls is
// flagged onto tool_pending_key and the graph routes onward.
func Inference(deps InferenceNodeDeps) graph.NodeType[InferenceConfig] {
	return graph.NodeType[InferenceConfig]{
		Meta: graph.Meta{
			Desc: "single-shot generation (text, image, audio, video): channel tail in, one assistant message out",
			Reads: []graph.Role{
				{Kind: graph.RoleMessages, ConfigKey: "messages_channel"},
			},
			Writes: []graph.Role{
				{Kind: graph.RoleMessages, ConfigKey: "messages_channel"},
				{Kind: graph.RoleVar, ConfigKey: "output_key"},
				{Kind: graph.RoleVar, ConfigKey: "usage_key"},
				{Kind: graph.RoleVar, ConfigKey: "tool_pending_key"},
				{Kind: graph.RoleVar, ConfigKey: "recover_pending_key"},
				{Kind: graph.RoleVar, ConfigKey: "recover_count_key"},
				{Kind: graph.RoleVar, ConfigKey: "recover_feedback_key"},
			},
		},
		Handler: func(ec graph.ExecutionContext, board *agent.Board, cfg InferenceConfig) error {
			return runInference(ec, board, cfg, deps)
		},
	}
}

// RegisterInference registers the "inference" node type into reg.
func RegisterInference(reg *graph.Registry, deps InferenceNodeDeps) error {
	return graph.RegisterType(reg, "inference", Inference(deps))
}

func runInference(ec graph.ExecutionContext, board *agent.Board, cfg InferenceConfig, deps InferenceNodeDeps) error {
	channel := cfg.MessagesChannel
	if channel == "" {
		channel = agent.MainChannel
	}
	if cfg.UndefinedToolRecovery != nil && cfg.UndefinedToolRecovery.Enabled &&
		(cfg.RecoverPendingKey == "" || cfg.RecoverCountKey == "") {
		return errdefs.Validationf(
			"inference node: undefined_tool_recovery requires recover_pending_key and recover_count_key")
	}
	req, err := buildGenerateRequest(ec, board, channel, cfg, deps)
	if err != nil {
		return err
	}

	resp, err := executeGenerate(ec, board, cfg, deps, req)
	if err != nil {
		return recoverUndefinedTool(ec, board, channel, cfg, err)
	}
	if cfg.RecoverPendingKey != "" {
		// A successful round clears the recovery marker so the loop
		// returns to its normal edges.
		board.SetVar(cfg.RecoverPendingKey, false)
	}
	if cfg.UndefinedToolRecovery != nil && cfg.UndefinedToolRecovery.Enabled {
		// Stale feedback from a previous recovery must not leak into a
		// later round once the loop has returned to its normal edges.
		// Only nodes participating in recovery touch their own slot, so
		// an unrelated node's success never clears another node's
		// pending feedback.
		board.DeleteVar(recoverFeedbackKey(ec, cfg))
	}
	// Mirror the provider request / response identifiers and token
	// usage onto the node span after a successful call so
	// llm.request.id / llm.response.id / llm.tokens.* are visible in
	// otel even when the call did not go through a router (failure ids
	// already ride the error chain via execute.go).
	inference.RecordLLMTelemetry(ec.Context, resp.Metadata, resp.Usage, nil)
	if len(cfg.Tools) > 0 || cfg.AllTools {
		if catalog := roundCatalog(ec.Context, cfg, deps); catalog != nil {
			if advancer, ok := catalog.(roundAdvancer); ok {
				advancer.AdvanceTurn()
			}
		}
	}

	// Stream every part of the response: text and reasoning (unless
	// they were already streamed incrementally), images, audio, files,
	// tool calls and results — each as one StreamDeltaPart. The board
	// still receives the complete assembled message.
	for _, part := range resp.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return err
		}
		if cfg.Stream {
			switch normalized.(type) {
			case message.TextPart, message.ReasoningPart:
				continue // already streamed incrementally
			}
		}
		if err := ec.EmitStreamDelta(agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart,
			Part: normalized,
		}); err != nil {
			return err
		}
	}

	board.AppendChannelMessage(channel, resp.Message)
	if cfg.OutputKey != "" {
		board.SetVar(cfg.OutputKey, resp.Message)
	}
	if cfg.UsageKey != "" {
		board.SetVar(cfg.UsageKey, resp.Usage)
	}
	if cfg.ToolPendingKey != "" {
		board.SetVar(cfg.ToolPendingKey, resp.FinishReason == inference.FinishToolCalls)
	}
	if ec.Host != nil {
		if err := ec.Host.ReportUsage(ec.Context, resp.Usage); err != nil {
			return err
		}
	}
	if err := emitGenerationTerminal(ec, resp); err != nil {
		return err
	}
	return nil
}

// recoverUndefinedTool converts an undefined-tool generate failure into
// recoverable feedback when the node is configured to recover. The
// rejection is stored as a user-role feedback text under
// RecoverFeedbackKey (or the per-node reserved
// "__recover_feedback.<node id>" var) — it is deliberately NOT appended
// to the messages channel, so the user-visible transcript never shows
// an engine-generated turn. The recovered round (routed back to
// inference on RecoverPendingKey) consumes the stored text as its
// current input with the whole channel as context; see
// buildGenerateRequest. The rejected call is never replayed as a
// synthetic assistant tool_call: that would fabricate a turn the model
// never produced, violate provider reasoning round-trip rules (DeepSeek
// thinking mode requires reasoning_content on assistant tool-call
// turns), and leave an undefined function reference in history for
// providers that validate calls against the request tool set. The graph
// must still route back to inference, never through a tool node, or the
// call would be executed for real. The original error is returned when
// recovery is disabled, the failure is not an undefined-tool rejection,
// or the per-run budget is exhausted.
func recoverUndefinedTool(ec graph.ExecutionContext, board *agent.Board, channel string, cfg InferenceConfig, err error) error {
	if cfg.UndefinedToolRecovery == nil || !cfg.UndefinedToolRecovery.Enabled {
		return err
	}
	var infErr *inference.Error
	if !errors.As(err, &infErr) ||
		infErr.Kind != inference.UndefinedTool ||
		infErr.UndefinedToolCall == nil {
		return err
	}
	max := cfg.UndefinedToolRecovery.MaxPerRun
	if max <= 0 {
		max = defaultUndefinedToolMaxRecoveries
	}
	count := recoveryCount(board, cfg)
	if count >= max {
		return err
	}
	call := *infErr.UndefinedToolCall

	// The failed stream materialized its buffered text on the channel
	// before the error propagated (see drainGenerateStream). The turn is
	// being rolled back for a retry, so that rejected text must not stay
	// in the transcript: the recovered round appends its own complete
	// message right after it, leaving adjacent assistant messages that
	// violate provider reasoning round-trip rules on the next request.
	// Nothing is removed when the tail is not that text-only
	// materialization (unary failures and streams that buffered no text
	// never commit one).
	rollbackFailedTurnText(board, channel)

	board.SetVar(recoverFeedbackKey(ec, cfg), fmt.Sprintf(undefinedToolFeedback, call.Name))
	if cfg.ToolPendingKey != "" {
		board.SetVar(cfg.ToolPendingKey, false)
	}
	board.SetVar(cfg.RecoverPendingKey, true)
	board.SetVar(cfg.RecoverCountKey, count+1)
	telemetry.Warn(ec.Context, "inference node: recovered undefined tool call",
		otellog.String("node.type", "inference"),
		otellog.String("tool.name", call.Name),
		otellog.Int("recovery.count", count+1))
	return nil
}

// rollbackFailedTurnText removes the standalone text-only assistant
// message a failed stream committed to the channel. It is a no-op when
// the channel tail is not that exact materialization, so ordinary tails
// (user, tool, tool result) are never touched.
func rollbackFailedTurnText(board *agent.Board, channel string) {
	msgs := board.Channel(channel)
	if len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1]
	if last.Role != message.RoleAssistant {
		return
	}
	for _, part := range last.Content.Parts {
		if _, ok := part.(message.TextPart); !ok {
			return
		}
	}
	board.PopChannelMessage(channel)
}

// recoveryCount reads the per-run undefined-tool recovery counter.
func recoveryCount(board *agent.Board, cfg InferenceConfig) int {
	count, ok := board.GetVar(cfg.RecoverCountKey)
	if !ok {
		return 0
	}
	n, ok := count.(int)
	if !ok {
		return 0
	}
	return n
}

// emitGenerationTerminal publishes the terminal stream deltas of one
// successful generation: the final provider-owned outputs (when any)
// followed by the finish signal carrying the normalized finish reason
// and the provider-issued request / response identifiers. It fixes the
// node's previous silent drop of ProviderOutputs and gives downstream
// stream consumers a definitive end-of-generation event.
func emitGenerationTerminal(ec graph.ExecutionContext, resp inference.GenerateResponse) error {
	if len(resp.ProviderOutputs) > 0 {
		envelopes := make([]agent.ProviderOutputEnvelope, 0, len(resp.ProviderOutputs))
		for _, output := range resp.ProviderOutputs {
			raw, err := json.Marshal(output)
			if err != nil {
				return errdefs.Validationf(
					"inference node: marshal provider output %q/%q: %v",
					output.ProviderID(), output.ExtensionID(), err)
			}
			envelopes = append(envelopes, agent.ProviderOutputEnvelope{
				Provider:  output.ProviderID(),
				Extension: output.ExtensionID(),
				Value:     raw,
			})
		}
		if err := ec.EmitStreamDelta(agent.StreamDeltaPayload{
			Type:            agent.StreamDeltaProviderOutputs,
			ProviderOutputs: envelopes,
		}); err != nil {
			return err
		}
	}
	return ec.EmitStreamDelta(agent.StreamDeltaPayload{
		Type:         agent.StreamDeltaFinish,
		FinishReason: string(resp.FinishReason),
		RequestID:    resp.Metadata.RequestID,
		ResponseID:   resp.Metadata.ResponseID,
	})
}

// buildGenerateRequest splits the channel tail into context + current
// input — the exact shape GenerateRequest demands — and attaches the
// configured intent and extensions.
func buildGenerateRequest(ec graph.ExecutionContext, board *agent.Board, channel string, cfg InferenceConfig, deps InferenceNodeDeps) (inference.GenerateRequest, error) {
	var req inference.GenerateRequest
	messages := board.Channel(channel)
	if len(messages) == 0 {
		return req, errdefs.Validationf("inference node: messages channel %q is empty", channel)
	}

	intent, err := resolveIntent(ec.Context, cfg, deps)
	if err != nil {
		return req, err
	}

	extensions, err := inference.DecodeExtensions(cfg.Extensions, deps.Extensions, "inference node extensions")
	if err != nil {
		return req, err
	}

	// A recovered round feeds the stored feedback as its current input
	// instead of a channel tail: the feedback is engine-generated and
	// never written to the user-visible transcript. The whole channel
	// (including the rejected assistant tool_calls turn) is the
	// context, with the system prompt prepended exactly as on a normal
	// round.
	if recoveryPending(board, cfg) {
		feedbackKey := recoverFeedbackKey(ec, cfg)
		feedback := board.GetVarString(feedbackKey)
		if feedback == "" {
			return req, errdefs.Validationf(
				"inference node: recovery pending but var %q holds no feedback text",
				feedbackKey)
		}
		contextMessages := messages
		if cfg.SystemPrompt != "" &&
			(len(contextMessages) == 0 || contextMessages[0].Role != message.RoleSystem) {
			contextMessages = append(
				[]message.Message{message.NewTextMessage(message.RoleSystem, cfg.SystemPrompt)},
				contextMessages...,
			)
		}
		return inference.GenerateRequest{
			Context: contextMessages,
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.NewTextMessage(message.RoleUser, feedback).Content,
					Intent:  intent,
				},
			},
			Extensions: extensions,
			ModelHint:  cfg.ModelHint,
		}, nil
	}

	last := messages[len(messages)-1]
	var inputRole inference.InputRole
	switch last.Role {
	case message.RoleUser:
		inputRole = inference.InputRoleUser
	case message.RoleTool:
		inputRole = inference.InputRoleTool
	default:
		return req, errdefs.Validationf(
			"inference node: last message on channel %q must have role user or tool, got %q",
			channel, last.Role)
	}
	contextMessages := messages[:len(messages)-1]
	if cfg.SystemPrompt != "" &&
		(len(contextMessages) == 0 || contextMessages[0].Role != message.RoleSystem) {
		contextMessages = append(
			[]message.Message{message.NewTextMessage(message.RoleSystem, cfg.SystemPrompt)},
			contextMessages...,
		)
	}

	return inference.GenerateRequest{
		Context: contextMessages,
		Input: inference.GenerateInput{
			Role: inputRole,
			Content: inference.InputContent{
				Content: last.Content,
				Intent:  intent,
			},
		},
		Extensions: extensions,
		ModelHint:  cfg.ModelHint,
	}, nil
}

// recoveryPending reports whether the board is mid-recovery: the
// previous round was converted into feedback and the graph looped back
// to this node. The feedback var must be configured for recovery, so an
// empty key makes this never match (the guard also covers validation
// failures at node start).
func recoveryPending(board *agent.Board, cfg InferenceConfig) bool {
	if cfg.UndefinedToolRecovery == nil || !cfg.UndefinedToolRecovery.Enabled ||
		cfg.RecoverPendingKey == "" {
		return false
	}
	pending, ok := board.GetVar(cfg.RecoverPendingKey)
	return ok && pending == true
}

// recoverFeedbackKey returns the configured feedback var, falling back
// to the engine-reserved per-node default
// ("__recover_feedback.<node id>") when the config leaves it unset.
func recoverFeedbackKey(ec graph.ExecutionContext, cfg InferenceConfig) string {
	if cfg.RecoverFeedbackKey != "" {
		return cfg.RecoverFeedbackKey
	}
	return defaultRecoverFeedbackPrefix + ec.NodeID
}

// resolveIntent assembles the invocation's canonical execution
// envelope. The intent config is authoritative; the legacy tools /
// all_tools / tool_choice knobs are node-level sugar that resolve the
// wired catalog into intent.text.tools / intent.text.tool_choice. When
// no intent is configured the node keeps the historical behavior: a
// text intent, tools-first when tools are configured. The assembled
// intent is validated here so configuration errors surface before any
// provider I/O.
func resolveIntent(ctx context.Context, cfg InferenceConfig, deps InferenceNodeDeps) (inference.Intent, error) {
	var intent inference.Intent
	if cfg.Intent != nil {
		intent = cfg.Intent.Clone()
	} else {
		// Default shape: plain text generation.
		intent.Text = &inference.TextIntent{}
	}
	if len(cfg.Tools) > 0 || cfg.AllTools || cfg.ToolChoice != nil {
		if intent.Text == nil {
			return intent, errdefs.Validationf(
				"inference node: tools/tool_choice configured but intent has no text modality")
		}
		if len(intent.Text.Tools) > 0 || intent.Text.ToolChoice != nil {
			return intent, errdefs.Validationf(
				"inference node: tools/tool_choice declared both in intent and via node config")
		}
		if len(cfg.Tools) > 0 || cfg.AllTools {
			definitions, err := toolDefinitions(ctx, cfg.Tools, deps.Catalog, cfg.AllTools)
			if err != nil {
				return intent, err
			}
			intent.Text.Tools = definitions
		}
		intent.Text.ToolChoice = cfg.ToolChoice
	}
	if err := intent.Validate(); err != nil {
		return intent, errdefs.Validationf("inference node: intent: %v", err)
	}
	return intent, nil
}

func toolDefinitions(ctx context.Context, names []string, catalog tool.Catalog, allTools bool) ([]message.ToolDefinition, error) {
	if catalog == nil {
		return nil, errdefs.Validationf("inference node: tools configured but no tool catalog wired")
	}
	if allTools {
		if override, ok := tool.SessionFromContext(ctx); ok {
			catalog = override
		}
		return resolveAllTools(names, catalog)
	}
	return resolveExplicitTools(names, catalog)
}

// requiredCatalog is implemented by catalogs that accept RequiredByName
// declarations. The inference node never assumes one: it is an optional
// contract a catalog (like the dynamic injection view) may implement.
type requiredCatalog interface {
	Require(names ...string)
}

// roundAdvancer is the optional per-round lifecycle contract: a
// session-scoped catalog advances its Selected retention once per
// inference node invocation, which is the correct granularity for M
// rounds (a single user turn may contain several rounds).
type roundAdvancer interface {
	AdvanceTurn()
}

// roundCatalog resolves the catalog this round's request was built
// from: the context override for all_tools mode, otherwise the bound
// dependency.
func roundCatalog(ctx context.Context, cfg InferenceConfig, deps InferenceNodeDeps) tool.Catalog {
	if cfg.AllTools {
		if override, ok := tool.SessionFromContext(ctx); ok {
			return override
		}
	}
	return deps.Catalog
}

func resolveExplicitTools(names []string, catalog tool.Catalog) ([]message.ToolDefinition, error) {
	available := make(map[string]message.ToolDefinition)
	for _, def := range catalog.Definitions() {
		available[def.Name] = def
	}
	definitions := make([]message.ToolDefinition, len(names))
	for i, name := range names {
		def, ok := available[name]
		if !ok {
			return nil, errdefs.Validationf("inference node: unknown tool %q", name)
		}
		definitions[i] = def
	}
	return definitions, nil
}

func resolveAllTools(names []string, catalog tool.Catalog) ([]message.ToolDefinition, error) {
	if rc, ok := catalog.(requiredCatalog); ok {
		rc.Require(names...)
	}
	definitions := catalog.Definitions()
	if len(names) == 0 {
		return definitions, nil
	}
	available := make(map[string]struct{}, len(definitions))
	for _, def := range definitions {
		available[def.Name] = struct{}{}
	}
	for _, name := range names {
		if _, ok := available[name]; !ok {
			return nil, errdefs.Validationf("inference node: unknown tool %q", name)
		}
	}
	return definitions, nil
}

func executeGenerate(ec graph.ExecutionContext, board *agent.Board, cfg InferenceConfig, deps InferenceNodeDeps, req inference.GenerateRequest) (inference.GenerateResponse, error) {
	if cfg.Stream {
		return executeGenerateStream(ec, board, cfg, deps, req)
	}
	if cfg.Model != nil {
		if deps.Assembly == nil {
			return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: model configured but no runtime wired")
		}
		return deps.Assembly.Generate(ec.Context, *cfg.Model, req)
	}
	if deps.Router == nil {
		return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: no model configured and no router wired")
	}
	resp, _, err := deps.Router.Generate(ec.Context, req)
	return resp, err
}

// executeGenerateStream drains a GenerateStream through a
// MessageStream: each text delta is buffered and published as a token
// event, and each reasoning delta is published incrementally as a
// reasoning part. On success the caller appends the driver-accumulated
// response (complete message, tool_calls included). On a mid-stream
// failure — driver error or run interruption — the buffered partial
// text is committed to the board as one assistant message before the
// error propagates, so downstream consumers and a host-saved board keep
// the progress instead of silently losing every token. Partial
// reasoning is streamed but not committed to the board: an unsigned
// fragment must not round-trip into a conversation context that
// requires signed reasoning. The last cumulative usage snapshot seen
// before the failure is still reported to the host, so budget
// accounting observes the tokens the provider already billed.
func executeGenerateStream(ec graph.ExecutionContext, board *agent.Board, cfg InferenceConfig, deps InferenceNodeDeps, req inference.GenerateRequest) (inference.GenerateResponse, error) {
	var stream inference.GenerateStream
	var err error
	if cfg.Model != nil {
		if deps.Assembly == nil {
			return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: model configured but no runtime wired")
		}
		stream, err = deps.Assembly.GenerateStream(ec.Context, *cfg.Model, req)
	} else {
		if deps.Router == nil {
			return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: no model configured and no router wired")
		}
		stream, _, err = deps.Router.GenerateStream(ec.Context, req)
	}
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	defer func() {
		if cerr := stream.Close(); cerr != nil {
			telemetry.WarnErr(ec.Context, "inference node: close stream after drain", cerr,
				otellog.String("node.type", "inference"))
		}
	}()

	return drainGenerateStream(ec, board, cfg.MessagesChannel, stream)
}

func drainGenerateStream(ec graph.ExecutionContext, board *agent.Board, channel string, stream inference.GenerateStream) (response inference.GenerateResponse, err error) {
	s := ec.NewMessageStream(channel)
	var (
		lastUsage inference.Usage
		usageSeen bool
	)
	reportPartialUsage := func() {
		if !usageSeen || ec.Host == nil {
			return
		}
		if reportErr := ec.Host.ReportUsage(ec.Context, lastUsage); reportErr != nil {
			telemetry.WarnErr(ec.Context, "inference node: report partial usage", reportErr,
				otellog.String("node.type", "inference"),
				otellog.String("channel", channel))
		}
	}
	defer func() {
		if err != nil {
			// The provider may have billed tokens before the stream
			// failed; surface the last cumulative usage snapshot so
			// budget accounting still observes the partial spend.
			reportPartialUsage()
			// Preserve the original stream error; partial materialization
			// is best effort, but if Close itself fails the caller should
			// still see why their partial materialization didn't land.
			if _, cerr := s.Close(board); cerr != nil {
				telemetry.WarnErr(ec.Context, "inference node: materialize partial stream", cerr,
					otellog.String("node.type", "inference"),
					otellog.String("channel", channel))
			}
		}
	}()
	for {
		event, nextErr := stream.Next(ec.Context)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return response, nextErr
		}
		if event.Usage != nil {
			lastUsage = event.Usage.Clone()
			usageSeen = true
		}
		switch delta := event.Delta.(type) {
		case inference.TextPartDelta:
			if delta.Text == "" {
				continue
			}
			if emitErr := s.Emit(delta.Text); emitErr != nil {
				return response, emitErr
			}
		case inference.ReasoningDelta:
			// Reasoning fragments bypass MessageStream, which is
			// text-only: publish each fragment as an incremental
			// reasoning part. Signature/ID ride the terminal fragment.
			part := message.ReasoningPart{
				Text:      delta.Text,
				Signature: delta.Signature,
				ID:        delta.ID,
			}
			if err := part.Validate(); err != nil {
				// ID-only bookkeeping delta with nothing displayable.
				continue
			}
			if emitErr := ec.EmitStreamDelta(agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: part,
			}); emitErr != nil {
				return response, emitErr
			}
		}
	}
	return stream.Result()
}
