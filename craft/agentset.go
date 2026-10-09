package craft

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// AgentDecl is one desired runtime agent in a declaration set passed to
// [Craft.SyncAgents]. The declaration set is the application's view of
// the dynamic agents a runtime should have; the coordinator makes the
// runtime match it without reloading the deployment generation.
type AgentDecl struct {
	// Name is the runtime registration id.
	Name string
	// Def is the agent definition registered under Name. It runs
	// through the same assembly path as deployment (deploy.BindAgent),
	// so it may reference deployment resources and `${...}` values.
	Def agent.Definition
	// Rev is the application-owned declaration revision and the only
	// input to the diff: a name whose Rev is unchanged since it was
	// last applied counts as unchanged, and a changed Rev makes the
	// declaration a replacement (unregister + register). Whatever the
	// application treats as part of the declaration's identity — the
	// definition, its settings, the resources it references — must be
	// reflected in Rev; a content hash is the natural choice. An empty
	// Rev is rejected.
	Rev string
}

// AgentSyncReport summarizes one [Craft.SyncAgents] pass. Every list is
// in name order.
type AgentSyncReport struct {
	// Registered lists declarations that were registered on a name
	// with no live agent (nothing was replaced).
	Registered []string
	// Updated lists declarations whose live agent was replaced
	// (unregister + register).
	Updated []string
	// Removed lists names an earlier pass applied that this set no
	// longer declares and that were unregistered. A recorded name
	// whose agent is already gone is dropped from the records without
	// a call and without appearing here.
	Removed []string
	// Errors carries one error per declaration that failed, wrapped
	// with the runtime key and the name. A failed declaration is not
	// recorded as applied, so a later pass retries it even when its
	// Rev is unchanged. A replacement can fail after its unregister
	// step, in which case the name is left without a live agent and
	// the error reports it; the next pass retries the declaration.
	Errors []error
}

