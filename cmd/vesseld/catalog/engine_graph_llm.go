package catalog

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// publishLifecycle is the lifecycle-event analogue of
// engine.EmitStreamDelta — the engine package only ships emit
// helpers for stream-delta envelopes, so the inline executor below
// hand-rolls run-level subjects. Centralising the construction
// keeps run.start and run.end identical in shape so consumers
// (vessel.Logs, /v1/vessels/{id}/logs) can treat them as a pair.
func publishLifecycle(ctx context.Context, pub engine.Publisher, subject event.Subject, runID string, payload any) {
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
	_ = pub.Publish(ctx, env)
}

// graphLLMEngineFactory builds the v0.1.0 default engine: a small
// in-line agent loop that calls one llm.LLM, executes any tool
// calls the model requests via the daemon-shared tool registry,
// and loops until the model returns plain text or maxIterations is
// reached.
//
// Why an in-line loop instead of wiring sdk/graph runner: graph
// runner is the production execution engine but it requires
// callers to hand-build a GraphDefinition + node factory wiring,
// which is exactly the friction vesseld is trying to remove. For
// v0.1.0 the in-line loop covers the common "single LLM with
// tools" shape; users wanting structured DAGs supply their own
// engine factory through a custom build of vesseld (catalog
// re-registration in main.go) or, in the meantime, drive vessel
// directly via vessel.New + WithEngineFactory.
//
// Config keys recognised:
//
//	llmProfile       string   required; LLMProfile name to resolve
//	systemPrompt     string   optional; prepended as a system message
//	maxIterations    int      optional; default 8 — loop bound
//	temperature      float64  optional; passed via llm.WithTemperature
//
// Unknown keys are ignored so future config knobs can roll out
// without rejecting existing configs.
func graphLLMEngineFactory(ref string, cfg map[string]any, deps Deps) (engine.Engine, error) {
	profile, _ := cfg["llmProfile"].(string)
	if profile == "" {
		return nil, formatRefError("vesseld engine", ref, "config.llmProfile is required")
	}
	client := deps.LLMClients[profile]
	if client == nil {
		return nil, formatRefError("vesseld engine", ref, "LLMProfile %q not found in daemon LLMClients map", profile)
	}
	systemPrompt, _ := cfg["systemPrompt"].(string)
	maxIter := intFromAny(cfg["maxIterations"], 8)
	if maxIter <= 0 {
		maxIter = 8
	}

	limiter := deps.LLMLimiters[profile]
	// Allow-list resolution moved INTO the EngineFunc closure
	// (contract-audit Epic D / #11): the inline engine now reads
	// the per-run agent.Agent.Tools snapshot agent.Run promotes
	// into engine.Run.Deps[depname.ToolAllowedNames] and falls
	// back to the legacy factory-time deps.AgentTools for callers
	// that still wire vessel without driving it through agent.Run.
	// See resolveAllowSet for the precedence rules.

	// Wrap the inline executor with the engine's declared
	// capabilities so hosts (agent.Run preflight, dashboards, the
	// vessel build path) can introspect the engine via
	// engine.CapabilitiesOf without ad-hoc type assertions. The
	// claim list deliberately mirrors the inline engine's true
	// behaviour today:
	//
	//   - SupportsResume = false: the inline executor restarts from
	//     scratch every time; ResumeFrom is unused.
	//   - EmitsUserPrompt = false: no host.AskUser call sites.
	//   - EmitsCheckpoint = false: the inline loop never invokes
	//     host.Checkpoint (graph runner is the engine that does, but
	//     this factory does not delegate to it).
	//   - RequiredDepNames = []string{depname.ToolAllowedNames}:
	//     declares the only dep this engine resolves at run time
	//     (the policy gate). LLMClient / ToolRegistry are still
	//     wired through factory-time catalog.Deps for v0.1.0 so
	//     they're not declared here; teaching the inline engine
	//     to resolve those from run.Deps too is a follow-up that
	//     turns this into a fully run-deps-driven engine.
	return engine.WithCapabilities(engine.EngineFunc(func(ctx context.Context, run engine.Run, host engine.Host, board *engine.Board) (*engine.Board, error) {
		allowSet := resolveAllowSet(run, deps)
		// actorID is the engine "actor" key used in step-level
		// subjects. We use the agent name so SSE consumers can
		// see "agent X started step Y" without having to consult
		// a separate registry for an opaque node id. For the
		// engine-contract subjects this maps to the
		// engine.run.<runID>.step.<actorID>.{start,complete,error}
		// shape (see sdk/engine/subjects.go).
		actorID := deps.AgentName

		// Lifecycle: run.started fires exactly once at entry,
		// run.ended fires exactly once on exit (defer), regardless
		// of whether we return cleanly or with an error. This is
		// the engine.Engine contract surfaced via subjects so
		// consumers can rely on the start/end pair existing for
		// every Submit, mirroring sdk/graph/runner's behaviour
		// even though we are an in-line executor.
		publishLifecycle(ctx, host, engine.SubjectRunStart(run.ID), run.ID, map[string]any{
			"vessel": deps.VesselID,
			"agent":  deps.AgentName,
		})
		var runErr error
		defer func() {
			status := "success"
			if runErr != nil {
				status = "error"
			}
			payload := map[string]any{"status": status}
			if runErr != nil {
				payload["error"] = runErr.Error()
			}
			publishLifecycle(ctx, host, engine.SubjectRunEnd(run.ID), run.ID, payload)
		}()

		// Pull the current message stream from MainChannel; the
		// vessel BoardSeeder loaded any historical transcript into
		// it before we ran.
		msgs := append([]model.Message(nil), board.Channel(engine.MainChannel)...)
		if systemPrompt != "" && (len(msgs) == 0 || msgs[0].Role != model.RoleSystem) {
			msgs = append([]model.Message{model.NewTextMessage(model.RoleSystem, systemPrompt)}, msgs...)
		}

		// Build the per-call tool definitions from the daemon-shared
		// registry filtered by the agent's allow-list. Tools NOT in
		// the allow-list are stripped here so the LLM never even
		// hears about them; executeToolCall below provides a second
		// gate at execution time in case a model jailbreak or
		// hand-crafted tool_call slips a name through anyway.
		toolDefs := buildToolDefinitions(deps, allowSet)

		for iter := 0; iter < maxIter; iter++ {
			// Per-iteration step lifecycle. One Generate call =
			// one step in the engine-contract sense. Tool
			// dispatch (when the model returns tool_calls) runs
			// inside the SAME step; that keeps the step.start /
			// step.complete pair anchored to "one model turn"
			// rather than fragmenting across each tool call.
			stepActor := fmt.Sprintf("%s.iter%d", actorID, iter)
			publishLifecycle(ctx, host, engine.SubjectStepStart(run.ID, stepActor), run.ID, map[string]any{
				"actor_id":  stepActor,
				"iteration": iter,
				"kind":      "llm",
			})

			// Honour the daemon-wide LLM rate limit before each
			// Generate call. v0.1.0 only enforces the requests-
			// per-minute axis (tokens=0); a future iteration that
			// pre-counts prompt tokens can pass a non-zero value
			// to also gate on the tokens-per-minute cap.
			if limiter != nil {
				if err := limiter.Acquire(ctx, deps.VesselID, 0); err != nil {
					runErr = fmt.Errorf("graph-llm[%s/%s]: rate limit acquire iter=%d: %w", deps.VesselID, deps.AgentName, iter, err)
					publishLifecycle(ctx, host, engine.SubjectStepError(run.ID, stepActor), run.ID, map[string]any{
						"actor_id": stepActor, "error": runErr.Error(),
					})
					return board, runErr
				}
			}
			opts := []llm.GenerateOption{}
			if len(toolDefs) > 0 {
				opts = append(opts, llm.WithTools(toolDefs...))
			}
			reply, err := streamLLMRound(ctx, host, run.ID, stepActor, client, msgs, opts)
			if err != nil {
				runErr = fmt.Errorf("graph-llm[%s/%s]: generate iter=%d: %w", deps.VesselID, deps.AgentName, iter, err)
				publishLifecycle(ctx, host, engine.SubjectStepError(run.ID, stepActor), run.ID, map[string]any{
					"actor_id": stepActor, "error": runErr.Error(),
				})
				return board, runErr
			}
			msgs = append(msgs, reply)
			board.AppendChannelMessage(engine.MainChannel, reply)

			if !reply.HasToolCalls() {
				publishLifecycle(ctx, host, engine.SubjectStepComplete(run.ID, stepActor), run.ID, map[string]any{
					"actor_id": stepActor, "final": true,
				})
				return board, nil
			}

			// Execute every tool call sequentially. Parallel
			// execution is a v0.2.0 graph-runner feature; v0.1.0
			// keeps the loop simple and serial.
			results := make([]model.ToolResult, 0, len(reply.ToolCalls()))
			for _, call := range reply.ToolCalls() {
				out, terr := executeToolCall(ctx, deps, allowSet, call)
				_ = engine.EmitStreamToolResult(ctx, host, run.ID, stepActor, call.ID, call.Name, out, terr != nil, false)
				results = append(results, model.ToolResult{
					ToolCallID: call.ID,
					Content:    out,
					IsError:    terr != nil,
				})
			}
			toolMsg := model.NewToolResultMessage(results)
			msgs = append(msgs, toolMsg)
			board.AppendChannelMessage(engine.MainChannel, toolMsg)
			publishLifecycle(ctx, host, engine.SubjectStepComplete(run.ID, stepActor), run.ID, map[string]any{
				"actor_id":   stepActor,
				"tool_calls": len(reply.ToolCalls()),
			})
		}
		runErr = errdefs.Conflictf("graph-llm[%s/%s]: max iterations (%d) reached without final answer", deps.VesselID, deps.AgentName, maxIter)
		return board, runErr
	}), engine.Capabilities{
		SupportsResume:   false,
		EmitsUserPrompt:  false,
		EmitsCheckpoint:  false,
		RequiredDepNames: []string{depname.ToolAllowedNames},
	}), nil
}

