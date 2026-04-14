// Package node provides the Go-native LLM and Knowledge graph nodes.
package node

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// LLMConfig configures an LLM node.
type LLMConfig struct {
	SystemPrompt  string   `json:"system_prompt" yaml:"system_prompt"`
	Model         string   `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature   float64  `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens     int64    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	OutputKey     string   `json:"output_key,omitempty" yaml:"output_key,omitempty"`
	MessagesKey   string   `json:"messages_key,omitempty" yaml:"messages_key,omitempty"`
	JSONMode      bool     `json:"json_mode,omitempty" yaml:"json_mode,omitempty"`
	Thinking      bool     `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	QueryFallback bool     `json:"query_fallback,omitempty" yaml:"query_fallback,omitempty"`
	TrackSteps    bool     `json:"track_steps,omitempty" yaml:"track_steps,omitempty"`
	ToolNames     []string `json:"tool_names,omitempty" yaml:"tool_names,omitempty"`
}

// LLMNode is a Go-native graph node that calls an LLM, handles tool calls,
// and manages message history.
type LLMNode struct {
	id           string
	resolver     llm.LLMResolver
	toolRegistry *tool.Registry
	config       LLMConfig
	rawConfig    map[string]any
}

// NewLLMNode creates a new LLM node.
func NewLLMNode(id string, resolver llm.LLMResolver, toolReg *tool.Registry, config LLMConfig) *LLMNode {
	return &LLMNode{id: id, resolver: resolver, toolRegistry: toolReg, config: config}
}

func (n *LLMNode) ID() string   { return n.id }
func (n *LLMNode) Type() string { return "llm" }

// Config returns the raw config map for variable resolution by the executor.
func (n *LLMNode) Config() map[string]any { return n.rawConfig }

// SetConfig updates the raw config and re-parses the typed LLMConfig so that
// resolved ${board.xxx} variables take effect (e.g. system_prompt).
func (n *LLMNode) SetConfig(c map[string]any) {
	n.rawConfig = c
	n.config = ConfigFromMap(c)
}

func (n *LLMNode) InputPorts() []graph.Port {
	return []graph.Port{
		{Name: workflow.VarMessages, Type: graph.PortTypeMessages, Required: true},
	}
}

func (n *LLMNode) OutputPorts() []graph.Port {
	outputKey := n.config.OutputKey
	if outputKey == "" {
		outputKey = VarResponse
	}
	return []graph.Port{
		{Name: outputKey, Type: graph.PortTypeString, Required: true},
		{Name: workflow.VarMessages, Type: graph.PortTypeMessages, Required: true},
		{Name: VarUsage, Type: graph.PortTypeUsage, Required: true},
		{Name: VarToolPending, Type: graph.PortTypeBool, Required: true},
	}
}

func (n *LLMNode) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	_, span := telemetry.Tracer().Start(ctx.Context, "node.llm.execute",
		trace.WithAttributes(attribute.String("node.id", n.id)))
	defer span.End()

	cfg := n.config
	messagesKey := cfg.MessagesKey
	if messagesKey == "" {
		messagesKey = workflow.VarMessages
	}
	chName := messagesKey
	if chName == workflow.VarMessages {
		chName = workflow.MainChannel
	}

	messages := n.buildMessages(cfg, board, chName, messagesKey)
	if _, ok := board.GetVar(workflow.VarPrevMessageCount); !ok {
		board.SetVar(workflow.VarPrevMessageCount, len(messages))
	}

	opts := n.buildOptions(cfg)

	l, err := n.resolver.Resolve(ctx.Context, cfg.Model)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("node %q: cannot resolve LLM model %q: %w", n.id, cfg.Model, err)
	}

	stream, err := l.GenerateStream(ctx.Context, messages, opts...)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("llm node %s generate failed: %w", n.id, err)
	}

	var fullContent strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		if chunk.Content != "" {
			fullContent.WriteString(chunk.Content)
			if ctx.Stream != nil {
				ctx.Stream(graph.StreamEvent{
					Type:    "token",
					NodeID:  n.id,
					Payload: map[string]any{"content": chunk.Content},
				})
			}
		}
	}
	if err := stream.Err(); err != nil {
		span.RecordError(err)
		return fmt.Errorf("llm node %s stream error: %w", n.id, err)
	}

	rawUsage := stream.Usage()
	usage := llm.TokenUsage{
		InputTokens:  rawUsage.InputTokens,
		OutputTokens: rawUsage.OutputTokens,
		TotalTokens:  rawUsage.InputTokens + rawUsage.OutputTokens,
	}

	accMsg := stream.Message()
	if fullContent.Len() > 0 && len(accMsg.Parts) == 0 {
		accMsg = llm.NewTextMessage(llm.RoleAssistant, fullContent.String())
	}
	if accMsg.Role != "" || len(accMsg.Parts) > 0 {
		messages = append(messages, accMsg)
	}

	toolCalls := accMsg.ToolCalls()
	toolPending, messages := n.handleToolCalls(ctx, toolCalls, messages)

	content := fullContent.String()
	n.writeResults(ctx, board, cfg, content, messages, toolCalls, toolPending, usage, messagesKey, chName)

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", usage.InputTokens),
		attribute.Int64("llm.output_tokens", usage.OutputTokens),
		attribute.Bool("llm.tool_pending", toolPending),
		attribute.Int("llm.tool_calls", len(toolCalls)),
	)
	return nil
}

func (n *LLMNode) buildMessages(cfg LLMConfig, board *graph.Board, chName, messagesKey string) []llm.Message {
	var messages []llm.Message
	if msgs := board.Channel(chName); len(msgs) > 0 {
		messages = msgs
	} else if existing, ok := board.GetVar(messagesKey); ok {
		if msgs, ok := existing.([]llm.Message); ok {
			messages = msgs
		}
	}

	if cfg.SystemPrompt != "" {
		hasSystem := false
		for _, m := range messages {
			if m.Role == llm.RoleSystem {
				hasSystem = true
				break
			}
		}
		if !hasSystem {
			messages = append([]llm.Message{llm.NewTextMessage(llm.RoleSystem, cfg.SystemPrompt)}, messages...)
		}
	}

	if si, ok := board.GetVar(workflow.VarSummaryIndex); ok {
		if index, ok := si.(string); ok && index != "" {
			for i, m := range messages {
				if m.Role == llm.RoleSystem {
					messages[i] = llm.NewTextMessage(llm.RoleSystem, m.Content()+"\n\n"+index)
					break
				}
			}
		}
	}

	if cfg.QueryFallback && messagesKey != workflow.VarMessages {
		if query, ok := board.GetVar(workflow.VarQuery); ok {
			if qs, ok := query.(string); ok && qs != "" {
				needAppend := true
				for i := len(messages) - 1; i >= 0; i-- {
					if messages[i].Role == llm.RoleUser {
						if messages[i].Content() == qs {
							needAppend = false
						}
						break
					}
				}
				if needAppend {
					messages = append(messages, llm.NewTextMessage(llm.RoleUser, qs))
				}
			}
		}
	}
	return messages
}

func (n *LLMNode) buildOptions(cfg LLMConfig) []llm.GenerateOption {
	var opts []llm.GenerateOption
	if cfg.Temperature > 0 {
		opts = append(opts, llm.WithTemperature(cfg.Temperature))
	}
	if cfg.MaxTokens > 0 {
		opts = append(opts, llm.WithMaxTokens(cfg.MaxTokens))
	}
	if cfg.Thinking {
		opts = append(opts, llm.WithThinking(true))
	}
	if cfg.JSONMode {
		opts = append(opts, llm.WithJSONMode(true))
	}

	var toolDefs []llm.ToolDefinition
	if n.toolRegistry != nil && len(cfg.ToolNames) > 0 {
		allowed := make(map[string]bool, len(cfg.ToolNames))
		for _, name := range cfg.ToolNames {
			allowed[name] = true
		}
		for _, def := range n.toolRegistry.Definitions() {
			if allowed[def.Name] {
				toolDefs = append(toolDefs, def)
			}
		}
	}
	if len(toolDefs) > 0 {
		opts = append(opts, llm.WithTools(toolDefs...))
	}
	return opts
}

func (n *LLMNode) handleToolCalls(ctx graph.ExecutionContext, toolCalls []llm.ToolCall, messages []llm.Message) (bool, []llm.Message) {
	if len(toolCalls) == 0 || n.toolRegistry == nil {
		return false, messages
	}

	if ctx.Stream != nil {
		for _, tc := range toolCalls {
			ctx.Stream(graph.StreamEvent{
				Type:    "tool_call",
				NodeID:  n.id,
				Payload: map[string]any{"id": tc.ID, "name": tc.Name, "arguments": tc.Arguments},
			})
		}
	}

	results := n.toolRegistry.ExecuteAll(ctx.Context, toolCalls)

	if ctx.Stream != nil {
		tcNames := make(map[string]string, len(toolCalls))
		for _, tc := range toolCalls {
			tcNames[tc.ID] = tc.Name
		}
		for _, r := range results {
			ctx.Stream(graph.StreamEvent{
				Type:   "tool_result",
				NodeID: n.id,
				Payload: map[string]any{
					"tool_call_id": r.ToolCallID,
					"name":         tcNames[r.ToolCallID],
					"content":      r.Content,
					"is_error":     r.IsError,
				},
			})
		}
	}

	return true, append(messages, llm.NewToolResultMessage(results))
}

func (n *LLMNode) writeResults(
	ctx graph.ExecutionContext, board *graph.Board, cfg LLMConfig,
	content string, messages []llm.Message, toolCalls []llm.ToolCall,
	toolPending bool, usage llm.TokenUsage, messagesKey, chName string,
) {
	if cfg.TrackSteps {
		steps, _ := board.GetVar("agent_steps")
		stepList, _ := steps.([]map[string]any)
		step := map[string]any{
			"response":   content,
			"tool_calls": toolCalls,
			"usage":      usage,
		}
		board.SetVar("agent_steps", append(stepList, step))
	}

	outputKey := cfg.OutputKey
	if outputKey == "" {
		outputKey = VarResponse
	}
	if cfg.JSONMode {
		extracted, _, extractErr := llm.ExtractJSON(content)
		if extractErr != nil {
			telemetry.Warn(ctx.Context, "llm json_mode: extract failed, keeping existing board value",
				otellog.String("node_id", n.id),
				otellog.String("raw", truncate(content, 200)),
				otellog.String("error", extractErr.Error()))
		} else {
			var parsed any
			if err := json.Unmarshal(extracted, &parsed); err == nil {
				switch parsed.(type) {
				case map[string]any, []any:
					board.SetVar(outputKey, parsed)
				default:
					telemetry.Warn(ctx.Context, "llm json_mode: parsed to scalar, keeping existing board value",
						otellog.String("node_id", n.id),
						otellog.String("raw", truncate(content, 200)))
				}
			} else {
				telemetry.Warn(ctx.Context, "llm json_mode: parse failed, keeping existing board value",
					otellog.String("node_id", n.id),
					otellog.String("raw", truncate(content, 200)),
					otellog.String("error", err.Error()))
			}
		}
	} else {
		board.SetVar(outputKey, content)
	}

	board.SetVar(messagesKey, messages)
	board.SetChannel(chName, messages)
	board.SetVar(VarToolPending, toolPending)

	if existingUsage, ok := board.GetVar(workflow.VarInternalUsage); ok {
		if u, ok := existingUsage.(llm.TokenUsage); ok {
			usage = usage.Add(u)
		}
	}
	board.SetVar(workflow.VarInternalUsage, usage)
	board.SetVar(VarUsage, usage)
}

// ConfigFromMap parses an LLMConfig from a generic map via JSON round-trip.
// The LLMConfig struct tags drive the mapping, so adding new fields only
// requires updating the struct — no manual parsing needed.
func ConfigFromMap(m map[string]any) LLMConfig {
	var cfg LLMConfig
	data, err := json.Marshal(m)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
