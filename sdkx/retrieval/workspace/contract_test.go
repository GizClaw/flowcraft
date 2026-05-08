package workspace_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/contract"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	wsindex "github.com/GizClaw/flowcraft/sdkx/retrieval/workspace"
)

// TestContract runs the generic [contract.Run] suite against the
// workspace-backed Index. This is the same contract that
// sdk/retrieval/memory and sdk/retrieval/postgres satisfy, so a
// passing run here means the workspace backend is plug-compatible
// with every retrieval consumer in the codebase (recall, history,
// knowledge, pipelines).
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
