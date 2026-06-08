package retrieval_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

type declaredOnlyIndex struct{}

func (declaredOnlyIndex) Capabilities() retrieval.Capabilities {
	return retrieval.Capabilities{
		NativeDeleteByFilter: true,
		Extensions: retrieval.ExtensionCapabilities{
			DocGetter:      true,
			Filterable:     true,
			Vectorizable:   true,
			Snapshottable:  true,
			Iterable:       true,
			Count:          true,
			DeleteByFilter: true,
			DropNamespace:  true,
		},
	}
}
func (declaredOnlyIndex) Close() error { return nil }
func (declaredOnlyIndex) Upsert(context.Context, string, []retrieval.Doc) error {
	return nil
}
func (declaredOnlyIndex) Delete(context.Context, string, []string) error { return nil }
func (declaredOnlyIndex) Search(context.Context, string, retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return &retrieval.SearchResponse{}, nil
}
func (declaredOnlyIndex) List(context.Context, string, retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return &retrieval.ListResponse{}, nil
}

func TestCapabilitiesOfProjectsOptionalInterfaces(t *testing.T) {
	idx := memidx.New()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.Extensions.DocGetter || !caps.Extensions.Iterable || !caps.Extensions.DeleteByFilter || !caps.Extensions.DropNamespace {
		t.Fatalf("memory extensions not projected: %+v", caps.Extensions)
	}
	if !retrieval.Supports(idx, retrieval.CapabilityDocGetter) {
		t.Fatal("memory index should support DocGetter")
	}
	if !retrieval.Supports(idx, retrieval.CapabilityHybrid) {
		t.Fatal("memory index should report Search hybrid support")
	}
}

func TestCapabilitiesOfIgnoresDeclaredExtensionsWithoutMethods(t *testing.T) {
	idx := declaredOnlyIndex{}
	caps := retrieval.CapabilitiesOf(idx)
	if caps.Extensions != (retrieval.ExtensionCapabilities{}) {
		t.Fatalf("declared-only extensions should be cleared: %+v", caps.Extensions)
	}
	if caps.NativeDeleteByFilter {
		t.Fatalf("NativeDeleteByFilter should be cleared without DeleteByFilter method")
	}
	for _, cap := range []retrieval.Capability{
		retrieval.CapabilityDocGetter,
		retrieval.CapabilityIterable,
		retrieval.CapabilityCount,
		retrieval.CapabilityDeleteByFilter,
		retrieval.CapabilityDropNamespace,
		retrieval.CapabilitySnapshot,
		retrieval.CapabilityVectorizable,
	} {
		if retrieval.Supports(idx, cap) {
			t.Fatalf("Supports(%q) = true for declared-only extension", cap)
		}
	}
	if _, ok := retrieval.AsDocGetter(idx); ok {
		t.Fatal("AsDocGetter should reject declared-only extension")
	}
	if _, ok := retrieval.AsIterable(idx); ok {
		t.Fatal("AsIterable should reject declared-only extension")
	}
	if _, ok := retrieval.AsCountable(idx); ok {
		t.Fatal("AsCountable should reject declared-only extension")
	}
	if _, ok := retrieval.AsDeletableByFilter(idx); ok {
		t.Fatal("AsDeletableByFilter should reject declared-only extension")
	}
	if _, ok := retrieval.AsDroppable(idx); ok {
		t.Fatal("AsDroppable should reject declared-only extension")
	}
}
