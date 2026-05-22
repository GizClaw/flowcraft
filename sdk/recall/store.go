package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// ListQuery filters scope-local TemporalStore.List results. Empty fields
// mean "match anything"; see TemporalStore.List for ordering guarantees.
type ListQuery = port.ListQuery

// TemporalStore is the canonical TemporalFact ledger boundary.
//
// Durable adapters implement this interface to persist recall's truth
// layer. Retrieval indexes are derived projections; they must not be
// treated as the canonical store.
type TemporalStore = port.TemporalStore

// ErrStoreNotFound is the portable missing-row sentinel for recall stores.
// TemporalStore and EvidenceStore adapters should return or wrap this value
// so callers can use errors.Is.
var ErrStoreNotFound = port.ErrNotFound

// ErrTemporalReopenConflict is returned by TemporalStore.ReopenValidity when
// the fact's current CorrectedBy does not match the expected value.
var ErrTemporalReopenConflict = temporalstore.ErrReopenConflict

// ErrTemporalValidityAlreadyClosed is returned by TemporalStore.UpdateValidity
// when the target fact is already closed with a different tuple.
var ErrTemporalValidityAlreadyClosed = temporalstore.ErrValidityAlreadyClosed

// WithTemporalStore installs the canonical TemporalFact store.
//
// The default is an in-memory store suitable for tests and local development.
// Production deployments should provide a durable adapter.
func WithTemporalStore(s TemporalStore) Option {
	return func(c *config) {
		if s != nil {
			c.store = s
		}
	}
}

// NewInMemoryTemporalStore returns the process-local TemporalStore used by
// the default Memory stack. Facts are lost on process restart.
func NewInMemoryTemporalStore() TemporalStore {
	return temporalstore.NewMemoryStore()
}
