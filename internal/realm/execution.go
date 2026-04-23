package realm

import (
	"context"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/skill"
	"github.com/GizClaw/flowcraft/internal/template"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph/adapter"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/memory"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/rs/xid"

	otellog "go.opentelemetry.io/otel/log"
)

// executeAgent is the execution closure passed to AgentActor.
func (rt *Realm) executeAgent(ctx context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error) {
	if onStart := onStartFromReq(req); onStart != nil {
		onStart()
	}

	ctx = rt.prepareContext(ctx, agent, req.ContextID)
	rt.injectResolver(agent)

	if rt.deps.RunOverride != nil {
		return rt.deps.RunOverride(ctx, agent, req)
	}

	rt.enrichRequest(ctx, agent, req)

	result, err := rt.runtime.Run(ctx, agent, req)
	if err != nil {
		return nil, err
	}

	rt.postRun(ctx, agent, req, result)
	return result, nil
}

// RunResume restores execution from a BoardSnapshot using WithBoard.
func (rt *Realm) RunResume(ctx context.Context, agent *model.Agent, req *workflow.Request, snap *workflow.BoardSnapshot, startNode string) (*workflow.Result, error) {
	ctx = rt.prepareContext(ctx, agent, req.ContextID)
	rt.injectResolver(agent)

	board := workflow.RestoreBoard(snap)
	board.SetVar(workflow.VarStartTime, time.Now())
	for k, v := range req.Inputs {
		board.SetVar(k, v)
	}
	if q := req.Message.Content(); q != "" {
		board.SetVar(VarDecisionKey, q)
	}

	rt.enrichRequest(ctx, agent, req)

	if bus := eventBusFromReq(req); bus != nil {
		if req.Extensions == nil {
			req.Extensions = make(map[string]any)
		}
		eOpts := []executor.RunOption{
			executor.WithStartNode(startNode),
			executor.WithEventBus(bus),
		}
		if existing, ok := req.Extensions[adapter.ExtExecutorRunOpts]; ok {
			if sl, ok := existing.([]executor.RunOption); ok {
				eOpts = append(sl, eOpts...)
			}
		}
		req.Extensions[adapter.ExtExecutorRunOpts] = eOpts
	}

	result, err := rt.runtime.Run(ctx, agent, req, workflow.WithBoard(board))
	if err != nil {
		return nil, err
	}

	rt.postRun(ctx, agent, req, result)
	return result, nil
}

// ---------- context preparation ----------

func (rt *Realm) injectResolver(agent *model.Agent) {
	if rt.deps.StrategyResolver != nil {
		agent.SetStrategyResolver(rt.deps.StrategyResolver)
	}
}

func (rt *Realm) prepareContext(ctx context.Context, a *model.Agent, convID string) context.Context {
	ctx = kanban.WithProducerID(ctx, a.AgentID)
	ctx = executor.WithActorKey(ctx, a.AgentID)
	if rt.deps.Workspace != nil {
		ctx = workspace.WithWorkspace(ctx, rt.deps.Workspace)
	}
	if len(a.Config.SkillWhitelist) > 0 {
		ctx = skill.WithSkillWhitelist(ctx, a.Config.SkillWhitelist)
	}
	if convID != "" {
		ctx = memory.WithConversationID(ctx, convID)
	}
	return ctx
}

// ---------- request enrichment ----------

func (rt *Realm) enrichRequest(ctx context.Context, agent *model.Agent, req *workflow.Request) {
	if req.Inputs == nil {
		req.Inputs = make(map[string]any)
	}

	if agent.InputSchema != nil {
		if err := agent.InputSchema.Validate(req.Inputs); err != nil {
			telemetry.Warn(ctx, "input schema validation failed", otellog.String("error", err.Error()))
		}
		maps.Copy(req.Inputs, agent.InputSchema.ApplyDefaults(req.Inputs))
	}

	if agent.OutputSchema != nil {
		req.Inputs[workflow.VarOutputSchema] = agent.OutputSchema
	}

	if len(agent.Config.SubAgents) > 0 && rt.deps.Store != nil {
		var subAgents []*model.Agent
		for _, subID := range agent.Config.SubAgents {
			if sub, err := rt.deps.Store.GetAgent(ctx, subID); err == nil {
				subAgents = append(subAgents, sub)
			}
		}
		req.Inputs["sub_agents_table"] = template.BuildSubAgentsTable(subAgents)
	}

	if rt.deps.SummaryStore != nil && req.ContextID != "" {
		if index := memory.BuildSummaryIndex(ctx, rt.deps.SummaryStore, req.ContextID, 1500); index != "" {
			req.Inputs[workflow.VarSummaryIndex] = index
		}
	}

	req.Inputs[workflow.VarStartTime] = time.Now()

	rt.injectExecutorOpts(agent, req)
}

