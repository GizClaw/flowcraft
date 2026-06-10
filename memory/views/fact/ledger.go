package fact

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultLedgerID is the descriptor ID used by NewLedger unless overridden.
	DefaultLedgerID views.ID = "fact-ledger"

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
func WithID(id views.ID) Option {
	return descriptorOption{id: id}
}

// WithVersion overrides the descriptor version for Ledger.
func WithVersion(version string) Option {
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

// Ledger is a lightweight facade for the fact ledger view contract.
//
// It persists reconciled semantic facts derived from observation ledger outputs.
// The records are rebuildable from their observation and canonical evidence
// lineage and must not be treated as canonical evidence.
type Ledger struct {
	store   Store
	id      views.ID
	version string
}

var _ views.View = (*Ledger)(nil)

// NewLedger creates a fact ledger view backed by store.
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
		Kind:    views.KindFactLedger,
		Version: l.version,
	}
}

// Put stores or replaces a fact. Empty status is normalized to active.
func (l *Ledger) Put(ctx context.Context, fact Fact) (Fact, error) {
	if l.store == nil {
		return Fact{}, errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	fact = normalizeFact(cloneFact(fact))
	if err := validateFact(fact); err != nil {
		return Fact{}, err
	}
	stored, err := l.store.Put(ctx, fact)
	if err != nil {
		return Fact{}, err
	}
	return cloneFact(stored), nil
}

// Get returns one fact by id.
func (l *Ledger) Get(ctx context.Context, id FactID) (Fact, bool, error) {
	if l.store == nil {
		return Fact{}, false, errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateFactID(id); err != nil {
		return Fact{}, false, err
	}
	fact, ok, err := l.store.Get(ctx, id)
	if err != nil {
		return Fact{}, false, err
	}
	if !ok {
		return Fact{}, false, nil
	}
	return cloneFact(fact), true, nil
}

// List returns facts matching opts.
func (l *Ledger) List(ctx context.Context, opts ListOptions) ([]Fact, error) {
	if l.store == nil {
		return nil, errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateListOptions(opts); err != nil {
		return nil, err
	}
	facts, err := l.store.List(ctx, cloneListOptions(opts))
	if err != nil {
		return nil, err
	}
	return cloneFacts(facts), nil
}

// Delete removes one fact by id. It is idempotent at the Store boundary.
func (l *Ledger) Delete(ctx context.Context, id FactID) error {
	if l.store == nil {
		return errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateFactID(id); err != nil {
		return err
	}
	return l.store.Delete(ctx, id)
}

// DeleteSubject removes all facts for one subject. It is idempotent at the Store boundary.
func (l *Ledger) DeleteSubject(ctx context.Context, subject string) error {
	if l.store == nil {
		return errdefs.Validationf("%s: store is required", ledgerErrPrefix)
	}
	if err := validateSubject(subject); err != nil {
		return err
	}
	return l.store.DeleteSubject(ctx, subject)
}
