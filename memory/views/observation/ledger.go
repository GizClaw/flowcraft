package observation

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultLedgerID is the descriptor ID used by NewLedger unless overridden.
	DefaultLedgerID views.ID = "observation-ledger"

	// DefaultLedgerVersion is the descriptor version used by NewLedger unless overridden.
	DefaultLedgerVersion = "v1"
)

// Option configures a Ledger.
type Option interface {
	applyLedger(*Ledger)
}

type descriptorOption struct {
	id      views.ID
	version string
}

// WithID overrides the descriptor ID for Ledger.
func WithID(id views.ID) descriptorOption {
	return descriptorOption{id: id}
}

// WithVersion overrides the descriptor version for Ledger.
func WithVersion(version string) descriptorOption {
	return descriptorOption{version: version}
}

func (o descriptorOption) applyLedger(l *Ledger) {
	if o.id != "" {
		l.id = o.id
	}
	if o.version != "" {
		l.version = o.version
	}
}

// Ledger is a lightweight facade for the observation ledger view contract.
//
// It persists derived semantic observations. The records are rebuildable from
// canonical source evidence and must not be treated as canonical evidence.
type Ledger struct {
	store   Store
	id      views.ID
	version string
}

var _ views.View = (*Ledger)(nil)

// NewLedger creates an observation ledger view backed by store.
func NewLedger(store Store, opts ...Option) *Ledger {
	ledger := &Ledger{
		store:   store,
		id:      DefaultLedgerID,
		version: DefaultLedgerVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyLedger(ledger)
		}
	}
	return ledger
}

// Descriptor declares the Ledger view identity.
func (l *Ledger) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      l.id,
		Kind:    views.KindObservationLedger,
		Version: l.version,
	}
}

// Put stores or replaces an observation.
func (l *Ledger) Put(ctx context.Context, observation Observation) (Observation, error) {
	if l.store == nil {
		return Observation{}, errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateObservation(observation); err != nil {
		return Observation{}, err
	}
	stored, err := l.store.Put(ctx, cloneObservation(observation))
	if err != nil {
		return Observation{}, err
	}
	return cloneObservation(stored), nil
}

// Get returns one observation by id.
func (l *Ledger) Get(ctx context.Context, id string) (Observation, bool, error) {
	if l.store == nil {
		return Observation{}, false, errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateObservationID(id); err != nil {
		return Observation{}, false, err
	}
	observation, ok, err := l.store.Get(ctx, id)
	if err != nil {
		return Observation{}, false, err
	}
	if !ok {
		return Observation{}, false, nil
	}
	return cloneObservation(observation), true, nil
}

// List returns observations matching opts.
func (l *Ledger) List(ctx context.Context, opts ListOptions) ([]Observation, error) {
	if l.store == nil {
		return nil, errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateListOptions(opts); err != nil {
		return nil, err
	}
	observations, err := l.store.List(ctx, cloneListOptions(opts))
	if err != nil {
		return nil, err
	}
	return cloneObservations(observations), nil
}

// Delete removes one observation by id. It is idempotent at the Store boundary.
func (l *Ledger) Delete(ctx context.Context, id string) error {
	if l.store == nil {
		return errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateObservationID(id); err != nil {
		return err
	}
	return l.store.Delete(ctx, id)
}

// DeleteScope removes all observations for one scope. It is idempotent at the Store boundary.
func (l *Ledger) DeleteScope(ctx context.Context, scope Scope) error {
	if l.store == nil {
		return errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateScope(scope); err != nil {
		return err
	}
	return l.store.DeleteScope(ctx, scope)
}
