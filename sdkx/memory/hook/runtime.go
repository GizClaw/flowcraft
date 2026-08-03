// Package hook wires the *memory.Runtime surface into the
// agent lifecycle through deploy-registered factories.
//
// Each factory in this package reads its typed settings off
// the deploy document, resolves the *memory.Runtime from the
// declared dep, and returns an agent.Preparer or
// agent.Committer that calls the corresponding op on every
// Run. Settings are decoded strictly with
// [deploy.DecodeSettings] so a typo in the document fails
// build, not run.
//
// # Mapping
//
//   - memory.load    → agent.Preparer   (LoadPreparerFactory)
//   - memory.recall  → agent.Preparer   (RecallPreparerFactory)
//   - memory.append  → agent.Committer  (AppendCommitterFactory)
//
// memory.import / memory.compact / memory.archive are
// intentionally absent: Import is a tool (sdkx/tool/memory),
// Compact/Archive are scheduled maintenance tasks
// (sdkx/scheduler/memory). See the memory guide for the
// rationale.
package hook

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// ScopeConfig is the YAML shape of a memory Scope the hook
// settings carry. Only the hard-partition fields are part of
// the schema; soft fields (AgentID, ConversationID, DatasetID)
// are request-time concerns, not deployment-time.
type ScopeConfig struct {
	RuntimeID string `yaml:"runtime_id"`
	UserID    string `yaml:"user_id,omitempty"`
}

// resolveScope returns a fully-populated memory.Scope,
// filling any empty field from runtime.Spec().DefaultScope.
// The kernel rejects empty RuntimeID, so the resolved scope is
// always valid when rt is built by the config package.
func resolveScope(rt *memory.Runtime, cfg ScopeConfig) memory.Scope {
	def := rt.Spec().DefaultScope
	if cfg.RuntimeID == "" {
		cfg.RuntimeID = def.RuntimeID
	}
	if cfg.UserID == "" {
		cfg.UserID = def.UserID
	}
	return memory.Scope{
		RuntimeID: cfg.RuntimeID,
		UserID:    cfg.UserID,
	}
}

// runtimeDepName is the documented dep name hook factories
// look for when resolving the *memory.Runtime from
// deploy.HookInput.Deps. A deployment that registers
// memory.load / memory.recall / memory.append without binding
// a `runtime: memory` dep fails build, not run.
const runtimeDepName = "runtime"

// resolveRuntime fetches the *memory.Runtime a hook depends
// on. The factory type-asserts to *memory.Runtime and rejects
// the build when the dep is missing or has the wrong type.
func resolveRuntime(in deploy.HookInput) (*memory.Runtime, error) {
	raw, ok := in.Dep(runtimeDepName)
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf(
			"memory hook: dep %q is not bound (declare it under deps in the deploy document)",
			runtimeDepName))
	}
	rt, ok := raw.(*memory.Runtime)
	if !ok || rt == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory hook: dep %q is %T, want *memory.Runtime",
			runtimeDepName, raw))
	}
	return rt, nil
}