func (rt *Realm) injectExecutorOpts(agent *model.Agent, req *workflow.Request) {
	var opts []executor.RunOption
	opts = append(opts, executor.WithRunID(req.RunID))

	pc := defaultParallelConfig()
	if agent.Config.Parallel != nil {
		pc = mergeParallelConfig(pc, agent.Config.Parallel)
	}
	if pc.Enabled == nil || *pc.Enabled {
		opts = append(opts, executor.WithParallel(executor.ParallelConfig{
			Enabled:       true,
			MaxBranches:   pc.MaxBranches,
			MaxNesting:    pc.MaxNesting,
			MergeStrategy: executor.MergeStrategy(pc.MergeStrategy),
		}))
	}

	res := variable.NewResolver()
	res.AddScope("input", req.Inputs)
	res.AddScope("env", envVarsMap())
	opts = append(opts, executor.WithResolver(res))

	if rt.deps.CheckpointStore != nil {
		opts = append(opts, executor.WithCheckpointStore(rt.deps.CheckpointStore))
	}

	if req.Extensions == nil {
		req.Extensions = make(map[string]any)
	}
	if existing, ok := req.Extensions[adapter.ExtExecutorRunOpts]; ok {
		if sl, ok := existing.([]executor.RunOption); ok {
			opts = append(sl, opts...)
		}
	}
	req.Extensions[adapter.ExtExecutorRunOpts] = opts
}

// ---------- post-run ----------

func (rt *Realm) postRun(ctx context.Context, agent *model.Agent, req *workflow.Request, result *workflow.Result) {
	if result == nil {
		return
	}

	if rt.deps.CheckpointStore != nil && result.Status == workflow.StatusCompleted {
		if gd := agent.StrategyDef.AsGraph(); gd != nil && gd.Name != "" {
			if mgr, ok := rt.deps.CheckpointStore.(executor.CheckpointManager); ok {
				_ = mgr.Delete(gd.Name)
			}
		}
	}

	runID, _ := result.State["run_id"].(string)
	answer := result.Text()

	var elapsedMs int64
	if startRaw, ok := result.LastBoard.GetVar(workflow.VarStartTime); ok {
		if t, ok := startRaw.(time.Time); ok {
			elapsedMs = time.Since(t).Milliseconds()
			result.State["elapsed_ms"] = elapsedMs
		}
	}
	result.State["message_id"] = xid.New().String()

	if rt.deps.Store != nil {
		u := result.Usage
		run := &model.WorkflowRun{
			ID:             runID,
			AgentID:        agent.AgentID,
			ActorID:        kanban.ProducerIDFrom(ctx),
			ConversationID: req.ContextID,
			Input:          req.Message.Content(),
			Output:         answer,
			Status:         string(result.Status),
			Usage:          &u,
			ElapsedMs:      elapsedMs,
			CreatedAt:      time.Now(),
		}
		if result.LastBoard != nil {
			if toolCalls, ok := result.LastBoard.GetVar(node.VarToolCalls); ok {
				if run.Outputs == nil {
					run.Outputs = make(map[string]any)
				}
				run.Outputs["tool_calls"] = toolCalls
			}
		}
		_ = rt.deps.Store.SaveWorkflowRun(ctx, run)
	}

	if req.ContextID != "" {
		rt.persistMessages(ctx, agent, req, result)
	}

	rt.extractLTM(ctx, agent, req, result)
}

