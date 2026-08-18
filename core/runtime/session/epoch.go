package session

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Deps is one Start's complete consistency snapshot: the resolver,
// host factory, and catalog provider a turn may use, tagged with the
// epoch they belong to. A turn bound to an epoch never mixes
// dependencies from another epoch, so a generation swap can never tear
// a single execution.
type Deps struct {
	Resolver        InstanceResolver
	HostFactory     HostFactory
	CatalogProvider CatalogProvider
	// Checkpoints and Resume are the epoch's session durability
	// settings: turns resolve them from their epoch, so a generation
	// reload can change the store or the resume flag without touching
	// in-flight turns.
	Checkpoints agent.CheckpointStore
	Resume      bool
	Epoch       uint64
}

// epochState tracks one epoch's dependencies and the references held
// by in-flight turns. A retired epoch stays alive until its refs drop
// to zero, then its onRetired hook (installed by SwapDeps) runs so the
// owner can release the underlying generation exactly once.
type epochState struct {
	deps      Deps
	refs      int
	retired   bool
	onRetired func(epoch uint64, deps Deps)
}

// beginEpoch atomically captures the current epoch and increments its
// reference count. The returned release function MUST be called exactly
// once per acquisition; it is safe to call again (a no-op), and it
// decrements the refcount for the captured epoch, not the current one.
// A missing current epoch is an internal invariant violation and is
// surfaced as an error rather than handed to the caller as empty deps.
func (m *Manager) beginEpoch() (Deps, func(), error) {
	m.mu.Lock()
	state := m.epochs[m.epochSeq]
	if state == nil {
		m.mu.Unlock()
		return Deps{}, func() {}, errdefs.Internalf(
			"runtime session: current epoch %d is missing", m.epochSeq)
	}
	state.refs++
	deps := state.deps
	m.mu.Unlock()
	return deps, func() { m.endEpoch(deps.Epoch) }, nil
}

// endEpoch releases one reference on epoch. When the epoch is retired
// and its refs reach zero it is removed from the manager and its
// onRetired hook runs (outside the manager lock).
func (m *Manager) endEpoch(epoch uint64) {
	m.mu.Lock()
	state := m.epochs[epoch]
	if state == nil || state.refs <= 0 {
		m.mu.Unlock()
		return
	}
	state.refs--
	if state.refs > 0 || !state.retired {
		m.mu.Unlock()
		return
	}
	delete(m.epochs, epoch)
	hook := state.onRetired
	deps := state.deps
	m.mu.Unlock()
	if hook != nil {
		hook(epoch, deps)
	}
}

// SwapDeps atomically retires the current epoch and installs deps as
// the new current epoch. onRetired, when non-nil, is invoked exactly
// once when the retired epoch's references reach zero (immediately, if
// none are outstanding). SwapDeps fails after the manager is closed.
func (m *Manager) SwapDeps(
	deps Deps,
	onRetired func(epoch uint64, d Deps),
) error {
	if isNil(deps.Resolver) || isNil(deps.HostFactory) {
		return errdefs.Validationf(
			"runtime session: swap deps require a resolver and a host factory")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	m.epochSeq++
	deps.Epoch = m.epochSeq
	m.epochs[deps.Epoch] = &epochState{deps: deps}

	var retireNow *epochState
	if prev := m.epochs[m.epochSeq-1]; prev != nil {
		prev.onRetired = onRetired
		prev.retired = true
		if prev.refs == 0 {
			delete(m.epochs, prev.deps.Epoch)
			retireNow = prev
		}
	}
	m.deps = deps
	m.mu.Unlock()

	if retireNow != nil && retireNow.onRetired != nil {
		retireNow.onRetired(retireNow.deps.Epoch, retireNow.deps)
	}
	return nil
}
