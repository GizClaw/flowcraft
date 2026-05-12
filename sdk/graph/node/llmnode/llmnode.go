package llmnode

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
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
//   - Graph-level board I/O: SystemPrompt, OutputKey, MessagesChannel,
//     TrackSteps — consumed by Node.ExecuteBoard around the round boundary.
//   - Pure LLM call parameters: Model, Temperature, MaxTokens, JSONMode,
//     Thinking, ToolNames — forwarded into the in-package round driver
//     via Config.generateOptions.
//
// The split exists to keep the round driver (round.go) ignorant of the
// graph board, which is essential for testing it in isolation.
//
// MessagesChannel selects the typed-message channel the node reads from
// and writes to. The empty string (zero value) means [graph.MainChannel],
// shared by every LLM node in the graph. Any other name produces an
// "isolated" channel; an upstream node (or the caller) is responsible
// for seeding it with at least one message before the node runs.
type Config struct {
	SystemPrompt    string   `json:"system_prompt" yaml:"system_prompt"`
	Model           string   `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens       int64    `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	OutputKey       string   `json:"output_key,omitempty" yaml:"output_key,omitempty"`
	MessagesChannel string   `json:"messages_channel,omitempty" yaml:"messages_channel,omitempty"`
	JSONMode        bool     `json:"json_mode,omitempty" yaml:"json_mode,omitempty"`
	Thinking        bool     `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	TrackSteps      bool     `json:"track_steps,omitempty" yaml:"track_steps,omitempty"`
	ToolNames       []string `json:"tool_names,omitempty" yaml:"tool_names,omitempty"`
}

// Node is a Go-native graph node that calls an LLM, dispatches tool calls,
// and manages message history on the board.
type Node struct {
	id       string
	resolver llm.LLMResolver

	// toolRegistry is the legacy "builder closure" tool registry,
	// retained as a fallback only. The runtime now prefers the
	// engine.Run-scoped *tool.Registry exposed via
	// graph.ExecutionContext.Deps[depname.ToolRegistry] (populated
	// by agent.Run from agent.WithDependencies). Closure-binding
	// here remains supported for callers that construct llmnode
	// without an upstream agent.Run wiring deps (vessel inline
	// engine, hand-built test graphs).
	toolRegistry *tool.Registry

	config     Config
	rawConfig  map[string]any
	isDeferred func(string) bool
}

// New creates a Node.
//
// toolReg is optional: pass nil when the surrounding engine.Run is
// expected to supply the registry via [engine.Dependencies] under the
// [depname.ToolRegistry] key. Pass a non-nil registry to keep the
// legacy "builder closure" behaviour for callers driving the node
// outside agent.Run (e.g. the vessel inline engine, unit tests).
// At runtime the run-scoped registry wins when both are present.
func New(id string, resolver llm.LLMResolver, toolReg *tool.Registry, config Config) *Node {
	return &Node{id: id, resolver: resolver, toolRegistry: toolReg, config: config}
}

// resolveTools returns the tool registry and effective allow-list for
// one round, applying contract-audit Epic A's policy:
//
//  1. Registry: graph.ExecutionContext.Deps[depname.ToolRegistry]
//     wins when present and well-typed; falls back to the
//     constructor-bound n.toolRegistry. This lets agent.Run swap a
//     run-scoped registry in without touching the graph builder.
//  2. Allow list: when ec.Deps carries [depname.ToolAllowedNames]
//     it acts as the run-level CEILING. agent.Run populates that
//     key from agent.Agent.Tools, so this is where the historically
//     ignored policy gate finally takes effect.
//     - ceiling absent → fall back to Config.ToolNames verbatim
//     (preserves legacy behaviour for engines that don't wire
//     deps).
//     - ceiling present and empty → no tools permitted (fail
//     closed).
//     - ceiling present and non-empty → INTERSECT with
//     Config.ToolNames so the node still controls which subset
//     to expose to the LLM for this round.
//
// The returned slice is a fresh allocation; callers may pass it into
// downstream Config copies without aliasing concerns.
func (n *Node) resolveTools(ec graph.ExecutionContext) (*tool.Registry, []string) {
	reg := n.toolRegistry
	if r, err := engine.GetDep[*tool.Registry](ec.Deps, depname.ToolRegistry); err == nil && r != nil {
		reg = r
	}

	requested := n.config.ToolNames
	if ec.Deps == nil {
		return reg, requested
	}
	ceiling, err := engine.GetDep[[]string](ec.Deps, depname.ToolAllowedNames)
	if err != nil {
		return reg, requested
	}
	return reg, intersectAllow(requested, ceiling)
}

// intersectAllow returns the names present in both requested and
// ceiling. Either input being empty means "deny all" — empty
// requested is the node opting out, empty ceiling is the run-level
// gate closing everything.
func intersectAllow(requested, ceiling []string) []string {
	if len(requested) == 0 || len(ceiling) == 0 {
		return nil
	}
	cset := make(map[string]struct{}, len(ceiling))
	for _, name := range ceiling {
		cset[name] = struct{}{}
	}
	out := make([]string, 0, len(requested))
	for _, name := range requested {
		if _, ok := cset[name]; ok {
			out = append(out, name)
		}
	}
	return out
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
		{Name: n.channelName(), Type: graph.PortTypeMessages, Required: true},
	}
}

func (n *Node) OutputPorts() []graph.Port {
	outputKey := n.config.OutputKey
	if outputKey == "" {
		outputKey = VarResponse
	}
	return []graph.Port{
		{Name: outputKey, Type: graph.PortTypeString, Required: true},
		// Messages live on a typed channel (board.SetChannel), not on
		// board vars. ValidateOutputs only inspects board vars unless
		// the port is PortTypeMessages, so this port is intentionally
		// not Required: doing otherwise short-circuits the executor
		// after every successful llm round.
		{Name: n.channelName(), Type: graph.PortTypeMessages, Required: false},
		{Name: VarUsage, Type: graph.PortTypeUsage, Required: true},
		{Name: VarToolPending, Type: graph.PortTypeBool, Required: true},
	}
}

// channelName returns the typed-message channel this node binds to.
// Empty MessagesChannel maps to [graph.MainChannel] (shared transcript);
// any other value names an isolated per-node channel.
func (n *Node) channelName() string {
	if n.config.MessagesChannel == "" {
		return graph.MainChannel
	}
	return n.config.MessagesChannel
}

func (n *Node) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	_, span := telemetry.Tracer().Start(ctx.Context, "node.llm.execute",
		trace.WithAttributes(attribute.String(telemetry.AttrNodeID, n.id)))
	defer span.End()

	cfg := n.config
	chName := n.channelName()

	messages := n.buildMessages(cfg, board, chName)
	if len(messages) == 0 {
		// Empty channel + no system prompt → nothing to send. This is
		// always a graph wiring mistake (no upstream node populated the
		// channel and the operator did not configure a system prompt);
		// fail fast with a clear message rather than emitting a
		// provider-dependent error (Anthropic 400, OpenAI silent
		// behaviour, …) that varies across the LLM stack.
		return errdefs.Validationf(
			"llm node %q has nothing to send: messages_channel %q is empty and system_prompt is unset",
			n.id, chName)
	}
	if _, ok := board.GetVar(VarPrevMessageCount); !ok {
		board.SetVar(VarPrevMessageCount, len(messages))
	}

	// Resolve registry + allow-list at runtime so agent.Run-supplied
	// dependencies (and Agent.Tools as the policy gate) actually
	// reach the LLM call. cfg is a value-copied Config above; we
	// overwrite ToolNames on the local copy without aliasing the
	// node-level field.
	reg, allowedNames := n.resolveTools(ctx)
	cfg.ToolNames = allowedNames

	result, err := runRound(
		ctx.Context, ctx.Host, ctx.Publisher,
		n.resolver, reg,
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
	if werr := n.writeResults(ctx, board, cfg, result, chName); werr != nil {
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

func (n *Node) buildMessages(cfg Config, board *graph.Board, chName string) []llm.Message {
	// Defensive copy: we mutate the slice below (system-prompt prepend,
	// summary-index injection) and must not write back into board state.
	messages := append([]llm.Message(nil), board.Channel(chName)...)

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

	return messages
}

func (n *Node) writeResults(
	ctx graph.ExecutionContext, board *graph.Board, cfg Config,
	result *roundResult, chName string,
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
