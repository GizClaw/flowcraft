package runtime

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// Generation is one immutable deployment snapshot plus the services
// derived from it: the built result, the host factory, the per-agent
// resolver, and the dynamic tool catalog. A generation is built as a
// value and swapped into the Runtime atomically; it is never mutated
// while current except for the one-time freeze performed by Reload
// (adopting the dynamic instances that retire with it).
type Generation struct {
	id          uint64
	doc         deploy.Document
	registry    *AgentRegistry
	result      *deploy.Result
	bus         event.Bus
	hostFactory session.HostFactory
	resolver    session.InstanceResolver
	catalog     *catalogRegistry

	// adopted are the dynamic agent instances that retire with this
	// generation. They are frozen by Reload at swap time (empty while
	// current) and closed exactly once when the generation closes.
	adopted map[string]*agent.Agent

	closeOnce sync.Once
	closeErr  error
}

// generationResolver pins deployed agents to one generation's result and
// serves dynamic agents from the live registry while current, or from the
// frozen set once Reload has retired the generation. The frozen branch
// guarantees that a Start which captured the retired epoch still resolves
// the exact instances that generation served.
type generationResolver struct {
	registry *AgentRegistry
	result   *deploy.Result
	frozen   map[string]*agent.Agent
}

// Instance implements session.InstanceResolver.
func (r generationResolver) Instance(id string) (*agent.Agent, bool) {
	if r.frozen != nil {
		if instance, ok := r.frozen[id]; ok {
			return instance, true
		}
		return r.result.Agent(id)
	}
	if instance, ok := r.registry.Dynamic(id); ok {
		return instance, true
	}
	return r.result.Agent(id)
}

// freeze adopts the given dynamic instances into the generation so they
// retire (and later close) with it, and pins the resolver to them.
// Callers must hold lifecycleMu so no registration or reload can mutate
// the live registry concurrently.
func (g *Generation) freeze(dynamic map[string]*agent.Agent) {
	if g == nil {
		return
	}
	g.adopted = dynamic
	g.resolver = generationResolver{
		registry: g.registry,
		result:   g.result,
		frozen:   dynamic,
	}
}

// unfreeze restores the live resolver after an aborted swap, so the
// still-current generation keeps serving dynamic registrations.
func (g *Generation) unfreeze() {
	if g == nil {
		return
	}
	g.adopted = nil
	g.resolver = generationResolver{
		registry: g.registry,
		result:   g.result,
	}
}

// close releases the generation's result and adopted dynamic instances
// exactly once. Deployed agents are owned by the result; dynamic
// instances are owned by the generation that adopted them.
func (g *Generation) close() error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() {
		var errs []error
		if g.result != nil {
			if err := g.result.Close(); err != nil {
				errs = append(errs, fmt.Errorf(
					"runtime generation %d: close deployment: %w", g.id, err))
			}
		}
		if len(g.adopted) > 0 {
			names := make([]string, 0, len(g.adopted))
			for name := range g.adopted {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if err := g.adopted[name].Close(); err != nil {
					errs = append(errs, fmt.Errorf(
						"runtime generation %d: close dynamic agent %q: %w",
						g.id, name, err))
				}
			}
		}
		g.closeErr = errors.Join(errs...)
	})
	return g.closeErr
}
