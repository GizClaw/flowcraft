package recall

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	linkstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/link"
	observationstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/observation"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
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

// ObservationStore is the canonical raw-evidence ledger boundary used by the
// experimental Observation/Assertion/Link graph.
type ObservationStore = port.ObservationStore

// ObservationListQuery filters scope-local ObservationStore.List results.
type ObservationListQuery = port.ObservationListQuery

// LinkStore is the canonical typed-edge ledger boundary used by the
// experimental Observation/Assertion/Link graph.
type LinkStore = port.LinkStore

// LinkListQuery filters scope-local LinkStore.List results.
type LinkListQuery = port.LinkListQuery

// ScopeListQuery filters scope enumeration for privileged/operator flows.
type ScopeListQuery = port.ScopeListQuery

// ScopeEnumerator is an optional extension for TemporalStore adapters that can
// enumerate canonical scope partitions. It is not part of TemporalStore so
// durable adapters can adopt it independently of the base ledger contract.
type ScopeEnumerator = port.ScopeEnumerator

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
//
// Memory.Close calls Close on the installed store. Callers that share a store
// between Memory instances should coordinate ownership externally.
func WithTemporalStore(s TemporalStore) Option {
	return func(c *config) {
		if s != nil {
			c.store = s
		}
	}
}

// WithObservationStore installs the canonical raw-evidence store used by the
// experimental graph ledger. The default is an in-memory store.
func WithObservationStore(s ObservationStore) Option {
	return func(c *config) {
		if s != nil {
			c.observationStore = s
		}
	}
}

// WithLinkStore installs the canonical typed-edge store used by the
// experimental graph ledger. The default is an in-memory store.
func WithLinkStore(s LinkStore) Option {
	return func(c *config) {
		if s != nil {
			c.linkStore = s
		}
	}
}

// NewInMemoryTemporalStore returns the process-local TemporalStore used by
// the default Memory stack. Facts are lost on process restart.
func NewInMemoryTemporalStore() TemporalStore {
	return temporalstore.NewMemoryStore()
}

// NewInMemoryObservationStore returns the process-local ObservationStore used
// by the default experimental graph stack.
func NewInMemoryObservationStore() ObservationStore {
	return observationstore.New()
}

// NewInMemoryLinkStore returns the process-local LinkStore used by the default
// experimental graph stack.
func NewInMemoryLinkStore() LinkStore {
	return linkstore.New()
}
