package delegation_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

type fakeDeployment struct {
	instances map[string]*agent.Agent
	order     []string
}

func (d *fakeDeployment) Agent(id string) (*agent.Agent, bool) {
	instance, ok := d.instances[id]
	return instance, ok
}

func (d *fakeDeployment) AgentNames() []string {
	return append([]string(nil), d.order...)
}

func testAgent(id, name, description string) *agent.Agent {
	return &agent.Agent{
		ID:   id,
		Card: agent.AgentCard{Name: name, Description: description},
	}
}

type fakeTargetSource struct {
	mu     sync.Mutex
	agents map[string]*agent.Agent
}

func newFakeTargetSource() *fakeTargetSource {
	return &fakeTargetSource{agents: make(map[string]*agent.Agent)}
}

func (s *fakeTargetSource) Dynamic(id string) (*agent.Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instance, ok := s.agents[id]
	return instance, ok
}

func (s *fakeTargetSource) DynamicNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.agents))
	for name := range s.agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *fakeTargetSource) set(id string, instance *agent.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[id] = instance
}

func (s *fakeTargetSource) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, id)
}

func TestDirectoryMergesDynamicTargets(t *testing.T) {
	dir := delegation.NewDirectory()
	deployment := &fakeDeployment{
		instances: map[string]*agent.Agent{"alpha": testAgent("alpha", "Alpha", "deployed")},
		order:     []string{"alpha"},
	}
	if err := dir.Bind(deployment); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	source := newFakeTargetSource()
	source.set("omega", testAgent("omega", "Omega", "dynamic"))
	if err := dir.BindTargetSource(source); err != nil {
		t.Fatalf("BindTargetSource: %v", err)
	}

	ctx := context.Background()
	targets, err := dir.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 2 || targets[0].ID != "alpha" || targets[1].ID != "omega" {
		t.Fatalf("List = %+v, want [alpha omega]", targets)
	}

	omega, err := dir.Get(ctx, "omega")
	if err != nil {
		t.Fatalf("Get(omega): %v", err)
	}
	if omega.ID != "omega" || omega.Description != "dynamic" {
		t.Fatalf("Get(omega) = %+v", omega)
	}
	instance, err := dir.Lookup(ctx, "omega")
	if err != nil {
		t.Fatalf("Lookup(omega): %v", err)
	}
	if instance.ID != "omega" {
		t.Fatalf("Lookup(omega) instance ID = %q", instance.ID)
	}
}

func TestDirectoryDynamicReflectsUnregister(t *testing.T) {
	dir := delegation.NewDirectory()
	if err := dir.Bind(&fakeDeployment{
		instances: map[string]*agent.Agent{"alpha": testAgent("alpha", "Alpha", "")},
		order:     []string{"alpha"},
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	source := newFakeTargetSource()
	source.set("omega", testAgent("omega", "Omega", ""))
	if err := dir.BindTargetSource(source); err != nil {
		t.Fatalf("BindTargetSource: %v", err)
	}

	source.remove("omega")
	ctx := context.Background()
	targets, _ := dir.List(ctx)
	if len(targets) != 1 || targets[0].ID != "alpha" {
		t.Fatalf("List after remove = %+v, want [alpha]", targets)
	}
	if _, err := dir.Get(ctx, "omega"); !errdefs.IsNotFound(err) {
		t.Fatalf("Get(omega) after remove error = %v, want not found", err)
	}
	if _, err := dir.Lookup(ctx, "omega"); !errdefs.IsNotFound(err) {
		t.Fatalf("Lookup(omega) after remove error = %v, want not found", err)
	}
}

func TestDirectoryBindTargetSourceSetOnce(t *testing.T) {
	dir := delegation.NewDirectory()
	if err := dir.Bind(&fakeDeployment{}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := dir.BindTargetSource(newFakeTargetSource()); err != nil {
		t.Fatalf("first BindTargetSource: %v", err)
	}
	if err := dir.BindTargetSource(newFakeTargetSource()); !errdefs.IsConflict(err) {
		t.Fatalf("second BindTargetSource error = %v, want conflict", err)
	}
}

func TestDirectoryFreezeTargets(t *testing.T) {
	dir := delegation.NewDirectory()
	if err := dir.Bind(&fakeDeployment{
		instances: map[string]*agent.Agent{"alpha": testAgent("alpha", "Alpha", "")},
		order:     []string{"alpha"},
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	source := newFakeTargetSource()
	source.set("omega", testAgent("omega", "Omega", ""))
	if err := dir.BindTargetSource(source); err != nil {
		t.Fatalf("BindTargetSource: %v", err)
	}

	dir.FreezeTargets(map[string]*agent.Agent{
		"omega": testAgent("omega", "Omega", ""),
	})
	// A registration made after freeze must not be visible.
	source.set("zeta", testAgent("zeta", "Zeta", ""))

	ctx := context.Background()
	targets, err := dir.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(targets) != 2 || targets[0].ID != "alpha" || targets[1].ID != "omega" {
		t.Fatalf("List after freeze = %+v, want [alpha omega]", targets)
	}
	if _, err := dir.Get(ctx, "zeta"); !errdefs.IsNotFound(err) {
		t.Fatalf("Get(zeta) after freeze error = %v, want not found", err)
	}
	if _, err := dir.Lookup(ctx, "omega"); err != nil {
		t.Fatalf("Lookup(omega) after freeze: %v", err)
	}
}

func TestDirectoryDynamicDedupeWithDeployed(t *testing.T) {
	dir := delegation.NewDirectory()
	if err := dir.Bind(&fakeDeployment{
		instances: map[string]*agent.Agent{"dup": testAgent("dup", "Dup", "deployed")},
		order:     []string{"dup"},
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	source := newFakeTargetSource()
	source.set("dup", testAgent("dup", "Dup", "dynamic"))
	if err := dir.BindTargetSource(source); err != nil {
		t.Fatalf("BindTargetSource: %v", err)
	}

	ctx := context.Background()
	targets, _ := dir.List(ctx)
	if len(targets) != 1 || targets[0].ID != "dup" {
		t.Fatalf("List = %+v, want one dup", targets)
	}
	// Dynamic-first resolution.
	instance, err := dir.Lookup(ctx, "dup")
	if err != nil {
		t.Fatalf("Lookup(dup): %v", err)
	}
	if instance.Card.Description != "dynamic" {
		t.Fatalf("Lookup(dup) resolved %q, want the dynamic instance", instance.Card.Description)
	}
}
