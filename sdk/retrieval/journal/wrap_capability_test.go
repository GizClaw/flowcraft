package journal_test

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
)

// hybridClaimingIndex is a minimal retrieval.Index that advertises
// Capabilities.Hybrid == true but DOES NOT implement
// retrieval.Hybridable — exactly the in-tree workspace.Index profile
// that triggered #157.
type hybridClaimingIndex struct{}

func (hybridClaimingIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{Hybrid: true}
}
func (hybridClaimingIndex) Close() error { return nil }
func (hybridClaimingIndex) Upsert(context.Context, string, []retrieval.Doc) error {
	return nil
}
func (hybridClaimingIndex) Delete(context.Context, string, []string) error { return nil }
func (hybridClaimingIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}
func (hybridClaimingIndex) List(context.Context, string, retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return &retrieval.ListResponse{}, nil
}

// nullJournal absorbs Record / Close calls so Wrap can return a
// useful value without spinning up a real journal backend.
type nullJournal struct{}

func (nullJournal) Record(context.Context, journal.Event) error { return nil }
func (nullJournal) History(context.Context, string, string) ([]journal.Event, error) {
	return nil, nil
}
func (nullJournal) Replay(context.Context, string, uint64) iter.Seq2[journal.Event, error] {
	return func(func(journal.Event, error) bool) {}
}
func (nullJournal) Compact(context.Context, time.Time) error { return nil }
func (nullJournal) Close() error                             { return nil }

// TestWrap_DoesNotFalselyAdvertiseHybridable is the regression guard
// for issue #157. Pre-fix, journal.Wrap embedded a SearchHybrid
// bridge on every variant via *journaledIndex, so the type
// assertion to retrieval.Hybridable always succeeded — and the
// bridge returned (nil, nil), which downstream
// pipeline.HybridShortCircuit treated as a successful empty
// short-circuit.
//
// The fix removes the bridge methods from the base wrapper type so
// no wrapped value falsely advertises Hybridable / Snapshottable /
// Vectorizable when the inner index does not implement them.
func TestWrap_DoesNotFalselyAdvertiseHybridable(t *testing.T) {
	wrapped := journal.Wrap(hybridClaimingIndex{}, nullJournal{})
	if _, ok := wrapped.(retrieval.Hybridable); ok {
		t.Fatalf("#157 regression: journal.Wrap of a non-Hybridable inner must NOT satisfy retrieval.Hybridable")
	}
	if _, ok := wrapped.(retrieval.Snapshottable); ok {
		t.Fatalf("#157 regression: journal.Wrap of a non-Snapshottable inner must NOT satisfy retrieval.Snapshottable")
	}
	if _, ok := wrapped.(retrieval.Vectorizable); ok {
		t.Fatalf("#157 regression: journal.Wrap of a non-Vectorizable inner must NOT satisfy retrieval.Vectorizable")
	}
}