// streamLLMRound drives one client.GenerateStream call, emitting
// engine.EmitStreamToken for every assistant content chunk and
// engine.EmitStreamToolCall for every tool call the model produces.
// Returns the accumulated assistant message so the caller can fold
// it into the conversation transcript.
//
// Falls back to client.Generate (non-streaming) when GenerateStream
// is unsupported or errors before the first chunk: this keeps the
// inline engine usable against minimal LLM mocks that don't bother
// implementing the streaming surface.
func streamLLMRound(
	ctx context.Context,
	host engine.Host,
	runID, actorID string,
	client llm.LLM,
	msgs []model.Message,
	opts []llm.GenerateOption,
) (model.Message, error) {
	stream, err := client.GenerateStream(ctx, msgs, opts...)
	if err != nil || stream == nil {
		// Streaming unsupported / startup error → fall back to
		// the synchronous Generate call with no per-token deltas.
		// nil-stream case covers test fakes that return
		// (nil, nil) from GenerateStream rather than implementing
		// the streaming surface; both paths share the same
		// fallback semantics.
		reply, _, gerr := client.Generate(ctx, msgs, opts...)
		if gerr != nil {
			return reply, gerr
		}
		_ = engine.EmitStreamToken(ctx, host, runID, actorID, reply.Content())
		return reply, nil
	}
	defer stream.Close()

	for stream.Next() {
		chunk := stream.Current()
		if chunk.Content != "" {
			_ = engine.EmitStreamToken(ctx, host, runID, actorID, chunk.Content)
		}
		for _, tc := range chunk.ToolCalls {
			_ = engine.EmitStreamToolCall(ctx, host, runID, actorID, tc.ID, tc.Name, tc.Arguments)
		}
	}
	if serr := stream.Err(); serr != nil {
		return stream.Message(), serr
	}
	return stream.Message(), nil
}

