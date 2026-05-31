package claw

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// AgentConfig configures the single agent owned by a Claw.
type AgentConfig struct {
	ID            string   `json:"id,omitempty" yaml:"id,omitempty"`
	Name          string   `json:"name,omitempty" yaml:"name,omitempty"`
	Description   string   `json:"description,omitempty" yaml:"description,omitempty"`
	SystemPrompt  string   `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	Tools         []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty" yaml:"max_iterations,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
}

func (c *Claw) buildAgent() agent.Agent {
	return agent.Agent{
		ID:    c.cfg.Agent.ID,
		Tools: append([]string(nil), c.cfg.Agent.Tools...),
		Card: agent.AgentCard{
			Name:        c.cfg.Agent.Name,
			Description: c.cfg.Agent.Description,
			Capabilities: agent.AgentCapabilities{
				Streaming: true,
			},
		},
	}
}

func (c *Claw) buildEngine(client llm.LLM) engine.Engine {
	return engine.EngineFunc(func(ctx context.Context, run engine.Run, host engine.Host, board *engine.Board) (*engine.Board, error) {
		agentID := c.cfg.Agent.ID
		step := agentID + ".iter0"
		publishLifecycle(ctx, host, engine.SubjectRunStart(run.ID), run.ID, agentID, map[string]any{"agent_id": agentID})
		var runErr error
		defer func() {
			payload := map[string]any{"status": "success"}
			if runErr != nil {
				payload["status"] = "error"
				payload["error"] = runErr.Error()
			}
			publishLifecycle(ctx, host, engine.SubjectRunEnd(run.ID), run.ID, agentID, payload)
		}()

		msgs := append([]model.Message(nil), board.Channel(engine.MainChannel)...)
		userText := latestUserText(msgs)
		if c.memory != nil {
			memText, err := c.memory.recallContext(ctx, userText)
			if err != nil {
				runErr = fmt.Errorf("claw: recall memory: %w", err)
				return board, runErr
			}
			if memText != "" {
				msgs = append([]model.Message{model.NewTextMessage(model.RoleSystem, memText)}, msgs...)
			}
		}
		if c.cfg.Agent.SystemPrompt != "" {
			msgs = append([]model.Message{model.NewTextMessage(model.RoleSystem, c.cfg.Agent.SystemPrompt)}, msgs...)
		}

		opts := []llm.GenerateOption{}
		if c.cfg.Agent.Temperature != nil {
			opts = append(opts, llm.WithTemperature(*c.cfg.Agent.Temperature))
		}
		reply, err := streamLLMRound(ctx, host, run.ID, step, client, msgs, opts)
		if err != nil {
			runErr = fmt.Errorf("claw: generate: %w", err)
			return board, runErr
		}
		board.AppendChannelMessage(engine.MainChannel, reply)
		if c.memory != nil {
			if err := c.memory.saveTurn(ctx, run.Attributes["context_id"], userText, reply); err != nil {
				runErr = fmt.Errorf("claw: save memory: %w", err)
				return board, runErr
			}
		}
		return board, nil
	})
}

func latestUserText(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleUser {
			return msgs[i].Content()
		}
	}
	return ""
}

func publishLifecycle(ctx context.Context, pub engine.Publisher, subject event.Subject, runID, agentID string, payload any) {
	if pub == nil {
		return
	}
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return
	}
	if runID != "" {
		env.SetRunID(runID)
	}
	if agentID != "" {
		env.SetAgentID(agentID)
	}
	_ = pub.Publish(ctx, env)
}

func streamLLMRound(
	ctx context.Context,
	host engine.Host,
	runID, stepActor string,
	client llm.LLM,
	msgs []model.Message,
	opts []llm.GenerateOption,
) (model.Message, error) {
	stream, err := client.GenerateStream(ctx, msgs, opts...)
	if err != nil || stream == nil {
		reply, _, gerr := client.Generate(ctx, msgs, opts...)
		if gerr != nil {
			return reply, gerr
		}
		_ = engine.EmitStreamToken(ctx, host, runID, stepActor, reply.Content())
		return reply, nil
	}
	defer func() { _ = stream.Close() }()

	for stream.Next() {
		chunk := stream.Current()
		if chunk.Content != "" {
			_ = engine.EmitStreamToken(ctx, host, runID, stepActor, chunk.Content)
		}
		for _, tc := range chunk.ToolCalls {
			_ = engine.EmitStreamToolCall(ctx, host, runID, stepActor, tc.ID, tc.Name, tc.Arguments)
		}
	}
	if serr := stream.Err(); serr != nil {
		return stream.Message(), serr
	}
	usage := stream.Usage()
	_ = host.ReportUsage(ctx, model.TokenUsage{
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		OutputTokens:      usage.OutputTokens,
		TotalTokens:       usage.InputTokens + usage.OutputTokens,
	})
	return stream.Message(), nil
}
