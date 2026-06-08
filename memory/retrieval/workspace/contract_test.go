package workspace_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/contract"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// TestContract runs the generic [contract.Run] suite against the
// workspace-backed Index. This is the same contract that
// sdk/retrieval/memory and sdk/retrieval/postgres satisfy, so a
// passing run here means the workspace backend is plug-compatible
// with retrieval consumers in the codebase.
//
// Each subtest gets a fresh MemWorkspace via the factory below; no
// per-subtest state leaks. AutoCompact is disabled to keep the
// segment layout deterministic across the suite.
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) (retrieval.Index, func()) {
		t.Helper()
		ws := sdkworkspace.NewMemWorkspace()
		idx, err := wsindex.New(ws,
			wsindex.WithAutoCompact(false),
			// Tight thresholds so the suite exercises actual flushes
			// rather than running entirely out of the memtable.
			wsindex.WithMemtableMaxDocs(2),
			// Bump the heartbeat above the suite's runtime so
			// no namespace ever times out on us.
			wsindex.WithLockHeartbeat(10*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		return idx, func() { _ = idx.Close() }
	})
}

func TestCapabilitiesDeleteByFilterIsFallback(t *testing.T) {
	idx, err := wsindex.New(sdkworkspace.NewMemWorkspace(), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	caps := retrieval.CapabilitiesOf(idx)
	if !caps.Extensions.DeleteByFilter {
		t.Fatalf("workspace should expose callable DeleteByFilter: %+v", caps.Extensions)
	}
	if caps.NativeDeleteByFilter {
		t.Fatalf("workspace DeleteByFilter scans and tombstones; NativeDeleteByFilter must be false: %+v", caps)
	}
}

func TestCapabilitiesExposeManagementInterfaces(t *testing.T) {
	idx, err := wsindex.New(sdkworkspace.NewMemWorkspace(), wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	caps := retrieval.CapabilitiesOf(idx)
	if !caps.FilterPushdown || caps.NativeDeleteByFilter {
		t.Fatalf("workspace capability flags mismatch: %+v", caps)
	}
	if !caps.Extensions.DocGetter || !caps.Extensions.Filterable || !caps.Extensions.Count ||
		!caps.Extensions.DeleteByFilter || !caps.Extensions.Iterable || !caps.Extensions.DropNamespace {
		t.Fatalf("workspace should expose management/read extensions: %+v", caps.Extensions)
	}
	if _, ok := retrieval.AsDocGetter(idx); !ok {
		t.Fatal("AsDocGetter should succeed for workspace Index")
	}
	if _, ok := any(idx).(retrieval.Filterable); !ok || !retrieval.Supports(idx, retrieval.CapabilityFilterPushdown) {
		t.Fatal("workspace Index should implement and advertise retrieval.Filterable")
	}
	if _, ok := retrieval.AsCountable(idx); !ok {
		t.Fatal("AsCountable should succeed for workspace Index")
	}
	if _, ok := retrieval.AsDeletableByFilter(idx); !ok {
		t.Fatal("AsDeletableByFilter should succeed for workspace Index")
	}
	if _, ok := retrieval.AsIterable(idx); !ok {
		t.Fatal("AsIterable should succeed for workspace Index")
	}
	if _, ok := retrieval.AsDroppable(idx); !ok {
		t.Fatal("AsDroppable should succeed for workspace Index")
	}
}
