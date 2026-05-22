package retrieval_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

func TestCapabilitiesOfProjectsOptionalInterfaces(t *testing.T) {
	idx := memidx.New()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.Extensions.DocGetter || !caps.Extensions.Iterable || !caps.Extensions.DeleteByFilter || !caps.Extensions.DropNamespace {
		t.Fatalf("memory extensions not projected: %+v", caps.Extensions)
	}
	if !retrieval.Supports(idx, retrieval.CapabilityDocGetter) {
		t.Fatal("memory index should support DocGetter")
	}
	if retrieval.Supports(idx, retrieval.CapabilityHybrid) {
		t.Fatal("memory index should not report native Hybridable support")
	}
}