// buildAllowSet materialises the per-agent allow-list as a set so
// the per-iteration filter and the per-call execution gate run in
// O(1). nil / empty input yields an empty set that buildToolDefinitions
// and executeToolCall interpret as "deny all".
// resolveAllowSet returns the per-run tool allow-set the inline
// engine should enforce. Precedence (contract-audit Epic D):
//
//  1. engine.Run.Deps[depname.ToolAllowedNames] when present and
//     well-typed. agent.Run promotes agent.Agent.Tools into this
//     key (see sdk/agent/run.go::promoteAgentTools), so this is
//     the canonical SDK-wide path: the same allow-list reaches
//     this engine, sdk/graph/node/llmnode, and any future engine
//     that honours the contract.
//  2. Factory-time catalog.Deps.AgentTools fallback. Preserves
//     back-compat for hosts that build vessel without going
//     through agent.Run (custom drivers, legacy tests). Marked
//     Deprecated on the catalog struct; scheduled for removal in
//     v0.5.0.
//
// An empty / absent allow-set yields an empty map (the strict
// default — buildToolDefinitions and executeToolCall both
// interpret empty as "deny all", a misconfigured agent gets a
// safe failure instead of a silent permission escalation).
func resolveAllowSet(run engine.Run, deps Deps) map[string]struct{} {
	if names, err := engine.GetDep[[]string](run.Deps, depname.ToolAllowedNames); err == nil {
		return buildAllowSet(names)
	}
	return buildAllowSet(deps.AgentTools)
}

