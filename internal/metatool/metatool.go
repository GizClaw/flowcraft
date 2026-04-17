// Package metatool provides platform-level tools used by the CoPilot system
// to query, create, and manage Agents, Graphs, and Node schemas.
//
// Tools are organized by file:
//   - agent_query.go:  agent (read-only, list or get by id)
//   - agent_manage.go: agent_create
//   - graph.go:        graph (get/update/compile/publish via action param)
//   - node.go:         schema (node_list/node_usage/model_list/tool_list via action param)
package metatool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/version"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/rs/xid"
)

// Deps holds dependencies injected into Meta Tools at construction time.
type Deps struct {
	Store        model.Store
	Compiler     *compiler.Compiler
	VersionStore version.VersionStore
	SchemaReg    *node.SchemaRegistry
	ToolRegistry *tool.Registry
	EventBus     event.EventBus

	graphMu sync.Map // per-agent mutex: agent_id → *sync.Mutex
}

// lockAgent acquires a per-agent mutex to serialize read-modify-write operations
// on the same agent's graph definition.
func (d *Deps) lockAgent(agentID string) func() {
	if d == nil {
		return func() {}
	}
	v, _ := d.graphMu.LoadOrStore(agentID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

var errNotAvailable = errdefs.NotAvailablef("dependency not available in current milestone")

func storeRequired(deps *Deps) (model.Store, error) {
	if deps == nil || deps.Store == nil {
		return nil, errNotAvailable
	}
	return deps.Store, nil
}

func jsonResult(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	return string(data), nil
}

func (d *Deps) publishGraphChanged(ctx context.Context, agentID string) {
	if d == nil || d.EventBus == nil {
		return
	}
	_ = d.EventBus.Publish(ctx, event.Event{
		ID:        xid.New().String(),
		Type:      event.EventGraphChanged,
		GraphID:   agentID,
		Timestamp: time.Now(),
		Payload:   map[string]any{"agent_id": agentID},
	})
}

// saveDraftAfterGraphChange persists the current graph definition as a draft
// version after a Meta Tool mutates the graph.
func (d *Deps) saveDraftAfterGraphChange(ctx context.Context, agentID string, graphDef *model.GraphDefinition) {
	if d == nil || d.VersionStore == nil || graphDef == nil {
		return
	}
	draft, err := d.VersionStore.GetDraft(ctx, agentID)
	if err != nil {
		versions, listErr := d.VersionStore.ListVersions(ctx, agentID)
		if listErr != nil {
			return
		}
		maxVersion := 0
		for _, v := range versions {
			if v.Version > maxVersion {
				maxVersion = v.Version
			}
		}
		draft = &model.GraphVersion{
			AgentID: agentID,
			Version: maxVersion + 1,
		}
	}
	draft.GraphDef = graphDef
	_ = d.VersionStore.SaveDraft(ctx, draft)
}

// Register registers all platform tools into the given Registry.
// Agent query tool (agent) uses ScopeAgent so it is
// visible to user-created agents for multi-agent coordination.
// All other metatools use ScopePlatform (hidden from tool_list and frontend).
func Register(reg *tool.Registry, deps *Deps) {
	for _, t := range buildAgentQueryTools(deps) {
		reg.Register(t) // ScopeAgent (default)
	}
	for _, t := range buildAgentManageTools(deps) {
		reg.RegisterWithScope(t, tool.ScopePlatform)
	}
	for _, t := range buildGraphTools(deps) {
		reg.RegisterWithScope(t, tool.ScopePlatform)
	}
	for _, t := range buildSchemaTools(deps) {
		reg.RegisterWithScope(t, tool.ScopePlatform)
	}
}
