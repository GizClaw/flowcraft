package nodes

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/bindings"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"

	"github.com/GizClaw/flowcraft/sdk/message"
	otellog "go.opentelemetry.io/otel/log"
)

// InferenceConfig is the config of the "inference" node type. Board
// references (${board.*}) are resolved per invocation before decode,
// so fields like system_prompt may interpolate upstream output.
type InferenceConfig struct {
	// Model targets a specific model through the wired Runtime. When
	// absent the node defers target selection to the wired Router.
	Model *inference.ModelRef `json:"model,omitempty"`

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

	// Stream opens a GenerateStream and publishes text deltas as
	// token stream events; the board still receives exactly one
	// assembled message (tool_call parts included).
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
	// The remaining knobs ride the text intent verbatim.
	ToolChoice       *inference.ToolChoice     `json:"tool_choice,omitempty"`
	Temperature      *float64                  `json:"temperature,omitempty"`
	TopP             *float64                  `json:"top_p,omitempty"`
	MaxOutputTokens  *int                      `json:"max_output_tokens,omitempty"`
	ReasoningEnabled *bool                     `json:"reasoning_enabled,omitempty"`
	ReasoningEffort  inference.ReasoningEffort `json:"reasoning_effort,omitempty"`

	// Extensions names host-registered provider knobs in the shared
	// {provider, id, fields} wire form (see bindings.DecodeExtensions).
	Extensions []bindings.ExtensionEntry `json:"extensions,omitempty"`
}

// InferenceNodeDeps wires the inference node's collaborators. Runtime
// serves configs carrying an explicit model; Router serves configs
// without one. Either may be nil if no graph needs it — the error
// surfaces at invocation, classified NotAvailable.
type InferenceNodeDeps struct {
	Runtime *inference.Runtime
	Router  *route.Router
	// Catalog resolves config tool names into definitions; required
	// only when a graph configures tools.
	Catalog tool.Catalog
	// Extensions maps "provider/id" to decoders, the same registry
	// shape the script bridge wires with bindings.WithExtensionDecoder.
	Extensions map[string]bindings.ExtensionDecoder
}