// persistMessages publishes chat.message.sent envelopes for every
// non-system message produced during this run. Persistence is no longer
// done via Store.SaveMessage — the ChatProjector materialises messages
// from these envelopes (see internal/projection/chat).
func (rt *Realm) persistMessages(ctx context.Context, agent *model.Agent, req *workflow.Request, result *workflow.Result) {
	rt.ensureConversation(ctx, agent.AgentID, req)

	if rt.deps.PublishMessage == nil {
		// In test setups (mock realm) PublishMessage may be unset; nothing
		// to persist then. Production wiring guarantees a non-nil fn.
		return
	}

	for _, m := range result.Messages {
		if m.Role == sdkmodel.RoleSystem {
			continue
		}
		content := m.Content()
		if content == "" {
			continue
		}
		if err := rt.deps.PublishMessage(ctx, req.ContextID, string(m.Role), content, 0); err != nil {
			telemetry.Error(ctx, "publish chat.message.sent failed",
				otellog.String("conversation_id", req.ContextID),
				otellog.String("error", err.Error()))
		}
	}
}

func (rt *Realm) extractLTM(ctx context.Context, agent *model.Agent, req *workflow.Request, result *workflow.Result) {
	if rt.deps.Extractor == nil || !agent.Config.Memory.LongTerm.Enabled {
		return
	}
	newMsgs := result.Messages
	if len(newMsgs) == 0 {
		return
	}

	scope := memoryScope(ctx, req)
	lt := agent.Config.Memory.LongTerm
	runtimeID := runtimeIDFor(ctx, req)
	extractInput := memory.ExtractInput{
		RuntimeID: runtimeID,
		Messages:  newMsgs,
		Source: memory.MemorySource{
			RuntimeID:      runtimeID,
			ConversationID: req.ContextID,
		},
		Scope:              scope,
		ScopeInExtract:     &lt.ScopeEnabled,
		GlobalCategories:   lt.GlobalCategories,
		LongTermCategories: lt.Categories,
	}
	extractCtx := context.WithoutCancel(ctx)
	go func() {
		if err := rt.deps.Extractor.Extract(extractCtx, extractInput); err != nil {
			telemetry.Warn(extractCtx, "long-term memory extraction failed",
				otellog.String("error", err.Error()),
				otellog.String("runtime_id", runtimeID))
		}
	}()
}

func (rt *Realm) ensureConversation(ctx context.Context, agentID string, req *workflow.Request) {
	if rt.deps.Store == nil || req.ContextID == "" {
		return
	}
	if _, err := rt.deps.Store.GetConversation(ctx, req.ContextID); err == nil {
		return
	}
	if _, err := rt.deps.Store.CreateConversation(ctx, &model.Conversation{
		ID:        req.ContextID,
		AgentID:   agentID,
		RuntimeID: req.RuntimeID,
	}); err != nil {
		telemetry.Warn(ctx, "ensure conversation failed",
			otellog.String("conversation_id", req.ContextID),
			otellog.String("error", err.Error()))
	}
}

// ---------- helpers ----------

func eventBusFromReq(req *workflow.Request) event.EventBus {
	if req.Extensions == nil {
		return nil
	}
	if v, ok := req.Extensions["event_bus"]; ok {
		if bus, ok := v.(event.EventBus); ok {
			return bus
		}
	}
	return nil
}

func onStartFromReq(req *workflow.Request) func() {
	if req.Extensions == nil {
		return nil
	}
	if v, ok := req.Extensions["on_start"]; ok {
		if fn, ok := v.(func()); ok {
			return fn
		}
	}
	return nil
}

func runtimeIDFor(ctx context.Context, req *workflow.Request) string {
	rid := model.RuntimeIDFrom(ctx)
	if rid == "" {
		rid = req.RuntimeID
	}
	return rid
}

func defaultParallelConfig() *model.ParallelConfig {
	enabled := true
	return &model.ParallelConfig{
		Enabled:       &enabled,
		MaxBranches:   10,
		MaxNesting:    3,
		MergeStrategy: "last_wins",
	}
}

func mergeParallelConfig(base, override *model.ParallelConfig) *model.ParallelConfig {
	if override.Enabled != nil {
		base.Enabled = override.Enabled
	}
	if override.MaxBranches > 0 {
		base.MaxBranches = override.MaxBranches
	}
	if override.MaxNesting > 0 {
		base.MaxNesting = override.MaxNesting
	}
	if override.MergeStrategy != "" {
		base.MergeStrategy = override.MergeStrategy
	}
	return base
}

func envVarsMap() map[string]any {
	result := make(map[string]any)
	for _, env := range os.Environ() {
		if k, v, ok := strings.Cut(env, "="); ok {
			result[k] = v
		}
	}
	return result
}
