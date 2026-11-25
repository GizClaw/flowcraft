package host

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
)

// statefulView is a deployment's exposure with the state a real projection carries: the tools
// one conversation has used, and the pool cap that evicts them.
//
// It is a fake, not a stub: the point of these tests is that this state must not be shared, so
// the fake has to have state to share.
type statefulView struct {
	mu   sync.Mutex
	defs []message.ToolDefinition
	used []string
}

func (v *statefulView) Definitions() []message.ToolDefinition {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]message.ToolDefinition, 0, len(v.defs))
	for _, definition := range v.defs {
		evicted := false
		for _, name := range v.used {
			if name == definition.Name {
				evicted = true
				break
			}
		}
		if !evicted {
			out = append(out, definition)
		}
	}
	return out
}

func (v *statefulView) Visible(string) bool { return true }

func (v *statefulView) RecordCall(call message.ToolCall) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.used = append(v.used, call.Name)
}

// viewRecorder is the seam under test: one factory per deployment, and every view it makes
// kept so a test can assert which session saw which.
type viewRecorder struct {
	mu sync.Mutex
	// projections counts the exposure factories the host built: one per distinct authority.
	projections int
	// built counts the views handed to sessions: one per session.
	built int
	views []*statefulView
}

func (r *viewRecorder) factory(catalog tool.Catalog, allowed policy.ToolSet) (tool.ViewFactory, error) {
	r.mu.Lock()
	r.projections++
	r.mu.Unlock()
	return r.make, nil
}

func (r *viewRecorder) make() tool.View {
	r.mu.Lock()
	defer r.mu.Unlock()
	view := &statefulView{defs: []message.ToolDefinition{{Name: "read"}, {Name: "write"}}}
	r.built++
	r.views = append(r.views, view)
	return view
}

// viewHost installs one deployment over a process whose projection is the recorder.
func viewHost(t *testing.T, recorder *viewRecorder) *Agents {
	t.Helper()
	log := &journal{}
	plan := deploy.ProcessPlan{
		Backends: []deploy.PlannedBackend{boundBackend("local", &boundary{log: log, filesystemBounds: true})},
	}
	options := testOptions(t)
	options.View = func(catalog tool.Catalog, allowed policy.ToolSet) (tool.ViewFactory, error) {
		return recorder.factory(catalog, allowed)
	}
	res, err := Open(t.Context(), plan, options)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := NewAgents(res)
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &seenProgram{driver: &seenDriver{}}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents
}

// TestEverySessionGetsItsOwnView is the isolation rule: the grant decides what may be shown and
// belongs to the deployment, while the discovery pool and the budget a round has spent belong to
// one conversation. Sharing the view let one session's tool calls evict the other's tools, so a
// prompt became a function of what somebody else did.
func TestEverySessionGetsItsOwnView(t *testing.T) {
	recorder := &viewRecorder{}
	agents := viewHost(t, recorder)

	first, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local"))
	if err != nil {
		t.Fatalf("Open s1: %v", err)
	}
	second, err := agents.Open(t.Context(), "s2", "referee", permissiveGrant("local"))
	if err != nil {
		t.Fatalf("Open s2: %v", err)
	}

	if first.binding.View == second.binding.View {
		t.Fatal("two sessions share one view; their tool budgets would interfere")
	}

	// The behaviour the identity is there to protect: a call in one session must not change what
	// the other session's model is shown.
	before := len(second.binding.View.Definitions())
	first.binding.View.RecordCall(message.ToolCall{Name: "read"})
	if after := len(second.binding.View.Definitions()); after != before {
		t.Fatalf("a call in s1 changed s2's tool list: %d → %d", before, after)
	}
	// And the session that made the call does lose the tool: budget consumption still works.
	if after := len(first.binding.View.Definitions()); after != before-1 {
		t.Fatalf("s1's own tool list = %d, want %d after a call", after, before-1)
	}
}

// TestTheFactoryIsBuiltOnceAndAskedPerSession pins the shape the seam has: the exposure (policy,
// shared index) is built once per authority, and every session opened with that authority gets
// its own view from it. Rebuilding the exposure per session would rebuild the search index.
func TestTheFactoryIsBuiltOnceAndAskedPerSession(t *testing.T) {
	recorder := &viewRecorder{}
	agents := viewHost(t, recorder)

	sessions := []string{"s1", "s2", "s3"}
	for _, id := range sessions {
		if _, err := agents.Open(t.Context(), id, "referee", permissiveGrant("local")); err != nil {
			t.Fatalf("Open %s: %v", id, err)
		}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	// One projection for the one authority, and one view per session: the projection holds the
	// policy and whatever index it shares, the view holds the conversation's state.
	if recorder.projections != 1 {
		t.Fatalf("projections = %d, want one for one authority", recorder.projections)
	}
	if want := len(sessions); recorder.built != want {
		t.Fatalf("views built = %d, want one per session (%d)", recorder.built, want)
	}
}

// TestAProjectionThatFailsIsRefusedAtOpen keeps the error where a reader can act on it: an
// authority this process cannot project is a session that must not open, not one that opens and
// then panics on its first tool call. The projection is built lazily because the authority is
// per session, so the refusal belongs to Open.
func TestAProjectionThatFailsIsRefusedAtOpen(t *testing.T) {
	refused := errors.New("this policy cannot be projected")
	log := &journal{}
	plan := deploy.ProcessPlan{
		Backends: []deploy.PlannedBackend{boundBackend("local", &boundary{log: log, filesystemBounds: true})},
	}
	options := testOptions(t)
	options.View = func(tool.Catalog, policy.ToolSet) (tool.ViewFactory, error) { return nil, refused }
	res, err := Open(t.Context(), plan, options)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := NewAgents(res)
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &seenProgram{driver: &seenDriver{}}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err == nil {
		t.Fatal("a session whose authority cannot be projected was opened")
	} else if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the projection's", err)
	}
}

// TestAProjectionThatBuildsNothingIsRefused is the other half of the same rule: a factory that
// returns nil is a wiring bug, and a session that held a nil view would panic deep in dispatch
// instead of failing where the mistake was made.
func TestAProjectionThatBuildsNothingIsRefused(t *testing.T) {
	log := &journal{}
	plan := deploy.ProcessPlan{
		Backends: []deploy.PlannedBackend{boundBackend("local", &boundary{log: log, filesystemBounds: true})},
	}
	options := testOptions(t)
	options.View = func(tool.Catalog, policy.ToolSet) (tool.ViewFactory, error) {
		return func() tool.View { return nil }, nil
	}
	res, err := Open(t.Context(), plan, options)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := NewAgents(res)
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &seenProgram{driver: &seenDriver{}}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(context.Background(), "s1", "referee", permissiveGrant("local")); err == nil {
		t.Fatal("a session opened with no tool view")
	}
}
