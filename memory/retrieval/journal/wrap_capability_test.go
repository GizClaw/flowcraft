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
// Capabilities.Hybrid == true through ordinary Search support.
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

type deleteOnlyIndex struct{ hybridClaimingIndex }

func (deleteOnlyIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{
		NativeDeleteByFilter: true,
		Extensions: retrieval.ExtensionCapabilities{
			DeleteByFilter: true,
		},
	}
}
func (deleteOnlyIndex) DeleteByFilter(context.Context, string, retrieval.Filter) (int64, error) {
	return 0, nil
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

// TestWrap_DoesNotFalselyAdvertiseOptionalInterfaces is the regression guard
// for issue #157. The wrapper must expose only optional method-set interfaces
// that the wrapped index actually implements.
func TestWrap_DoesNotFalselyAdvertiseOptionalInterfaces(t *testing.T) {
	wrapped := journal.Wrap(hybridClaimingIndex{}, nullJournal{})
	if _, ok := wrapped.(retrieval.Snapshottable); ok {
		t.Fatalf("#157 regression: journal.Wrap of a non-Snapshottable inner must NOT satisfy retrieval.Snapshottable")
	}
	if _, ok := wrapped.(retrieval.Vectorizable); ok {
		t.Fatalf("#157 regression: journal.Wrap of a non-Vectorizable inner must NOT satisfy retrieval.Vectorizable")
	}
	if !retrieval.Supports(wrapped, retrieval.CapabilityHybrid) {
		t.Fatalf("Capabilities.Hybrid should describe ordinary Search hybrid support")
	}
	if got := retrieval.CapabilitiesOf(wrapped); !got.Hybrid {
		t.Fatalf("CapabilitiesOf(wrapped) should preserve Hybrid: %+v", got)
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
	if caps.WriteIsAtomic {
		t.Fatalf("journal wrapper should not advertise atomic writes: %+v", caps)
	}
	if caps.NativeDeleteByFilter {
		t.Fatalf("journal wrapper should not advertise native DeleteByFilter: %+v", caps)
	}
}

func TestWrap_PartialExtensionCapabilitiesMatchWrapperMethodSet(t *testing.T) {
	wrapped := journal.Wrap(deleteOnlyIndex{}, nullJournal{})
	caps := retrieval.CapabilitiesOf(wrapped)
	if caps.Extensions.DeleteByFilter {
		t.Fatalf("wrapper must not advertise DeleteByFilter when its method set does not expose it: %+v", caps.Extensions)
	}
	if caps.NativeDeleteByFilter {
		t.Fatalf("wrapper must clear NativeDeleteByFilter for journaled fallback deletes: %+v", caps)
	}
	if _, ok := wrapped.(retrieval.DeletableByFilter); ok {
		t.Fatal("wrapped partial-extension backend should not expose DeletableByFilter")
	}
	if _, ok := retrieval.AsDeletableByFilter(wrapped); ok {
		t.Fatal("AsDeletableByFilter should match the wrapper method set")
	}
	assertExtensionProjectionMatchesMethodSet(t, wrapped)
}

func TestWrap_CapabilityProjectionMatchesMethodSet(t *testing.T) {
	assertExtensionProjectionMatchesMethodSet(t, journal.Wrap(memidx.New(), nullJournal{}))
}

func assertExtensionProjectionMatchesMethodSet(t *testing.T, idx retrieval.Index) {
	t.Helper()
	caps := retrieval.CapabilitiesOf(idx)
	if got, want := caps.Extensions.DocGetter, implementsDocGetter(idx); got != want {
		t.Fatalf("DocGetter projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.Filterable, implementsFilterable(idx); got != want {
		t.Fatalf("Filterable projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.Vectorizable, implementsVectorizable(idx); got != want {
		t.Fatalf("Vectorizable projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.Snapshottable, implementsSnapshottable(idx); got != want {
		t.Fatalf("Snapshottable projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.Iterable, implementsIterable(idx); got != want {
		t.Fatalf("Iterable projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.Count, implementsCountable(idx); got != want {
		t.Fatalf("Count projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.DeleteByFilter, implementsDeletableByFilter(idx); got != want {
		t.Fatalf("DeleteByFilter projection=%v methodSet=%v", got, want)
	}
	if got, want := caps.Extensions.DropNamespace, implementsDroppable(idx); got != want {
		t.Fatalf("DropNamespace projection=%v methodSet=%v", got, want)
	}
}

func implementsDocGetter(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.DocGetter)
	return ok
}
func implementsFilterable(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.Filterable)
	return ok
}
func implementsVectorizable(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.Vectorizable)
	return ok
}
func implementsSnapshottable(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.Snapshottable)
	return ok
}
func implementsIterable(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.Iterable)
	return ok
}
func implementsCountable(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.Countable)
	return ok
}
func implementsDeletableByFilter(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.DeletableByFilter)
	return ok
}
func implementsDroppable(idx retrieval.Index) bool {
	_, ok := idx.(retrieval.Droppable)
	return ok
}
