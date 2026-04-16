// Package node provides the Go-native LLM and Knowledge graph nodes.
package node

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// LLMConfig configures an LLM graph node.
// Fields like SystemPrompt, OutputKey, MessagesKey, QueryFallback, and TrackSteps
// are graph-level board I/O concerns; the pure LLM call parameters are forwarded
// to llm.RoundConfig via the roundConfig() method.
type LLMConfig struct {
	SystemPrompt  string   `json:"system_prompt" yaml:"system_prompt"`
	Model         string   `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens     int64    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	OutputKey     string   `json:"output_key,omitempty" yaml:"output_key,omitempty"`
	MessagesKey   string   `json:"messages_key,omitempty" yaml:"messages_key,omitempty"`
	JSONMode      bool     `json:"json_mode,omitempty" yaml:"json_mode,omitempty"`
	Thinking      bool     `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	QueryFallback bool     `json:"query_fallback,omitempty" yaml:"query_fallback,omitempty"`
	TrackSteps    bool     `json:"track_steps,omitempty" yaml:"track_steps,omitempty"`
	ToolNames     []string `json:"tool_names,omitempty" yaml:"tool_names,omitempty"`
}

func (c LLMConfig) roundConfig() llm.RoundConfig {
	return llm.RoundConfig{
		Model:       c.Model,
		Temperature: c.Temperature,
		MaxTokens:   c.MaxTokens,
		JSONMode:    c.JSONMode,
		Thinking:    c.Thinking,
		ToolNames:   c.ToolNames,
	}
}

// LLMNode is a Go-native graph node that calls an LLM, handles tool calls,
// and manages message history.
type LLMNode struct {
	id           string
	resolver     llm.LLMResolver
	toolRegistry *tool.Registry
	config       LLMConfig
	rawConfig    map[string]any
	isDeferred   func(string) bool
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
	cfg, err := ConfigFromMap(c, n.isDeferred)
	if err != nil {
		telemetry.Warn(context.Background(), "llm node: invalid config map",
			otellog.String("node_id", n.id),
			otellog.String("error", err.Error()))
		return
	}
	n.config = cfg
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

	result, err := llm.RunRound(
		ctx.Context, ctx.Stream,
		n.resolver, n.toolRegistry,
		n.id, messages, cfg.roundConfig(),
	)
	if err != nil {
		span.RecordError(err)
		return err
	}

	n.writeResults(ctx, board, cfg, result, messagesKey, chName)

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int64("llm.output_tokens", result.Usage.OutputTokens),
		attribute.Bool("llm.tool_pending", result.ToolPending),
		attribute.Int("llm.tool_calls", len(result.ToolCalls)),
	)
	return nil
}

func (n *LLMNode) buildMessages(cfg LLMConfig, board *graph.Board, chName, messagesKey string) []llm.Message {
	var messages []llm.Message
	if msgs := board.Channel(chName); len(msgs) > 0 {
		messages = msgs
	} else if existing, ok := board.GetVar(messagesKey); ok {
		if msgs, ok := existing.([]llm.Message); ok {
			messages = append([]llm.Message(nil), msgs...)
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

func (n *LLMNode) writeResults(
	ctx graph.ExecutionContext, board *graph.Board, cfg LLMConfig,
	result *llm.RoundResult, messagesKey, chName string,
) {
	if cfg.TrackSteps {
		steps, _ := board.GetVar("agent_steps")
		stepList, _ := steps.([]map[string]any)
		step := map[string]any{
			"response":   result.Content,
			"tool_calls": result.ToolCalls,
			"usage":      result.Usage,
		}
		board.SetVar("agent_steps", append(stepList, step))
	}

	outputKey := cfg.OutputKey
	if outputKey == "" {
		outputKey = VarResponse
	}
	if cfg.JSONMode {
		extracted, _, extractErr := llm.ExtractJSON(result.Content)
		if extractErr != nil {
			telemetry.Warn(ctx.Context, "llm json_mode: extract failed, keeping existing board value",
				otellog.String("node_id", n.id),
				otellog.String("raw", truncate(result.Content, 200)),
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
						otellog.String("raw", truncate(result.Content, 200)))
				}
			} else {
				telemetry.Warn(ctx.Context, "llm json_mode: parse failed, keeping existing board value",
					otellog.String("node_id", n.id),
					otellog.String("raw", truncate(result.Content, 200)),
					otellog.String("error", err.Error()))
			}
		}
	} else {
		board.SetVar(outputKey, result.Content)
	}

	board.SetVar(messagesKey, result.Messages)
	board.SetChannel(chName, result.Messages)
	board.SetVar(VarToolPending, result.ToolPending)

	usage := result.Usage
	if existingUsage, ok := board.GetVar(workflow.VarInternalUsage); ok {
		if u, ok := existingUsage.(model.TokenUsage); ok {
			usage = usage.Add(u)
		}
	}
	board.SetVar(workflow.VarInternalUsage, usage)
	board.SetVar(VarUsage, usage)
}

// ConfigFromMap parses an LLMConfig from a generic map via JSON round-trip.
// isDeferred is passed through to CoerceMapForStruct; see its documentation.
func ConfigFromMap(m map[string]any, isDeferred func(string) bool) (LLMConfig, error) {
	var cfg LLMConfig
	if m == nil {
		return cfg, nil
	}
	m = llm.CoerceMapForStruct[LLMConfig](m, isDeferred)
	data, err := json.Marshal(m)
	if err != nil {
		return cfg, fmt.Errorf("node: marshal config map: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("node: unmarshal config: %w", err)
	}
	return cfg, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
