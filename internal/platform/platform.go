// Package platform provides the application facade aggregating all subsystems.
// Adapter layers (api, gateway) depend on Platform instead of individual packages.
package platform

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/pluginhost"
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

// Platform is the unified application facade for adapter layers.
type Platform struct {
	Store        model.Store
	Realms       realm.RealmProvider
	Compiler     *compiler.Compiler
	SchemaReg    *node.SchemaRegistry
	TemplateReg  *template.Registry
	PluginReg    *pluginhost.Registry
	Knowledge    knowledge.Store
	VersionStore version.VersionStore
	LTStore      memory.LongTermStore
	EventBus     event.EventBus
	LLMResolver  llm.LLMResolver
	SkillStore   *skill.SkillStore
	ToolRegistry *tool.Registry
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

// Board returns the current Realm's TaskBoard.
func (p *Platform) Board(ctx context.Context) (*kanban.Board, error) {
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	return r.Board(), nil
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
	r, err := p.Realms.Resolve(ctx)
	if err != nil {
		return false, err
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