// SyncAgents reconciles the dynamically registered agents of one
// runtime with a declaration set: names with no live agent are
// registered, names whose Rev changed are replaced (unregister +
// register), and names an earlier pass applied that the set no longer
// declares are unregistered. The set is the full desired set — an
// empty set removes everything a previous pass applied.
//
// The pass drives only the runtime's dynamic agent surface. It never
// reloads, never rebuilds a generation and never touches deployed
// agents: a declaration whose name the deployment document declares is
// an error (the deployed agent wins; deployment documents must not
// declare names the coordinator manages). No reload is triggered —
// call [Craft.ReloadRuntime] with [ReasonAgent] when the document
// itself must change.
//
// Applied declarations are remembered per Craft and runtime key and
// survive reloads and runtime reopen. A record is trusted only while
// the runtime's live view ([runtime.Runtime.Agent]) still has the name
// — an agent a reload re-bound counts as the same declaration, and a
// record whose agent disappeared is not treated as applied and is
// registered again. A name that is live but not recorded (a direct
// [Craft.RegisterAgent] call, an agent that predates this Craft) is
// replaced: the declaration set is authoritative over the runtime's
// live view.
//
// The pass is serialized with [Craft.ReloadRuntime] and
// [Craft.ReloadAll]: a reload of this Craft never interleaves with a
// pass. The direct [Craft.RegisterAgent] / [Craft.UnregisterAgent]
// calls remain the escape hatch for single registrations and are not
// serialized with passes — a name driven by both may fail with a
// per-name conflict. RuntimeBinder callbacks and craft-plane event
// sinks run on paths that may hold the same lock, so they must not call
// SyncAgents, ReloadRuntime or ReloadAll synchronously; dispatch
// asynchronously instead.
//
// A per-declaration failure does not abort the pass: the remaining
// declarations still apply. The error is nil when every declaration is
// in sync (applied earlier or now) and otherwise joins the report's
// errors; callers that need per-name attribution read the report.
// Malformed input (nil context, unknown key, an empty or duplicated
// name, an empty Rev) rejects the whole call before anything mutates
// and returns the zero report.
func (c *Craft) SyncAgents(
	ctx context.Context,
	key RuntimeKey,
	decls []AgentDecl,
) (AgentSyncReport, error) {
	if ptr.IsNil(ctx) {
		return AgentSyncReport{}, errdefs.Validationf(
			"craft: SyncAgents context is required")
	}
	declared, err := checkAgentDecls(decls)
	if err != nil {
		return AgentSyncReport{}, err
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	rt, ok := c.Runtime(key)
	if !ok {
		return AgentSyncReport{}, fmt.Errorf("%w: %q", ErrRuntimeNotFound, key)
	}
	records := c.agentRevs[key]
	if records == nil {
		records = make(map[string]string, len(declared))
		c.agentRevs[key] = records
	}
	report := syncAgentDecls(ctx, key, rt, declared, records)
	if len(records) == 0 {
		delete(c.agentRevs, key)
	}
	return report, errors.Join(report.Errors...)
}

// checkAgentDecls validates a declaration set before any mutation:
// names must be non-empty and unique, and Rev must be non-empty so the
// diff has something to compare. It returns the set indexed by name.
func checkAgentDecls(decls []AgentDecl) (map[string]AgentDecl, error) {
	declared := make(map[string]AgentDecl, len(decls))
	for i, decl := range decls {
		if strings.TrimSpace(decl.Name) == "" {
			return nil, errdefs.Validationf(
				"craft: SyncAgents declaration[%d] has an empty name", i)
		}
		if strings.TrimSpace(decl.Rev) == "" {
			return nil, errdefs.Validationf(
				"craft: SyncAgents declaration %q has an empty rev; "+
					"the diff is rev-driven, so an empty rev could never "+
					"be updated", decl.Name)
		}
		if _, duplicate := declared[decl.Name]; duplicate {
			return nil, errdefs.Validationf(
				"craft: SyncAgents has duplicate declaration %q", decl.Name)
		}
		declared[decl.Name] = decl
	}
	return declared, nil
}

// syncAgentDecls performs one pass against a live runtime; callers hold
// the lifecycle lock. Records is the applied set, updated in place.
func syncAgentDecls(
	ctx context.Context,
	key RuntimeKey,
	rt *runtime.Runtime,
	declared map[string]AgentDecl,
	records map[string]string,
) AgentSyncReport {
	var report AgentSyncReport
	fail := func(name string, err error) {
		report.Errors = append(report.Errors, fmt.Errorf(
			"craft: sync agents %q: agent %q: %w", key, name, err))
	}
	live := func(name string) bool {
		_, ok := rt.Agent(name)
		return ok
	}

	// Removals first: names an earlier pass recorded that this set
	// dropped. The runtime's live view has the last word — a record
	// whose agent is already gone is dropped quietly (there is nothing
	// to remove), and a name whose unregister fails keeps its record so
	// a later pass retries the removal.
	removed := make([]string, 0, len(records))
	for name := range records {
		if _, stillDeclared := declared[name]; !stillDeclared {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		if !live(name) {
			delete(records, name)
			continue
		}
		if err := rt.UnregisterAgent(ctx, name); err != nil {
			fail(name, err)
			continue
		}
		delete(records, name)
		report.Removed = append(report.Removed, name)
	}

	// Upserts: declarations that are new, changed (Rev differs) or
	// recorded but no longer live. A live name is replaced first, so
	// the registration cannot collide with what is registered.
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		decl := declared[name]
		if applied, recorded := records[name]; recorded && applied == decl.Rev && live(name) {
			continue
		}
		replaced := false
		if live(name) {
			if err := rt.UnregisterAgent(ctx, name); err != nil {
				// The old agent stays live and keeps its record: the
				// replacement never happened, so a later pass may
				// retry it.
				fail(name, err)
				continue
			}
			delete(records, name)
			replaced = true
		}
		if _, err := rt.RegisterAgent(ctx, name, decl.Def); err != nil {
			fail(name, err)
			continue
		}
		records[name] = decl.Rev
		if replaced {
			report.Updated = append(report.Updated, name)
		} else {
			report.Registered = append(report.Registered, name)
		}
	}
	return report
}
