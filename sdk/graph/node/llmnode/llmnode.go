package llmnode

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// Config configures an LLM graph node. Fields fall into two groups:
//
//   - Graph-level board I/O: SystemPrompt, OutputKey, MessagesKey,
//     QueryFallback, TrackSteps — consumed by Node.ExecuteBoard around
//     the round boundary.
//   - Pure LLM call parameters: Model, Temperature, MaxTokens, JSONMode,
//     Thinking, ToolNames — forwarded into the in-package round driver
//     via Config.generateOptions.
//
// The split exists to keep the round driver (round.go) ignorant of the
// graph board, which is essential for testing it in isolation.
type Config struct {
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

// Node is a Go-native graph node that calls an LLM, dispatches tool calls,
// and manages message history on the board.
type Node struct {
	id           string
	resolver     llm.LLMResolver
	toolRegistry *tool.Registry
	config       Config
	rawConfig    map[string]any
	isDeferred   func(string) bool
}

// New creates a Node.
func New(id string, resolver llm.LLMResolver, toolReg *tool.Registry, config Config) *Node {
	return &Node{id: id, resolver: resolver, toolRegistry: toolReg, config: config}
}

func (n *Node) ID() string             { return n.id }
func (n *Node) Type() string           { return "llm" }
func (n *Node) Config() map[string]any { return n.rawConfig }

// SetConfig updates the raw config and re-parses the typed Config so that
// resolved ${board.xxx} variables take effect (e.g. system_prompt).
func (n *Node) SetConfig(c map[string]any) {
	n.rawConfig = c
	cfg, err := ConfigFromMap(c, n.isDeferred)
	if err != nil {
		telemetry.Warn(context.Background(), "llm node: invalid config map",
			otellog.String("node_id", n.id),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return
	}
	n.config = cfg
}

func (n *Node) InputPorts() []graph.Port {
	return []graph.Port{
		{Name: graph.VarMessages, Type: graph.PortTypeMessages, Required: true},
	}
}

func (n *Node) OutputPorts() []graph.Port {
	outputKey := n.config.OutputKey
	if outputKey == "" {
		outputKey = VarResponse
	}
	return []graph.Port{
		{Name: outputKey, Type: graph.PortTypeString, Required: true},
		{Name: graph.VarMessages, Type: graph.PortTypeMessages, Required: true},
		{Name: VarUsage, Type: graph.PortTypeUsage, Required: true},
		{Name: VarToolPending, Type: graph.PortTypeBool, Required: true},
	}
}

func (n *Node) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	_, span := telemetry.Tracer().Start(ctx.Context, "node.llm.execute",
		trace.WithAttributes(attribute.String(telemetry.AttrNodeID, n.id)))
	defer span.End()

	cfg := n.config
	messagesKey := cfg.MessagesKey
	if messagesKey == "" {
		messagesKey = graph.VarMessages
	}
	chName := messagesKey
	if chName == graph.VarMessages {
		chName = graph.MainChannel
	}

	messages := n.buildMessages(cfg, board, chName, messagesKey)
	if _, ok := board.GetVar(VarPrevMessageCount); !ok {
		board.SetVar(VarPrevMessageCount, len(messages))
	}

	result, err := runRound(
		ctx.Context, ctx.Host, ctx.Publisher,
		n.resolver, n.toolRegistry,
		n.id, messages, cfg,
	)
	if err != nil {
		span.RecordError(err)
		return err
	}

	// Always commit the round, even when interrupted: writeResults is
	// what materialises the partial assistant message + cancelled
	// tool_results onto the board so the agent layer / memory writer
	// can read them after the resume / discard decision.
	if werr := n.writeResults(ctx, board, cfg, result, messagesKey, chName); werr != nil {
		// writeResults only ever surfaces errors from the host's
		// budget / quota gate (UsageReporter). Propagate so the
		// run terminates rather than silently exceeding the budget,
		// but only after the partial state has been written above.
		span.RecordError(werr)
		return werr
	}

	span.SetAttributes(
		attribute.Int64("llm.input_tokens", result.Usage.InputTokens),
		attribute.Int64("llm.output_tokens", result.Usage.OutputTokens),
		attribute.Bool("llm.tool_pending", result.ToolPending),
		attribute.Int("llm.tool_calls", len(result.ToolCalls)),
		attribute.Bool("llm.interrupted", result.Interrupted),
	)

	if result.Interrupted {
		// Return engine.Interrupted so the executor classifies via
		// errdefs.IsInterrupted, writes graph.VarInterruptedNode, and
		// hands the cause/detail back to the host. ctx-cancel-only
		// interrupts (no host signal) surface as CauseUnknown.
		return engine.Interrupted(engine.Interrupt{
			Cause:  result.InterruptCause,
			Detail: result.InterruptDetail,
		})
	}
	return nil
}

func (n *Node) buildMessages(cfg Config, board *graph.Board, chName, messagesKey string) []llm.Message {
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

	if si, ok := board.GetVar(VarSummaryIndex); ok {
		if index, ok := si.(string); ok && index != "" {
			for i, m := range messages {
				if m.Role == llm.RoleSystem {
					messages[i] = llm.NewTextMessage(llm.RoleSystem, m.Content()+"\n\n"+index)
					break
				}
			}
		}
	}

	if cfg.QueryFallback && messagesKey != graph.VarMessages {
		if query, ok := board.GetVar(graph.VarQuery); ok {
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

func (n *Node) writeResults(
	ctx graph.ExecutionContext, board *graph.Board, cfg Config,
	result *roundResult, messagesKey, chName string,
) error {
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
				otellog.String(telemetry.AttrNodeID, n.id),
				otellog.String("raw", truncate(result.Content, 200)),
				otellog.String(telemetry.AttrErrorMessage, extractErr.Error()))
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
					otellog.String(telemetry.AttrErrorMessage, err.Error()))
			}
		}
	} else {
		board.SetVar(outputKey, result.Content)
	}

	board.SetVar(messagesKey, result.Messages)
	board.SetChannel(chName, result.Messages)
	board.SetVar(VarToolPending, result.ToolPending)

	usage := result.Usage
	if existingUsage, ok := board.GetVar(VarInternalUsage); ok {
		if u, ok := existingUsage.(model.TokenUsage); ok {
			usage = usage.Add(u)
		}
	}
	board.SetVar(VarInternalUsage, usage)
	board.SetVar(VarUsage, usage)

	// Report this round's delta to the host so token-budget /
	// quota / billing observers see traffic without having to
	// subscribe to stream envelopes. The host contract is "each
	// call adds delta usage" so we hand off result.Usage, NOT the
	// accumulated total. Interrupt path also flows through here so
	// partial usage is still attributed.
	//
	// An error from ReportUsage is only meaningful when it carries
	// the BudgetExceeded classification (sandbox / pod budget); per
	// the engine.UsageReporter contract any other error is
	// observability-only and SHOULD be swallowed so a flaky exporter
	// cannot kill the run.
	if ctx.Host != nil && (result.Usage.InputTokens > 0 ||
		result.Usage.OutputTokens > 0 ||
		result.Usage.TotalTokens > 0) {
		if err := ctx.Host.ReportUsage(ctx.Context, result.Usage); err != nil {
			if errdefs.IsBudgetExceeded(err) {
				return err
			}
			telemetry.Warn(ctx.Context, "llm: ReportUsage returned a non-budget error; ignoring",
				otellog.String(telemetry.AttrNodeID, n.id),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