// Inference returns the "inference" node type: one Generate call per
// invocation, channel tail in, one assistant message appended. The
// node never executes tool calls — finish_reason == tool_calls is
// flagged onto tool_pending_key and the graph routes onward.
func Inference(deps InferenceNodeDeps) graph.NodeType[InferenceConfig] {
	return graph.NodeType[InferenceConfig]{
		Meta: graph.Meta{
			Desc: "single-shot LLM generation: channel tail in, one assistant message out",
			Reads: []graph.Role{
				{Kind: graph.RoleMessages, ConfigKey: "messages_channel"},
			},
			Writes: []graph.Role{
				{Kind: graph.RoleMessages, ConfigKey: "messages_channel"},
				{Kind: graph.RoleVar, ConfigKey: "output_key"},
				{Kind: graph.RoleVar, ConfigKey: "usage_key"},
				{Kind: graph.RoleVar, ConfigKey: "tool_pending_key"},
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
	req, err := buildGenerateRequest(ec, board, channel, cfg, deps)
	if err != nil {
		return err
	}

	resp, err := executeGenerate(ec, board, cfg, deps, req)
	if err != nil {
		return err
	}
	if len(cfg.Tools) > 0 || cfg.AllTools {
		if catalog := roundCatalog(ec.Context, cfg, deps); catalog != nil {
			if advancer, ok := catalog.(roundAdvancer); ok {
				advancer.AdvanceTurn()
			}
		}
	}

	for _, part := range resp.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return err
		}
		if call, ok := normalized.(message.ToolCallPart); ok {
			if err := ec.EmitStreamDelta(agent.StreamDeltaPayload{
				Type:      agent.StreamDeltaToolCall,
				ID:        call.Call.ID,
				Name:      call.Call.Name,
				Arguments: string(call.Call.Arguments),
			}); err != nil {
				return err
			}
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
	return nil
}

// buildGenerateRequest splits the channel tail into context + current
// input — the exact shape GenerateRequest demands — and attaches the
// configured text intent and extensions.
func buildGenerateRequest(ec graph.ExecutionContext, board *agent.Board, channel string, cfg InferenceConfig, deps InferenceNodeDeps) (inference.GenerateRequest, error) {
	var req inference.GenerateRequest
	messages := board.Channel(channel)
	if len(messages) == 0 {
		return req, errdefs.Validationf("inference node: messages channel %q is empty", channel)
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

	text := &inference.TextIntent{
		ToolChoice:       cfg.ToolChoice,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		MaxOutputTokens:  cfg.MaxOutputTokens,
		ReasoningEnabled: cfg.ReasoningEnabled,
		ReasoningEffort:  cfg.ReasoningEffort,
	}
	if len(cfg.Tools) > 0 || cfg.AllTools {
		definitions, err := toolDefinitions(ec.Context, cfg.Tools, deps.Catalog, cfg.AllTools)
		if err != nil {
			return req, err
		}
		text.Tools = definitions
	}

	extensions, err := bindings.DecodeExtensions(cfg.Extensions, deps.Extensions, "inference node extensions")
	if err != nil {
		return req, err
	}

	return inference.GenerateRequest{
		Context: contextMessages,
		Input: inference.GenerateInput{
			Role: inputRole,
			Content: inference.InputContent{
				Content: last.Content,
				Intent:  inference.Intent{Text: text},
			},
		},
		Extensions: extensions,
	}, nil
}

func toolDefinitions(ctx context.Context, names []string, catalog tool.Catalog, allTools bool) ([]message.Definition, error) {
	if catalog == nil {
		return nil, errdefs.Validationf("inference node: tools configured but no tool catalog wired")
	}
	if allTools {
		if override, ok := tool.CatalogFromContext(ctx); ok {
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
		if override, ok := tool.CatalogFromContext(ctx); ok {
			return override
		}
	}
	return deps.Catalog
}

func resolveExplicitTools(names []string, catalog tool.Catalog) ([]message.Definition, error) {
	available := make(map[string]message.Definition)
	for _, def := range catalog.Definitions() {
		available[def.Name] = def
	}
	definitions := make([]message.Definition, len(names))
	for i, name := range names {
		def, ok := available[name]
		if !ok {
			return nil, errdefs.Validationf("inference node: unknown tool %q", name)
		}
		definitions[i] = def
	}
	return definitions, nil
}

func resolveAllTools(names []string, catalog tool.Catalog) ([]message.Definition, error) {
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
		if deps.Runtime == nil {
			return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: model configured but no runtime wired")
		}
		return deps.Runtime.Generate(ec.Context, *cfg.Model, req)
	}
	if deps.Router == nil {
		return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: no model configured and no router wired")
	}
	resp, _, err := deps.Router.Generate(ec.Context, req)
	return resp, err
}

// executeGenerateStream drains a GenerateStream through a
// MessageStream: each text delta is buffered and published as a token
// event. On success the caller appends the driver-accumulated response
// (complete message, tool_calls included). On a mid-stream failure —
// driver error or run interruption — the buffered partial text is
// committed to the board as one assistant message before the error
// propagates, so downstream consumers and a host-saved board keep the
// progress instead of silently losing every token.
func executeGenerateStream(ec graph.ExecutionContext, board *agent.Board, cfg InferenceConfig, deps InferenceNodeDeps, req inference.GenerateRequest) (inference.GenerateResponse, error) {
	var stream inference.GenerateStream
	var err error
	if cfg.Model != nil {
		if deps.Runtime == nil {
			return inference.GenerateResponse{}, errdefs.NotAvailablef("inference node: model configured but no runtime wired")
		}
		stream, err = deps.Runtime.GenerateStream(ec.Context, *cfg.Model, req)
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
	defer func() {
		if err != nil {
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
		if delta, ok := event.Delta.(inference.TextPartDelta); ok && delta.Text != "" {
			if emitErr := s.Emit(delta.Text); emitErr != nil {
				return response, emitErr
			}
		}
	}
	return stream.Result()
}
