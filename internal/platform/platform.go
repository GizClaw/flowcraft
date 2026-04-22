// Package platform provides the application facade aggregating all subsystems.
// Adapter layers (api, gateway) depend on Platform instead of individual packages.
package platform

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/knowledgeproc"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/pluginhost"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/skill"
	"github.com/GizClaw/flowcraft/internal/template"
	"github.com/GizClaw/flowcraft/internal/version"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"
)

// RunResult is the outcome of a single agent execution.
type RunResult struct {
	Value *workflow.Result
	Err   error
}

// Platform is the unified application facade for adapter layers.
type Platform struct {
	Store             model.Store
	Realms            realm.RealmProvider
	Compiler          *compiler.Compiler
	SchemaReg         *node.SchemaRegistry
	TemplateReg       *template.Registry
	PluginReg         *pluginhost.Registry
	Knowledge         knowledge.Store
	KnowledgeWorker   *knowledgeproc.Worker
	VersionStore      version.VersionStore
	LTStore           memory.LongTermStore
	EventBus          event.EventBus
	LLMResolver       llm.LLMResolver
	SkillStore        *skill.SkillStore
	ToolRegistry      *tool.Registry
	ProjectionManager *projection.Manager
}

// RunAgent resolves the current Realm and dispatches a run to the named agent's actor.
func (p *Platform) RunAgent(ctx context.Context, agentID string, req *workflow.Request, opts ...realm.ActorOption) (<-chan realm.RunResult, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	agent, err := p.Store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return r.SendToAgent(ctx, agent, req, opts...), nil
}

// RunAgentStreaming starts an agent run and returns a result channel, event
// subscription for live streaming, and the actor key for event correlation.
func (p *Platform) RunAgentStreaming(ctx context.Context, agent *model.Agent, req *workflow.Request, persistent bool) (<-chan RunResult, event.Subscription, string, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, nil, "", err
	}

	var opts []realm.ActorOption
	if persistent {
		opts = append(opts, realm.WithPersistent())
	}
	actor := r.GetOrCreateActor(agent.AgentID, opts...)
	actorKey := actor.ActorKey()

	sub, err := actor.Bus().Subscribe(ctx, event.EventFilter{ActorID: actorKey})
	if err != nil {
		return nil, nil, "", err
	}

	realmCh := r.SendToAgent(ctx, agent, req, opts...)
	ch := make(chan RunResult, 1)
	go func() {
		res := <-realmCh
		ch <- RunResult{Value: res.Value, Err: res.Err}
	}()

	return ch, sub, actorKey, nil
}

// ResumeAgent restores execution from a BoardSnapshot.
func (p *Platform) ResumeAgent(ctx context.Context, agent *model.Agent, req *workflow.Request, snap *workflow.BoardSnapshot, startNode string) (*workflow.Result, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	return r.RunResume(ctx, agent, req, snap, startNode)
}

// LoadCheckpoint returns the latest checkpoint snapshot for an agent.
func (p *Platform) LoadCheckpoint(ctx context.Context, agentID string) (*workflow.BoardSnapshot, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	cpStore := r.CheckpointStore()
	if cpStore == nil {
		return nil, nil
	}
	agent, err := p.Store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	gd := agent.StrategyDef.AsGraph()
	if gd == nil || gd.Name == "" {
		return nil, nil
	}
	cp, err := cpStore.Load(gd.Name, "")
	if err != nil || cp == nil {
		return nil, nil
	}
	return cp.Board, nil
}

// Board returns the current Realm's TaskBoard (nil if no realm).
func (p *Platform) Board(ctx context.Context) (*kanban.Board, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	return r.Board(), nil
}

// TaskBoard returns the current board without error (best-effort).
func (p *Platform) TaskBoard() *kanban.Board {
	r, ok := p.Realms.Current()
	if !ok || r == nil {
		return nil
	}
	return r.Board()
}

// Bus returns the current Realm's EventBus.
func (p *Platform) Bus(ctx context.Context) (event.EventBus, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	return r.Bus(), nil
}

// AbortAgent aborts the running actor for the given agent ID.
func (p *Platform) AbortAgent(ctx context.Context, agentID string) (bool, error) {
	r, ok := p.Realms.Current()
	if !ok {
		return false, nil
	}
	return r.AbortActor(agentID), nil
}

// RealmStats returns stats from the current RealmProvider.
func (p *Platform) RealmStats() realm.RealmProviderStats {
	if sp, ok := p.Realms.(*realm.SingleRealmProvider); ok {
		return sp.Stats()
	}
	return realm.RealmProviderStats{}
}

// InstantiateTemplate resolves a template by name, applies params, and
// returns the resulting graph definition as a map.
func (p *Platform) InstantiateTemplate(name string, params map[string]any) (map[string]any, error) {
	t, ok := p.TemplateReg.Get(name)
	if !ok {
		return nil, nil
	}
	return template.Instantiate(t, params)
}

// SaveTemplate creates or updates a template and returns the saved value.
func (p *Platform) SaveTemplate(ctx context.Context, name, label, desc, category string, graphDef any) (any, error) {
	gt := template.GraphTemplate{
		Name:        name,
		Label:       label,
		Description: desc,
		Category:    category,
		GraphDef:    graphDef,
	}
	if err := p.TemplateReg.Save(ctx, gt); err != nil {
		return nil, err
	}
	return gt, nil
}

// SyncPluginSchemas re-syncs plugin node schemas and tools after reload.
func (p *Platform) SyncPluginSchemas() {
	if p.PluginReg != nil && p.SchemaReg != nil {
		pluginhost.CleanupSchemas(p.PluginReg, p.SchemaReg)
		pluginhost.InjectSchemas(p.PluginReg, p.SchemaReg)
	}
	if p.PluginReg != nil && p.ToolRegistry != nil {
		pluginhost.CleanupTools(p.PluginReg, p.ToolRegistry)
		pluginhost.InjectTools(p.PluginReg, p.ToolRegistry)
	}
}
