package vesselquality

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

// fakeLLMEngineFactory mirrors cmd/vesseld/catalog/engine_graph_llm.go
// in shape: it loops calling client.Generate, executes any tool
// calls via deps.ToolRegistry, and stops when the model returns
// plain text or maxIter is hit. We re-implement it here (instead
// of importing the catalog package) because (a) the catalog has
// vesseld-specific deps wiring we don't need and (b) we want
// the test loop's behaviour pinned to this module so a refactor
// of the catalog cannot silently change quality semantics.
//
// Each fake agent gets its OWN fakellm.LLM so multi-agent tests
// can script different replies per agent.
func fakeLLMEngineFactory(perAgent map[string]*fakellm.LLM, maxIter int) vessel.EngineFactory {
	if maxIter <= 0 {
		maxIter = 8
	}
	return func(aspec spec.Agent, deps vessel.Deps) (engine.Engine, error) {
		fake := perAgent[aspec.Name]
		if fake == nil {
			return nil, fmt.Errorf("test: no fakellm wired for agent %q", aspec.Name)
		}
		return engine.EngineFunc(func(ctx context.Context, run engine.Run, _ engine.Host, board *engine.Board) (*engine.Board, error) {
			msgs := append([]model.Message(nil), board.Channel(engine.MainChannel)...)
			toolDefs := definitionsFor(deps.ToolRegistry)
			for iter := 0; iter < maxIter; iter++ {
				opts := []llm.GenerateOption{}
				if len(toolDefs) > 0 {
					opts = append(opts, llm.WithTools(toolDefs...))
				}
				reply, _, err := fake.Generate(ctx, msgs, opts...)
				if err != nil {
					return board, err
				}
				msgs = append(msgs, reply)
				board.AppendChannelMessage(engine.MainChannel, reply)
				if !reply.HasToolCalls() {
					return board, nil
				}
				results := make([]model.ToolResult, 0, len(reply.ToolCalls()))
				for _, call := range reply.ToolCalls() {
					out, terr := executeTool(ctx, deps.ToolRegistry, call)
					results = append(results, model.ToolResult{
						ToolCallID: call.ID,
						Content:    out,
						IsError:    terr != nil,
					})
				}
				toolMsg := model.NewToolResultMessage(results)
				msgs = append(msgs, toolMsg)
				board.AppendChannelMessage(engine.MainChannel, toolMsg)
			}
			return board, errdefs.Conflictf("test engine: max iterations (%d) reached", maxIter)
		}), nil
	}
}

func definitionsFor(r *tool.Registry) []llm.ToolDefinition {
	if r == nil {
		return nil
	}
	return r.Definitions()
}

func executeTool(ctx context.Context, r *tool.Registry, call model.ToolCall) (string, error) {
	if r == nil {
		return fmt.Sprintf("no tool registry; tool %q skipped", call.Name), errdefs.NotAvailablef("no tool registry")
	}
	tl, ok := r.Get(call.Name)
	if !ok {
		return fmt.Sprintf("tool %q not registered", call.Name), errdefs.NotFoundf("tool %q", call.Name)
	}
	out, err := tl.Execute(ctx, call.Arguments)
	if err != nil {
		return fmt.Sprintf("tool %q error: %v", call.Name, err), err
	}
	return out, nil
}

// launchedCaptain is a small helper that wraps New + Launch +
// t.Cleanup for the common "build, start, run one Call" path.
func launchedCaptain(t *testing.T, vs spec.Spec, opts ...vessel.Option) *vessel.Captain {
	t.Helper()
	c, err := vessel.New(vs, opts...)
	if err != nil {
		t.Fatalf("vessel.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("vessel.Launch: %v", err)
	}
	return c
}
