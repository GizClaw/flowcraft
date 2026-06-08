package journal_test

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/journal"
	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
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

type namespaceWarmIndex struct {
	hybridClaimingIndex
	warmed string
}

func (idx *namespaceWarmIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{}
}

func (idx *namespaceWarmIndex) WarmNamespace(_ context.Context, namespace string) error {
	idx.warmed = namespace
	return nil
}

type namespaceWarmCountIndex struct {
	hybridClaimingIndex
	warmed string
}

func (idx *namespaceWarmCountIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{Extensions: retrieval.ExtensionCapabilities{
		Count:         true,
		NamespaceWarm: true,
	}}
}

func (idx *namespaceWarmCountIndex) Count(context.Context, string, retrieval.Filter) (int64, error) {
	return 0, nil
}

func (idx *namespaceWarmCountIndex) WarmNamespace(_ context.Context, namespace string) error {
	idx.warmed = namespace
	return nil
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
	if retrieval.Supports(wrapped, retrieval.CapabilityHybrid) {
		t.Fatalf("#157 regression: Supports must project Hybrid through the wrapper method set")
	}
	if got := retrieval.CapabilitiesOf(wrapped); got.Hybrid || got.Extensions.HybridSearch {
		t.Fatalf("#157 regression: CapabilitiesOf(wrapped) = %+v", got)
	}
	if _, ok := wrapped.(retrieval.DocGetter); ok {
		t.Fatalf("#157 regression: non-DocGetter inner must not satisfy DocGetter")
	}
	if _, ok := wrapped.(retrieval.Filterable); ok {
		t.Fatalf("#157 regression: non-Filterable inner must not satisfy Filterable")
	}
	if _, ok := wrapped.(retrieval.DeletableByFilter); ok {
		t.Fatalf("#157 regression: non-DeletableByFilter inner must not satisfy DeletableByFilter")
	}
	if _, ok := wrapped.(retrieval.Droppable); ok {
		t.Fatalf("#157 regression: non-Droppable inner must not satisfy Droppable")
	}
	if _, ok := wrapped.(retrieval.Iterable); ok {
		t.Fatalf("#157 regression: non-Iterable inner must not satisfy Iterable")
	}
	if _, ok := wrapped.(retrieval.Countable); ok {
		t.Fatalf("#157 regression: non-Countable inner must not satisfy Countable")
	}
	if _, ok := wrapped.(retrieval.NamespaceWarmer); ok {
		t.Fatalf("#157 regression: non-NamespaceWarmer inner must not satisfy NamespaceWarmer")
	}
}

func TestWrap_ProjectsImplementedOptionalInterfaces(t *testing.T) {
	wrapped := journal.Wrap(memidx.New(), nullJournal{})
	if _, ok := wrapped.(retrieval.DocGetter); !ok {
		t.Fatal("wrapped memory index should expose DocGetter")
	}
	if _, ok := wrapped.(retrieval.DeletableByFilter); !ok {
		t.Fatal("wrapped memory index should expose DeletableByFilter")
	}
	if _, ok := wrapped.(retrieval.Droppable); !ok {
		t.Fatal("wrapped memory index should expose Droppable")
	}
	if _, ok := wrapped.(retrieval.Iterable); !ok {
		t.Fatal("wrapped memory index should expose Iterable")
	}
	if _, ok := wrapped.(retrieval.Countable); !ok {
		t.Fatal("wrapped memory index should expose Countable")
	}
	if _, ok := wrapped.(retrieval.Filterable); ok {
		t.Fatal("wrapped memory index should not expose Filterable")
	}
	caps := retrieval.CapabilitiesOf(wrapped)
	if !caps.Extensions.DocGetter || !caps.Extensions.DeleteByFilter || !caps.Extensions.DropNamespace || !caps.Extensions.Iterable || !caps.Extensions.Count {
		t.Fatalf("CapabilitiesOf did not project memory extensions: %+v", caps.Extensions)
	}
	if caps.Extensions.Filterable {
		t.Fatalf("CapabilitiesOf falsely projected Filterable: %+v", caps.Extensions)
	}
}

func TestWrap_ProjectsNamespaceWarmer(t *testing.T) {
	inner := &namespaceWarmIndex{}
	wrapped := journal.Wrap(inner, nullJournal{})
	warmer, ok := retrieval.AsNamespaceWarmer(wrapped)
	if !ok {
		t.Fatal("wrapped namespace warmer should expose NamespaceWarmer")
	}
	if err := warmer.WarmNamespace(context.Background(), "warm/ns"); err != nil {
		t.Fatal(err)
	}
	if inner.warmed != "warm/ns" {
		t.Fatalf("inner warmed namespace = %q, want warm/ns", inner.warmed)
	}
	if _, ok := wrapped.(retrieval.DocGetter); ok {
		t.Fatal("warm-only wrapper should not expose unrelated DocGetter")
	}
}

func TestWrap_WarmFallbackDoesNotAdvertiseDroppedCount(t *testing.T) {
	inner := &namespaceWarmCountIndex{}
	wrapped := journal.Wrap(inner, nullJournal{})

	if _, ok := wrapped.(retrieval.NamespaceWarmer); !ok {
		t.Fatal("wrapped warm+count fallback should expose NamespaceWarmer")
	}
	if _, ok := wrapped.(retrieval.Countable); ok {
		t.Fatal("wrapped warm+count fallback should not expose Countable")
	}
	if retrieval.Supports(wrapped, retrieval.CapabilityCount) {
		t.Fatal("wrapped warm+count fallback should not advertise Count capability")
	}
	caps := retrieval.CapabilitiesOf(wrapped)
	if caps.Extensions.Count {
		t.Fatalf("CapabilitiesOf falsely projected Count: %+v", caps.Extensions)
	}
	if !caps.Extensions.NamespaceWarm {
		t.Fatalf("CapabilitiesOf did not project NamespaceWarm: %+v", caps.Extensions)
	}
	if !retrieval.Supports(wrapped, retrieval.CapabilityNamespaceWarm) {
		t.Fatal("wrapped warm+count fallback should advertise NamespaceWarm capability")
	}
}