func buildAllowSet(tools []string) map[string]struct{} {
	if len(tools) == 0 {
		return map[string]struct{}{}
	}
	set := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		set[t] = struct{}{}
	}
	return set
}

// buildToolDefinitions returns the subset of the daemon-shared
// registry's definitions whose names appear in the per-agent
// allow-list. An empty allow-list yields an empty slice — the LLM
// will be invoked without any tools and will produce a plain-text
// reply. This is the strict default by design: a misconfigured
// agent should fail closed, not leak every registered tool.
func buildToolDefinitions(deps Deps, allow map[string]struct{}) []llm.ToolDefinition {
	if deps.ToolRegistry == nil || len(allow) == 0 {
		return nil
	}
	all := deps.ToolRegistry.Definitions()
	out := make([]llm.ToolDefinition, 0, len(allow))
	for _, def := range all {
		if _, ok := allow[def.Name]; ok {
			out = append(out, def)
		}
	}
	return out
}

// executeToolCall is the single tool-execution gate. It enforces
// the agent's allow-list (errdefs.PolicyDenied for off-list calls),
// then looks the tool up and runs it. Errors are surfaced inline
// as ToolResult.IsError=true so the LLM can react in its next turn
// rather than the engine bubbling the error up and aborting the run.
func executeToolCall(ctx context.Context, deps Deps, allow map[string]struct{}, call model.ToolCall) (string, error) {
	if deps.ToolRegistry == nil {
		return fmt.Sprintf("vesseld: no tool registry available to execute %q", call.Name), errdefs.NotAvailablef("no tool registry")
	}
	if _, ok := allow[call.Name]; !ok {
		msg := fmt.Sprintf("tool %q is not in the allow-list for agent %q", call.Name, deps.AgentName)
		return msg, errdefs.PolicyDeniedf("%s", msg)
	}
	tl, ok := deps.ToolRegistry.Get(call.Name)
	if !ok {
		return fmt.Sprintf("vesseld: tool %q is not registered", call.Name), errdefs.NotFoundf("tool %q", call.Name)
	}
	out, err := tl.Execute(ctx, call.Arguments)
	if err != nil {
		return fmt.Sprintf("tool %q error: %v", call.Name, err), err
	}
	return out, nil
}
